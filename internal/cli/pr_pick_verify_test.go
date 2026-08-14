package cli

// Test plan for launchPicked's verification, ordering, and accounting
// (pr_pick.go's post-dispatch half — plan step 6).
//
//   [x] Combined: admit three of four, defer one, fail one launch after
//       preparation, dispatch two, make one exact dispatch gone. One wait and
//       one verification list; per-ref lines, the deferral summary, and the
//       cleanup summary all land before the returned verification error; the
//       completion log carries verify=gone, gone=1 alongside the old counts.
//   [x] All dispatched windows survive → verify=live, gone=0, nil error.
//   [x] Verification list error → verify=unknown, gone=0, non-zero error, and
//       NO ref is labeled gone.
//   [x] Canceled wait → same unknown shape, and no list is issued at all.
//   [x] --no-verify → no wait, no verification list, verify=skipped, and the
//       dispatch that would have been reported gone is not.
//
// Every row here runs with noVerify=false unless it is the --no-verify row
// itself: the point is to drive the real pr.Client.VerifyDispatched path, which
// needs a tmux double whose list-windows answer changes once windows exist.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/pr"
	"github.com/cameronsjo/forgectl/internal/tmux"
)

// ledgerWindow is one window the fake tmux server is currently holding.
type ledgerWindow struct {
	id   string
	name string
}

// tmuxLedger models the slice of a tmux server that dispatch verification
// depends on: `new-window -P -F` mints a generation-qualified identity and the
// server remembers the window, and a later `list-windows` replays what is still
// there in the field order tmux.IdentityFormat produces.
//
// A stateless double cannot express this. It answers list-windows identically
// before and after a dispatch, so either admission sees phantom live reviews or
// verification sees an empty server and calls every healthy review gone — which
// is why the bulk tests had been running with verification switched off.
type tmuxLedger struct {
	mu      sync.Mutex
	session string
	pid     string
	start   string
	nextID  int
	live    []ledgerWindow

	// diesOnDispatch names windows whose new-window succeeds — tmux exits 0 the
	// instant the window exists — but whose child dies immediately, so the
	// window is gone by the time verification lists. This is forgectl#242's
	// exact failure, and the only way to reach the gone branch honestly.
	diesOnDispatch map[string]bool
	// dieAll is diesOnDispatch for every window, for callers that would
	// otherwise have to spell a derived window name (a local review's, say).
	dieAll bool
	// launchFails names windows whose new-window call itself errors, leaving a
	// prepared clean room behind.
	launchFails map[string]bool
	// listErr, when set, is returned once list-windows has been called more than
	// listErrAfter times. Bulk lists at admission and again at verification, so
	// listErrAfter=1 fails exactly the verification sweep; a single review never
	// reaches admission, so 0 fails its only list.
	listErr      error
	listErrAfter int

	listCalls   int
	windowCalls int
}

func newTmuxLedger(session string) *tmuxLedger {
	return &tmuxLedger{
		session:        session,
		pid:            "4242",
		start:          "1000",
		diesOnDispatch: map[string]bool{},
		launchFails:    map[string]bool{},
	}
}

// runner wires the ledger into a FakeRunner that also answers the `gh pr view`
// call a remote Prepare makes.
func (l *tmuxLedger) runner() *exec.FakeRunner {
	return l.runnerWith(func(name string, args []string) (string, error) {
		if name == "gh" && len(args) >= 2 && args[0] == "pr" && args[1] == "view" {
			return `{"headRefName":"feature","headRefOid":"abc123",` +
				`"headRepositoryOwner":{"login":"cameronsjo"},"headRepository":{"name":"forgectl"}}`, nil
		}
		return "", nil
	})
}

// runnerWith answers every tmux call from the ledger and hands everything else
// to fallback — the seam a local review needs, since PrepareLocal wants real
// `git rev-parse` answers rather than a PR head.
//
// RunFunc can be called concurrently (PrepareMany fans out), so every ledger
// read and write below is under the mutex.
func (l *tmuxLedger) runnerWith(fallback func(string, []string) (string, error)) *exec.FakeRunner {
	return &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		if name != "tmux" || len(args) == 0 {
			return fallback(name, args)
		}
		switch args[0] {
		case "-V":
			return "tmux 3.7b", nil
		case "display-message":
			return l.identity("@0"), nil
		case "list-sessions":
			// The review session already exists, so EnsureSession resolves it
			// and creates nothing — the state the old has-session probe
			// reported by exiting 0.
			return l.listSessions(), nil
		case "list-windows":
			return l.listWindows()
		case "new-window":
			return l.newWindow(args)
		}
		return "", nil
	}}
}

func (l *tmuxLedger) identity(windowID string) string {
	return strings.Join([]string{l.pid, l.start, windowID}, tmux.FieldSep)
}

// reviewSessionID is the native id every window in this ledger hangs off. It is
// fixed because the ledger models one server holding one review session.
const reviewSessionID = "$1"

// listSessions renders the review session in sessionFormat's field order:
// identity (pid, start, session id), name, windows, attached, created, path.
func (l *tmuxLedger) listSessions() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join([]string{
		l.pid, l.start, reviewSessionID, l.session,
		fmt.Sprint(len(l.live)), "0", "1700000000", "/w",
	}, tmux.FieldSep)
}

func (l *tmuxLedger) listWindows() (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.listCalls++
	if l.listErr != nil && l.listCalls > l.listErrAfter {
		return "", l.listErr
	}
	rows := make([]string, 0, len(l.live))
	for i, w := range l.live {
		// Field order is windowFormat's: identity (pid, start, window id),
		// parent session id, session name, index, name, active, panes.
		rows = append(rows, strings.Join([]string{
			l.pid, l.start, w.id, reviewSessionID, l.session, fmt.Sprint(i), w.name, "0", "1",
		}, tmux.FieldSep))
	}
	return strings.Join(rows, "\n"), nil
}

func (l *tmuxLedger) newWindow(args []string) (string, error) {
	name := argValue(args, "-n")
	l.mu.Lock()
	defer l.mu.Unlock()
	l.windowCalls++
	if l.launchFails[name] {
		return "", fmt.Errorf("boom: new-window refused %s", name)
	}
	l.nextID++
	id := fmt.Sprintf("@%d", l.nextID)
	if !l.dieAll && !l.diesOnDispatch[name] {
		l.live = append(l.live, ledgerWindow{id: id, name: name})
	}
	return l.identity(id), nil
}

func (l *tmuxLedger) counts() (listCalls, windowCalls int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.listCalls, l.windowCalls
}

func argValue(args []string, flag string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag {
			return args[i+1]
		}
	}
	return ""
}

// captureCompletionLog redirects the default slog handler for the duration of
// the test and returns a reader for launchPicked's completion record — the
// surface carrying the typed verify state and gone count.
func captureCompletionLog(t *testing.T) func(t *testing.T) map[string]any {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return func(t *testing.T) map[string]any {
		t.Helper()
		for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
			var record map[string]any
			if err := json.Unmarshal([]byte(line), &record); err != nil {
				continue
			}
			if record["msg"] == "Successfully completed bulk launch." {
				return record
			}
		}
		t.Fatalf("no bulk-launch completion record in %q", buf.String())
		return nil
	}
}

// verifyingClient builds a pr.Client over the ledger with a zero-cost dispatch
// waiter, so the real VerifyDispatched path runs without the production
// eight-second delay.
func verifyingClient(t *testing.T, l *tmuxLedger, fake *exec.FakeRunner) *pr.Client {
	t.Helper()
	return pr.New(fake,
		pr.WithSessionsDir(t.TempDir()),
		pr.WithTmuxSession(l.session),
		pr.WithDispatchWait(func(context.Context) error { return nil }),
	)
}

func pickRefs(numbers ...int) []pr.PR {
	updated := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	out := make([]pr.PR, 0, len(numbers))
	for _, n := range numbers {
		out = append(out, pr.PR{Ref: pr.Ref{Owner: "cameronsjo", Repo: "forgectl", Number: n}, UpdatedAt: updated})
	}
	return out
}

// pickWindowName is the review-window name for the Nth ref pickRefs produces.
// Window names are derived from a session's logical key (forgectl#218), so a
// test cannot spell one — it asks for it, exactly as production does.
func pickWindowName(t *testing.T, number int) string {
	t.Helper()
	name, err := pr.ReviewWindowName(pr.Ref{Owner: "cameronsjo", Repo: "forgectl", Number: number})
	if err != nil {
		t.Fatalf("ReviewWindowName(#%d): %v", number, err)
	}
	return name
}

func emptyStore(t *testing.T) *pr.ReviewedStore {
	t.Helper()
	return pr.LoadReviewed(filepath.Join(t.TempDir(), "reviewed.json"))
}

func TestLaunchPicked_BulkOrderingAndAccounting(t *testing.T) {
	fakeClaudeBin(t)
	readLog := captureCompletionLog(t)

	ledger := newTmuxLedger("forgectl")
	// #2's window never survives its own dispatch; #3's new-window refuses
	// outright, leaving a prepared clean room behind.
	ledger.diesOnDispatch[pickWindowName(t, 2)] = true
	ledger.launchFails[pickWindowName(t, 3)] = true
	fake := ledger.runner()
	client := verifyingClient(t, ledger, fake)

	cmd, out, errOut := newTestCmd()
	cfg := config.Config{Pr: config.PrConfig{MaxConcurrent: 3}}
	err := launchPicked(context.Background(), client, cfg, cmd, pickRefs(1, 2, 3, 4), emptyStore(t), false)
	if err == nil || !strings.Contains(err.Error(), "review window disappeared during startup") {
		t.Fatalf("error = %v, want the gone aggregate", err)
	}
	if got, want := err.Error(), "cameronsjo/forgectl#2"; !strings.Contains(got, want) {
		t.Errorf("error = %q, want it to name %s", got, want)
	}
	for _, absent := range []string{"cameronsjo/forgectl#1", "cameronsjo/forgectl#3", "cameronsjo/forgectl#4"} {
		if strings.Contains(err.Error(), absent) {
			t.Errorf("error = %q, must not name %s", err.Error(), absent)
		}
	}

	// One wait, one verification list. Admission lists once before any dispatch,
	// so two list calls total for the whole invocation.
	listCalls, windowCalls := ledger.counts()
	if listCalls != 2 {
		t.Errorf("list-windows calls = %d, want 2 (admission + one verification sweep)", listCalls)
	}
	if windowCalls != 3 {
		t.Errorf("new-window calls = %d, want 3 (one per prepared ref)", windowCalls)
	}

	// Per-ref success lines survive, and #4 was deferred rather than prepared.
	for _, n := range []int{1, 2} {
		want := fmt.Sprintf("launched clean-room review of cameronsjo/forgectl#%d", n)
		if !strings.Contains(out.String(), want) {
			t.Errorf("stdout = %q, want %q", out.String(), want)
		}
	}
	if strings.Contains(out.String(), "cameronsjo/forgectl#4") {
		t.Errorf("stdout = %q, deferred ref must not report a launch", out.String())
	}

	// Every summary precedes the returned error, which is the last thing to
	// happen. Assert their relative order on stderr.
	stderr := errOut.String()
	launchLine := strings.Index(stderr, "launch cameronsjo/forgectl#3 failed")
	deferLine := strings.Index(stderr, "1 PR(s) deferred by the concurrency cap")
	cleanupLine := strings.Index(stderr, "1 review(s) prepared but failed to launch")
	if launchLine < 0 || deferLine < 0 || cleanupLine < 0 {
		t.Fatalf("stderr = %q, want per-ref failure, deferral, and cleanup summaries", stderr)
	}
	if launchLine >= deferLine || deferLine >= cleanupLine {
		t.Errorf("stderr = %q, want per-ref line, then deferral, then cleanup", stderr)
	}

	record := readLog(t)
	assertCompletion(t, record, map[string]any{
		"launched": 2.0, "prepareFailed": 0.0, "launchFailed": 1.0,
		"skipped": 0.0, "deferred": 1.0, "verify": "gone", "gone": 1.0,
	})
}

func TestLaunchPicked_AllDispatchesLive(t *testing.T) {
	fakeClaudeBin(t)
	readLog := captureCompletionLog(t)

	ledger := newTmuxLedger("forgectl")
	fake := ledger.runner()
	client := verifyingClient(t, ledger, fake)

	cmd, _, _ := newTestCmd()
	if err := launchPicked(context.Background(), client, config.Config{}, cmd, pickRefs(1, 2), emptyStore(t), false); err != nil {
		t.Fatalf("launchPicked: %v", err)
	}
	if listCalls, _ := ledger.counts(); listCalls != 2 {
		t.Errorf("list-windows calls = %d, want 2", listCalls)
	}
	assertCompletion(t, readLog(t), map[string]any{"launched": 2.0, "verify": "live", "gone": 0.0})
}

func TestLaunchPicked_VerificationListErrorIsUnknown(t *testing.T) {
	fakeClaudeBin(t)
	readLog := captureCompletionLog(t)

	ledger := newTmuxLedger("forgectl")
	// Admission's list succeeds; the verification sweep's does not.
	ledger.listErr, ledger.listErrAfter = errors.New("boom: tmux exploded"), 1
	fake := ledger.runner()
	client := verifyingClient(t, ledger, fake)

	cmd, out, errOut := newTestCmd()
	err := launchPicked(context.Background(), client, config.Config{}, cmd, pickRefs(1, 2), emptyStore(t), false)
	if err == nil || !strings.Contains(err.Error(), "dispatch state is unknown") {
		t.Fatalf("error = %v, want the unknown wrapper", err)
	}
	// An unreadable tmux says nothing about any individual window. No ref may be
	// named as gone, on stderr or in the error.
	if strings.Contains(err.Error(), "disappeared during startup") {
		t.Errorf("error = %q, must not label any ref gone", err.Error())
	}
	if strings.Contains(errOut.String(), "disappeared") {
		t.Errorf("stderr = %q, must not label any ref gone", errOut.String())
	}
	// Per-ref accounting still landed before the failure.
	if !strings.Contains(out.String(), "launched clean-room review of cameronsjo/forgectl#1") {
		t.Errorf("stdout = %q, want the per-ref launch lines", out.String())
	}
	assertCompletion(t, readLog(t), map[string]any{"launched": 2.0, "verify": "unknown", "gone": 0.0})
}

func TestLaunchPicked_CanceledWaitIsUnknownWithoutListing(t *testing.T) {
	fakeClaudeBin(t)
	readLog := captureCompletionLog(t)

	ledger := newTmuxLedger("forgectl")
	fake := ledger.runner()
	client := pr.New(fake,
		pr.WithSessionsDir(t.TempDir()),
		pr.WithTmuxSession(ledger.session),
		pr.WithDispatchWait(func(ctx context.Context) error { return context.Canceled }),
	)

	cmd, _, _ := newTestCmd()
	err := launchPicked(context.Background(), client, config.Config{}, cmd, pickRefs(1), emptyStore(t), false)
	if err == nil || !strings.Contains(err.Error(), "dispatch state is unknown") {
		t.Fatalf("error = %v, want the unknown wrapper", err)
	}
	if listCalls, _ := ledger.counts(); listCalls != 1 {
		t.Errorf("list-windows calls = %d, want 1 — a failed wait must not list", listCalls)
	}
	assertCompletion(t, readLog(t), map[string]any{"launched": 1.0, "verify": "unknown", "gone": 0.0})
}

func TestLaunchPicked_NoVerifySkipsWaitAndSweep(t *testing.T) {
	fakeClaudeBin(t)
	readLog := captureCompletionLog(t)

	ledger := newTmuxLedger("forgectl")
	// The same window that the verified run reports gone. Under --no-verify the
	// command must succeed: the flag drops the observation, not the dispatch.
	ledger.diesOnDispatch[pickWindowName(t, 1)] = true
	fake := ledger.runner()
	waits := 0
	client := pr.New(fake,
		pr.WithSessionsDir(t.TempDir()),
		pr.WithTmuxSession(ledger.session),
		pr.WithDispatchWait(func(context.Context) error { waits++; return nil }),
	)

	cmd, _, _ := newTestCmd()
	if err := launchPicked(context.Background(), client, config.Config{}, cmd, pickRefs(1), emptyStore(t), true); err != nil {
		t.Fatalf("launchPicked: %v", err)
	}
	if waits != 0 {
		t.Errorf("waits = %d, want 0 under --no-verify", waits)
	}
	if listCalls, _ := ledger.counts(); listCalls != 1 {
		t.Errorf("list-windows calls = %d, want 1 (admission only)", listCalls)
	}
	// --no-verify is wait-only: the floor and capability probe still ran.
	if !hasTmuxCall(fake.Calls, "-V") || !hasTmuxCall(fake.Calls, "display-message") {
		t.Errorf("--no-verify must not skip the tmux floor or capability probe; calls=%v", fake.Calls)
	}
	assertCompletion(t, readLog(t), map[string]any{"launched": 1.0, "verify": "skipped", "gone": 0.0})
}

func hasTmuxCall(calls []exec.Call, verb string) bool {
	for _, c := range calls {
		if c.Name == "tmux" && len(c.Args) > 0 && c.Args[0] == verb {
			return true
		}
	}
	return false
}

func assertCompletion(t *testing.T, record map[string]any, want map[string]any) {
	t.Helper()
	for key, value := range want {
		if got := record[key]; got != value {
			t.Errorf("completion log %s = %v, want %v (record: %v)", key, got, value, record)
		}
	}
}
