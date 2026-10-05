package cli

// Test plan for newPrListCmd / windowStatus (forgectl#242)
//
// windowStatus (Classification: pure rendering helper, fail-soft)
//   [x] Happy: a ref present in the live map renders "live"
//   [x] Happy: a ref absent from a READABLE live map renders "window gone"
//   [x] Unhappy: an unreadable tmux (ok=false) renders "?" for every ref,
//       including one that happens to be in the map
//
// newPrListCmd (Classification: cobra command, tmux cross-check)
//   [x] Happy: a session whose review window is live renders "live"
//   [x] Happy: a session whose window vanished renders "window gone" — the
//       forgectl#242 case, where tmux new-window exited 0, the breadcrumb
//       landed, and the agent then died taking the window with it
//   [x] Unhappy: list-windows erroring renders "?" and the command still
//       EXITS ZERO — a tmux read failure must not fail `pr list`

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/termsafe"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/pr"
)

// TestPrListJSON_KeySet pins the exact per-row key set of `pr list --json`.
func TestPrListJSON_KeySet(t *testing.T) {
	ref := pr.Ref{Owner: "cameronsjo", Repo: "forgectl", Number: 9}
	fake := prListRunner(nil, listWinRow("forgectl", pickWindowName(t, 9)))
	got, err := runPrList2JSON(t, fake, ref)
	if err != nil {
		t.Fatalf("pr list --json: %v", err)
	}

	var rows []map[string]json.RawMessage
	if err := json.Unmarshal([]byte(got), &rows); err != nil {
		t.Fatalf("stdout did not parse as a JSON array: %v\n%s", err, got)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1: %s", len(rows), got)
	}
	want := []string{"ref", "created_at", "path", "status", "phase", "repair_reason"}
	if len(rows[0]) != len(want) {
		t.Fatalf("row keys = %v, want exactly %v", keysOf(rows[0]), want)
	}
	for _, k := range want {
		if _, ok := rows[0][k]; !ok {
			t.Errorf("missing key %q; got %v", k, keysOf(rows[0]))
		}
	}
}

// TestPrListJSON_RowsMatchHumanTable asserts the JSON row's ref/status carry
// the same values the human table's columns 1 and 4 show.
func TestPrListJSON_RowsMatchHumanTable(t *testing.T) {
	ref := pr.Ref{Owner: "cameronsjo", Repo: "forgectl", Number: 9}
	fake := prListRunner(nil, listWinRow("forgectl", pickWindowName(t, 9)), listWinRow("forgectl", "shell"))

	humanOut, err := runPrList(t, fake, ref)
	if err != nil {
		t.Fatalf("pr list: %v", err)
	}

	fake2 := prListRunner(nil, listWinRow("forgectl", pickWindowName(t, 9)), listWinRow("forgectl", "shell"))
	jsonOut, err := runPrList2JSON(t, fake2, ref)
	if err != nil {
		t.Fatalf("pr list --json: %v", err)
	}

	var rows []struct {
		Ref    string `json:"ref"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal([]byte(jsonOut), &rows); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if !strings.Contains(humanOut, rows[0].Ref) {
		t.Errorf("json ref %q not found in human table:\n%s", rows[0].Ref, humanOut)
	}
	if rows[0].Status != "live" {
		t.Errorf("status = %q, want %q", rows[0].Status, "live")
	}
}

// TestPrListJSON_EmptyIsArrayNeverNull: no active sessions must encode as
// [], never null, so a caller can range over it unconditionally.
func TestPrListJSON_EmptyIsArrayNeverNull(t *testing.T) {
	got := runPrListOverJSON(t, prListRunner(nil), nil, nil)
	if strings.TrimSpace(got) != "[]" {
		t.Errorf("no-sessions --json = %q, want []", got)
	}
}

// TestPrListJSON_EmptyIsArrayNeverNull_StdoutOnlyOnSuccess pins the "no
// stderr on success under --json" clause.
func TestPrListJSON_NoStderrOnSuccess(t *testing.T) {
	sessionsDir := t.TempDir()
	client := pr.New(prListRunner(nil), pr.WithSessionsDir(sessionsDir), pr.WithTmuxSession("forgectl"))
	cmd := newPrListCmd(client)
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"--json"})
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("pr list --json: %v", err)
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want empty on success", stderr.String())
	}
}

// runPrList2JSON is runPrList's --json sibling: prepares one real review
// session against fake, then runs `pr list --json` over it.
func runPrList2JSON(t *testing.T, fake *exec.FakeRunner, ref pr.Ref) (string, error) {
	t.Helper()
	fakeClaudeBin(t)
	client := pr.New(fake, pr.WithSessionsDir(t.TempDir()), pr.WithTmuxSession("forgectl"))
	if _, err := client.Prepare(context.Background(), ref, pr.PrepareOpts{Agent: "claude"}); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	cmd := newPrListCmd(client)
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"--json"})
	err := cmd.ExecuteContext(context.Background())
	return stdout.String(), err
}

// runPrListOverJSON is runPrListOver's --json sibling.
func runPrListOverJSON(t *testing.T, fake *exec.FakeRunner, live, stale []pr.Ref) string {
	t.Helper()
	sessionsDir := t.TempDir()
	seedSummaries(t, sessionsDir, live, stale)
	client := pr.New(fake, pr.WithSessionsDir(sessionsDir), pr.WithTmuxSession("forgectl"))

	cmd := newPrListCmd(client)
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"--json"})
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("pr list --json: %v", err)
	}
	return stdout.String()
}

// listWinRow builds one `tmux list-windows -a` fixture line in the format
// internal/tmux parses: generation identity, session, index, name, active,
// panes on \x1f.
// The parent session id ($1) matches what tmuxDouble's new-session hands back,
// so a resolved review window really does sit under the review session.
func listWinRow(session, name string) string {
	return strings.Join([]string{"123", "456", "@1", "$1", session, "1", name, "0", "1"}, "\x1f")
}

// prListRunner layers list-windows control over dashRunner, which already
// fakes the gh pr view + git calls a real Prepare makes. listErr, when
// non-nil, makes list-windows fail — the unreadable-tmux case.
//
// A plain listErr is never enough to prove an absent default socket, so it
// remains the unreadable-tmux case regardless of its text.
func prListRunner(listErr error, rows ...string) *exec.FakeRunner {
	out := strings.Join(rows, "\n")
	prepare := dashRunner("[]").RunFunc
	return &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		if name == "tmux" && len(args) > 0 && args[0] == "list-windows" {
			if listErr != nil {
				return "", listErr
			}
			return out, nil
		}
		return prepare(name, args)
	}}
}

// runPrList prepares one real review session against fake, then runs
// `pr list` over it and returns stdout plus the command's error.
func runPrList(t *testing.T, fake *exec.FakeRunner, ref pr.Ref) (string, error) {
	t.Helper()
	fakeClaudeBin(t)
	client := pr.New(fake, pr.WithSessionsDir(t.TempDir()), pr.WithTmuxSession("forgectl"))
	if _, err := client.Prepare(context.Background(), ref, pr.PrepareOpts{Agent: "claude"}); err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	cmd := newPrListCmd(client)
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs(nil)
	err := cmd.ExecuteContext(context.Background())
	return stdout.String(), err
}

// runPrListOver runs `pr list` against a client whose session-state dir was
// seeded directly, so stale records (whose workspace no longer exists) can be
// staged — a real Prepare can only produce live ones.
func runPrListOver(t *testing.T, fake *exec.FakeRunner, live, stale []pr.Ref) string {
	t.Helper()
	sessionsDir := t.TempDir()
	seedSummaries(t, sessionsDir, live, stale)
	client := pr.New(fake, pr.WithSessionsDir(sessionsDir), pr.WithTmuxSession("forgectl"))

	cmd := newPrListCmd(client)
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs(nil)
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("pr list: %v", err)
	}
	return stdout.String()
}

// TestPrList_StaleOnlyIssuesNoTmuxCalls pins the cost contract: a list of
// nothing but stale records asks tmux nothing. Their windows are irrelevant,
// and paying a tmux round-trip to learn that would be pure waste.
func TestPrList_StaleOnlyIssuesNoTmuxCalls(t *testing.T) {
	fake := prListRunner(nil, listWinRow("forgectl", "shell"))
	got := runPrListOver(t, fake, nil, []pr.Ref{
		{Owner: "cameronsjo", Repo: "forgectl", Number: 1},
		{Owner: "cameronsjo", Repo: "forgectl", Number: 2},
	})

	if _, ok := findCliCall(fake.Calls, "tmux"); ok {
		t.Errorf("a stale-only list must issue ZERO tmux calls; got %+v", fake.Calls)
	}
	if strings.Count(got, workspaceMissingStatus) != 2 {
		t.Errorf("both stale rows must report %q:\n%s", workspaceMissingStatus, got)
	}
	if strings.Contains(got, "window gone") || strings.Contains(got, "\t?\n") {
		t.Errorf("a stale row must not borrow a window status:\n%s", got)
	}
}

// TestPrList_MixedBatchesOnlyLiveRefs pins that a mixed list costs exactly ONE
// liveness read, and that an unreadable tmux degrades only the live rows — a
// stale row's status never depends on tmux.
//
// WHICH refs enter that read is not observable here: WindowsLive takes them
// in-process and issues one blanket `tmux list-windows -a` whose arguments
// never name a ref. The exclusion is pinned at its observable boundary instead,
// by TestPrList_StaleOnlyIssuesNoTmuxCalls — a stale-only list issues zero
// calls, which only holds if stale refs are filtered out before the read.
func TestPrList_MixedBatchesOnlyLiveRefs(t *testing.T) {
	liveRef := pr.Ref{Owner: "cameronsjo", Repo: "forgectl", Number: 1}
	staleRef := pr.Ref{Owner: "cameronsjo", Repo: "forgectl", Number: 2}
	fake := prListRunner(errors.New("boom: tmux exploded"))
	got := runPrListOver(t, fake, []pr.Ref{liveRef}, []pr.Ref{staleRef})

	if n := countCliCalls(fake.Calls, "tmux", "list-windows"); n != 1 {
		t.Errorf("a mixed list must cost exactly one liveness read, got %d: %+v", n, fake.Calls)
	}

	var liveLine, staleLine string
	for _, line := range strings.Split(got, "\n") {
		switch {
		case strings.Contains(line, liveRef.String()):
			liveLine = line
		case strings.Contains(line, staleRef.String()):
			staleLine = line
		}
	}
	if liveLine == "" || staleLine == "" {
		t.Fatalf("both rows must render:\n%s", got)
	}
	// Status is field 4; phase (field 5) is "-" on these legacy records; the
	// reason (field 6) is empty.
	if !strings.HasSuffix(liveLine, "\t?\t-\t") {
		t.Errorf("the live row must degrade to %q under an unreadable tmux: %q", "?", liveLine)
	}
	if !strings.HasSuffix(staleLine, "\t"+workspaceMissingStatus+"\t-\t") {
		t.Errorf("the stale row must report %q regardless of tmux: %q", workspaceMissingStatus, staleLine)
	}
}

// countCliCalls counts Runner calls to name whose first argument is verb.
func countCliCalls(calls []exec.Call, name, verb string) int {
	var n int
	for _, c := range calls {
		if c.Name == name && len(c.Args) > 0 && c.Args[0] == verb {
			n++
		}
	}
	return n
}

// findCliCall reports whether any Runner call invoked name.
func findCliCall(calls []exec.Call, name string) (exec.Call, bool) {
	for _, c := range calls {
		if c.Name == name {
			return c, true
		}
	}
	return exec.Call{}, false
}

func TestWindowStatus_Live(t *testing.T) {
	ref := pr.Ref{Owner: "cameronsjo", Repo: "forgectl", Number: 9}
	if got := windowStatus(map[pr.Ref]bool{ref: true}, ref, tmuxReadable); got != "live" {
		t.Errorf("windowStatus(live) = %q, want %q", got, "live")
	}
}

func TestWindowStatus_Gone(t *testing.T) {
	ref := pr.Ref{Owner: "cameronsjo", Repo: "forgectl", Number: 9}
	if got := windowStatus(map[pr.Ref]bool{}, ref, tmuxReadable); got != "window gone" {
		t.Errorf("windowStatus(absent) = %q, want %q", got, "window gone")
	}
}

// TestWindowStatus_NoTmuxServer is forgectl#805 item 5: over an exited
// server's leftover socket no window is live, but the row must not say
// "window gone" — the strict reads refuse that verdict on the same evidence,
// and the label sends an operator to teardown.
//
// Mutation that turns it red: drop windowStatus's tmuxNoServer arm.
func TestWindowStatus_NoTmuxServer(t *testing.T) {
	ref := pr.Ref{Owner: "cameronsjo", Repo: "forgectl", Number: 9}
	if got := windowStatus(map[pr.Ref]bool{ref: false}, ref, tmuxNoServer); got != noTmuxServerStatus {
		t.Errorf("windowStatus(no server) = %q, want %q", got, noTmuxServerStatus)
	}
}

// TestWindowStatus_UnreadableTmux is the fail-soft pin: when tmux could not be
// read, even a ref the map claims is live must render "?" — the map is
// meaningless in that branch, and rendering anything definite would cry wolf on
// every healthy launch.
func TestWindowStatus_UnreadableTmux(t *testing.T) {
	ref := pr.Ref{Owner: "cameronsjo", Repo: "forgectl", Number: 9}
	if got := windowStatus(map[pr.Ref]bool{ref: true}, ref, tmuxUnreadable); got != "?" {
		t.Errorf("windowStatus(tmuxOK=false) = %q, want %q", got, "?")
	}
	if got := windowStatus(nil, ref, tmuxUnreadable); got != "?" {
		t.Errorf("windowStatus(nil map, tmuxOK=false) = %q, want %q", got, "?")
	}
}

func TestPrList_LiveWindow(t *testing.T) {
	ref := pr.Ref{Owner: "cameronsjo", Repo: "forgectl", Number: 9}
	fake := prListRunner(nil,
		listWinRow("forgectl", pickWindowName(t, 9)),
		listWinRow("forgectl", "shell"),
	)
	got, err := runPrList(t, fake, ref)
	if err != nil {
		t.Fatalf("pr list: %v", err)
	}
	if !strings.Contains(got, "\tlive\tprepared\t\n") {
		t.Errorf("pr list output missing the \"live\" status column (field 4) before the phase column:\n%s", got)
	}
	if !strings.Contains(got, ref.String()) {
		t.Errorf("pr list output missing the ref:\n%s", got)
	}

	// The COLUMN CONTRACT, pinned by position and not merely by suffix.
	// README documents field 3 as the breadcrumb `pr teardown` is fed, so a
	// suffix check on the status alone is not enough: a future change could
	// insert a column at position 2, keep status last, pass every other
	// assertion here, and still hand `cut -f3` a timestamp.
	line := strings.TrimSuffix(got, "\n")
	fields := strings.Split(line, "\t")
	if len(fields) != 6 {
		t.Fatalf("pr list row has %d tab-separated fields, want exactly 6 (ref, created, breadcrumb, status, phase, reason):\n%s", len(fields), got)
	}
	if fields[0] != ref.String() {
		t.Errorf("field 1 = %q, want the ref %q", fields[0], ref.String())
	}
	if !strings.HasSuffix(fields[2], ".json") {
		t.Errorf("field 3 = %q, want the breadcrumb path — this is the field `pr teardown` is fed", fields[2])
	}
	if fields[3] != "live" {
		t.Errorf("field 4 = %q, want the status %q", fields[3], "live")
	}
	// Prepare writes a version-2 record, so a freshly prepared session reports
	// its phase here. The legacy "-" rendering is pinned separately, over a
	// seeded pre-phase record.
	if fields[4] != "prepared" {
		t.Errorf("field 5 = %q, want %q — the phase Prepare records", fields[4], "prepared")
	}
}

// TestPrList_WindowGone is forgectl#242 end to end: the breadcrumb is on disk
// (Launch returned nil, because tmux new-window exits 0 before the agent runs)
// but no window exists, so the row must read "window gone" instead of listing
// the session as active forever.
func TestPrList_WindowGone(t *testing.T) {
	ref := pr.Ref{Owner: "cameronsjo", Repo: "forgectl", Number: 9}
	fake := prListRunner(nil, listWinRow("forgectl", "shell"))
	got, err := runPrList(t, fake, ref)
	if err != nil {
		t.Fatalf("pr list: %v", err)
	}
	if !strings.Contains(got, "window gone") {
		t.Errorf("pr list output missing \"window gone\" for a vanished review window:\n%s", got)
	}
}

// TestPrList_UnreadableTmux_DegradesAndSucceeds is the single biggest risk in
// this change, pinned: an unreadable tmux must degrade to "?" and MUST NOT
// fail the command or render "window gone".
func TestPrList_UnreadableTmux_DegradesAndSucceeds(t *testing.T) {
	ref := pr.Ref{Owner: "cameronsjo", Repo: "forgectl", Number: 9}
	fake := prListRunner(errors.New("boom: tmux exploded"))
	got, err := runPrList(t, fake, ref)
	if err != nil {
		t.Fatalf("pr list must succeed when tmux is unreadable, got: %v", err)
	}
	if !strings.Contains(got, "\t?\tprepared\t\n") {
		t.Errorf("pr list output missing the \"?\" status (field 4) for an unreadable tmux:\n%s", got)
	}
	if strings.Contains(got, "window gone") {
		t.Errorf("an unreadable tmux must NOT render \"window gone\" — that would flag every "+
			"healthy review as dead:\n%s", got)
	}
}

// listOverPhased runs `pr list` (human and --json) over records seeded through
// the real loader, and returns both outputs plus the summaries for dash.
func listOverPhased(t *testing.T, recs []phasedRecord) (human string, jsonRows []prListRowJSON, summaries []pr.SessionSummary) {
	t.Helper()
	dir := t.TempDir()
	summaries = seedPhasedSummaries(t, dir, recs)
	run := func(args ...string) string {
		client := pr.New(prListRunner(nil), pr.WithSessionsDir(dir), pr.WithTmuxSession("forgectl"))
		cmd := newPrListCmd(client)
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(new(bytes.Buffer))
		cmd.SetArgs(args)
		if err := cmd.ExecuteContext(context.Background()); err != nil {
			t.Fatalf("pr list %v: %v", args, err)
		}
		return out.String()
	}
	human = run()
	if err := json.Unmarshal([]byte(run("--json")), &jsonRows); err != nil {
		t.Fatalf("--json did not parse: %v", err)
	}
	return human, jsonRows, summaries
}

// TestPrList_ShowsWhyASessionNeedsRepair is forgectl#542: the reason `pr dash`
// and `pr repair` show must be on `pr list` too, worded and capped identically,
// in both the human row (a sixth column) and --json.
func TestPrList_ShowsWhyASessionNeedsRepair(t *testing.T) {
	long := "launch failed:\tx\ny " + strings.Repeat("stderr noise ", 100)
	repairRef := pr.Ref{Owner: "cameronsjo", Repo: "forgectl", Number: 41}
	queuedRef := pr.Ref{Owner: "cameronsjo", Repo: "forgectl", Number: 43}
	human, rows, summaries := listOverPhased(t, []phasedRecord{
		{ref: repairRef, phase: pr.PhaseNeedsRepair, repairReason: long},
		{ref: queuedRef, phase: pr.PhaseQueued},
	})

	byRef := map[string]prListRowJSON{}
	for _, r := range rows {
		byRef[r.Ref] = r
	}
	want := repairReasonLine(long)
	if !strings.HasSuffix(want, termsafe.TruncatedMarker) {
		t.Fatalf("fixture reason should be capped: %q", want)
	}
	if got := byRef[repairRef.String()].RepairReason; got != want {
		t.Errorf("json repair_reason = %q, want the dash-capped %q", got, want)
	}
	if got := byRef[queuedRef.String()].RepairReason; got != "" {
		t.Errorf("a queued row carries repair_reason %q, want empty", got)
	}

	// REASON is the sixth tab-separated column, the dash-capped text on a
	// needs-repair row and empty elsewhere; the first five are untouched.
	for _, s := range summaries {
		var line string
		for _, l := range strings.Split(human, "\n") {
			if strings.HasPrefix(l, s.Ref().String()+"\t") {
				line = l
			}
		}
		if line == "" {
			t.Fatalf("no human row for %s:\n%s", s.Ref(), human)
		}
		cols := strings.Split(line, "\t")
		if len(cols) != 6 {
			t.Fatalf("%s: %d columns, want 6: %q", s.Ref(), len(cols), line)
		}
		if cols[4] != phaseLabel(s) {
			t.Errorf("%s: field 5 = %q, want the phase %q unchanged", s.Ref(), cols[4], phaseLabel(s))
		}
		wantReason := ""
		if s.Phase() == pr.PhaseNeedsRepair {
			wantReason = want
		}
		if cols[5] != wantReason {
			t.Errorf("%s: field 6 = %q, want %q", s.Ref(), cols[5], wantReason)
		}
	}
}

// blockingTmux is a Runner whose tmux calls hang until their context ends.
type blockingTmux struct{ *exec.FakeRunner }

func (b blockingTmux) Run(ctx context.Context, name string, args ...string) (string, error) {
	if name == "tmux" {
		<-ctx.Done()
		return "", ctx.Err()
	}
	return b.FakeRunner.Run(ctx, name, args...)
}

// TestPrTeardown_NotesAnUnresponsiveTmuxOnStderr: when tmux never answers, the
// window's state is unknown, so teardown removes nothing and says so on stderr
// — the slog warning alone is discarded by default. The note says the record
// was parked, and it really was: a legacy record (no version) is converted to
// a v2 needs-repair record rather than left with no repair path (#696). The
// not-parked wording is pinned by TestCleanupFailureLine_TimeoutNotParked.
func TestPrTeardown_NotesAnUnresponsiveTmuxOnStderr(t *testing.T) {
	for _, tc := range []struct {
		name       string
		legacy     bool
		wantParked bool
	}{
		{"v2 record is parked", false, true},
		{"legacy record is converted and parked", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			ws, err := os.MkdirTemp("", "forgectl-workflow-test-*")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(ws) })
			var path string
			if tc.legacy {
				data, merr := json.Marshal(map[string]any{
					"ref": "o/r#1", "agent": "claude", "workspace": ws,
					"createdAt": time.Now().UTC().Format(time.RFC3339Nano),
				})
				if merr != nil {
					t.Fatal(merr)
				}
				path = filepath.Join(dir, "o-r-1-1.json")
				if werr := os.WriteFile(path, append(data, '\n'), 0o600); werr != nil {
					t.Fatal(werr)
				}
			} else {
				path = seedRepairRecord(t, dir, "o/r#1", "prepared", ws)
			}
			client := pr.New(blockingTmux{&exec.FakeRunner{}}, pr.WithSessionsDir(dir), pr.WithTmuxSession("forgectl"),
				pr.WithTTYCheck(func() bool { return false }))

			cmd := newPrTeardownCmd(client)
			var out, errOut bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&errOut)
			cmd.SetArgs([]string{path})
			// The parent deadline stands in for the package's own (unexported)
			// budget: either way the tmux call is cut off and its state unknown.
			ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
			defer cancel()
			err = cmd.ExecuteContext(ctx)
			if !errors.Is(err, pr.ErrWindowKillTimedOut) {
				t.Fatalf("err = %v, want ErrWindowKillTimedOut", err)
			}
			if got := !errors.Is(err, pr.ErrRecordNotParked); got != tc.wantParked {
				t.Errorf("parked = %v, want %v (err = %v)", got, tc.wantParked, err)
			}
			note := errOut.String()
			if !strings.Contains(note, "may still be running") {
				t.Errorf("stderr = %q, want the unresponsive-tmux note", note)
			}
			if tc.wantParked && !strings.Contains(note, "the record is parked as needs-repair") {
				t.Errorf("stderr = %q, want it to say the record was parked", note)
			}
			if !tc.wantParked && (strings.Contains(note, "is parked") || !strings.Contains(note, "could not be parked")) {
				t.Errorf("stderr = %q, must NOT claim the record was parked", note)
			}
			if _, serr := os.Stat(ws); serr != nil {
				t.Errorf("workspace was removed: %v", serr)
			}
			if _, serr := os.Stat(path); serr != nil {
				t.Errorf("record was removed: %v", serr)
			}
			if strings.Contains(out.String(), "torn down") {
				t.Errorf("stdout claims a teardown: %q", out.String())
			}
		})
	}
}

// TestPrListHelpNamesEveryWindowValue is forgectl#815 item 6: the WINDOW
// column's help lists every value windowStatus can print, including the
// exited-server one.
//
// Mutation that turns it red: drop "no tmux server" from `pr list`'s Long.
func TestPrListHelpNamesEveryWindowValue(t *testing.T) {
	long := strings.Join(strings.Fields(newPrListCmd(nil).Long), " ")
	for _, value := range []string{"live", "window gone", noTmuxServerStatus, "?"} {
		if !strings.Contains(long, value) {
			t.Errorf("pr list --help does not name the WINDOW value %q", value)
		}
	}
}
