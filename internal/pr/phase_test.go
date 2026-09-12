package pr

// Test plan for phase.go transitions, admission.go reservation, and the
// launch-path phase writes (forgectl#299 Task 2)
//
// transition (Classification: compare-and-write state machine under the lock)
//   [x] Refuses when the on-disk phase is not `from`, and mutates nothing
//   [x] Applies the mutation, sets the new phase, increments the revision
//   [x] Retries once on a revision mismatch and succeeds
//   [x] Returns errRecordRevisionMismatch on a second mismatch
//   [x] A legacy record refuses every transition
// reserve (Classification: admission decision, one lock hold)
//   [x] Two reserves at one free slot yield one preparing record and one
//       errReviewCapReached
//   [x] Refuses when List reports an unreadable record
//   [x] Refuses a ref that already has a queued record, naming its path
//   [x] A refusal writes no record
// Launch phases (Classification: crash-safety ordering)
//   [x] `launching` is on disk BEFORE tmux new-window is called
//   [x] `active` carries the newDispatch string
//   [x] A new-window error leaves needs-repair with `launch failed: …`
//   [x] A rename failure after a successful new-window leaves needs-repair
//       naming the window
//   [x] An invalid returned identity leaves needs-repair with the validation
//       reason
// PrepareMany (Classification: fan-out, one hold for N reservations)
//   [x] One lock hold covers every reservation and no clone runs inside it

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/tmux"
)

// seedPhaseRecord writes one v2 record at the given phase and returns its path.
func seedPhaseRecord(t *testing.T, c *Client, ref Ref, phase Phase, workspace string) string {
	t.Helper()
	bc := Breadcrumb{
		Workspace: workspace, Ref: ref.String(), Agent: "claude",
		CreatedAt: time.Now().UTC(), Version: breadcrumbVersion, Phase: phase, Revision: 1,
	}
	path, err := writeBreadcrumb(c.SessionsDir(), ref, bc)
	if err != nil {
		t.Fatalf("seed %s record: %v", phase, err)
	}
	return path
}

func readRecord(t *testing.T, path string) Breadcrumb {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // test-owned temp dir
	if err != nil {
		t.Fatalf("read record: %v", err)
	}
	bc, err := decodeBreadcrumb(data)
	if err != nil {
		t.Fatalf("decode record: %v", err)
	}
	return bc
}

func TestTransition_RefusesAWrongFromPhase(t *testing.T) {
	c := testClient(t, &exec.FakeRunner{})
	ref := Ref{Owner: "o", Repo: "r", Number: 1}
	path := seedPhaseRecord(t, c, ref, PhasePrepared, fakeWorkspace(t))

	err := c.transition(context.Background(), path, PhaseLaunching, PhaseActive, nil)
	if err == nil {
		t.Fatal("expected a refusal: the record is prepared, not launching")
	}
	if !strings.Contains(err.Error(), "prepared") || !strings.Contains(err.Error(), "launching") {
		t.Errorf("refusal %q does not name both the found and the expected phase", err)
	}
	bc := readRecord(t, path)
	if bc.Phase != PhasePrepared || bc.Revision != 1 {
		t.Errorf("a refusal mutated the record: phase=%q revision=%d", bc.Phase, bc.Revision)
	}
}

func TestTransition_AppliesMutationAndBumpsRevision(t *testing.T) {
	c := testClient(t, &exec.FakeRunner{})
	ref := Ref{Owner: "o", Repo: "r", Number: 1}
	path := seedPhaseRecord(t, c, ref, PhaseLaunching, fakeWorkspace(t))
	want := strings.Join([]string{"123", "456", "@7"}, tmux.FieldSep)

	err := c.transition(context.Background(), path, PhaseLaunching, PhaseActive, func(bc *Breadcrumb) error {
		bc.WindowID = want
		return nil
	})
	if err != nil {
		t.Fatalf("transition: %v", err)
	}
	bc := readRecord(t, path)
	if bc.Phase != PhaseActive {
		t.Errorf("phase = %q, want active", bc.Phase)
	}
	if bc.Revision != 2 {
		t.Errorf("revision = %d, want 2", bc.Revision)
	}
	if bc.WindowID != want {
		t.Errorf("windowId = %q, want %q", bc.WindowID, want)
	}
}

// racingFS rewrites the destination record out from under the writer on the
// first n compare passes, so the compare-and-write mismatches exactly as a
// concurrent forgectl process would make it.
type racingFS struct {
	osRecordFS
	t       *testing.T
	races   int
	seen    int
	dest    string
	mu      sync.Mutex
	bumpRev int
}

func (r *racingFS) Lstat(path string) (os.FileInfo, error) {
	r.mu.Lock()
	if path == r.dest && r.seen < r.races {
		r.seen++
		r.bumpRev++
		r.rewrite(path, 1+r.bumpRev)
	}
	r.mu.Unlock()
	return r.osRecordFS.Lstat(path)
}

func (r *racingFS) rewrite(path string, rev int) {
	r.t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // test-owned temp dir
	if err != nil {
		return
	}
	bc, err := decodeBreadcrumb(data)
	if err != nil {
		return
	}
	bc.Revision = rev
	out, err := encodeBreadcrumb(bc)
	if err != nil {
		return
	}
	_ = os.WriteFile(path, out, 0o600) //nolint:gosec // the test's own record path inside its own t.TempDir
}

func TestTransition_RetriesOnceOnARevisionMismatch(t *testing.T) {
	dir := t.TempDir()
	rfs := &racingFS{t: t, races: 1}
	c := New(&exec.FakeRunner{}, WithSessionsDir(dir), WithFindingsDir(t.TempDir()),
		WithRecordFS(rfs), WithLockWait(time.Second))
	ref := Ref{Owner: "o", Repo: "r", Number: 1}
	path := seedPhaseRecord(t, c, ref, PhaseLaunching, fakeWorkspace(t))
	rfs.dest = path

	if err := c.transition(context.Background(), path, PhaseLaunching, PhaseActive, func(bc *Breadcrumb) error {
		bc.WindowID = strings.Join([]string{"1", "2", "@3"}, tmux.FieldSep)
		return nil
	}); err != nil {
		t.Fatalf("transition should survive one mismatch: %v", err)
	}
	if got := readRecord(t, path).Phase; got != PhaseActive {
		t.Errorf("phase = %q, want active", got)
	}
}

func TestTransition_SecondMismatchReturnsTheSentinel(t *testing.T) {
	dir := t.TempDir()
	rfs := &racingFS{t: t, races: 99}
	c := New(&exec.FakeRunner{}, WithSessionsDir(dir), WithFindingsDir(t.TempDir()),
		WithRecordFS(rfs), WithLockWait(time.Second))
	ref := Ref{Owner: "o", Repo: "r", Number: 1}
	path := seedPhaseRecord(t, c, ref, PhaseLaunching, fakeWorkspace(t))
	rfs.dest = path

	err := c.transition(context.Background(), path, PhaseLaunching, PhaseActive, func(bc *Breadcrumb) error {
		bc.WindowID = strings.Join([]string{"1", "2", "@3"}, tmux.FieldSep)
		return nil
	})
	if !errors.Is(err, errRecordRevisionMismatch) {
		t.Fatalf("err = %v, want errRecordRevisionMismatch", err)
	}
}

func TestTransition_LegacyRecordRefusesEveryTransition(t *testing.T) {
	c := testClient(t, &exec.FakeRunner{})
	ref := Ref{Owner: "o", Repo: "r", Number: 1}
	path, _ := seedSession(t, c, ref, time.Now().UTC())

	for _, to := range []Phase{PhaseLaunching, PhaseActive, PhaseNeedsRepair} {
		err := c.transition(context.Background(), path, PhasePrepared, to, nil)
		if err == nil {
			t.Fatalf("legacy record accepted a transition to %s", to)
		}
		if !strings.Contains(err.Error(), "legacy") {
			t.Errorf("refusal %q does not name the legacy record", err)
		}
	}
}

// capRunner fakes tmux for admission: list-windows returns the given rows.
func capRunner(rows ...string) *exec.FakeRunner {
	out := strings.Join(rows, "\n")
	return &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		if name == "tmux" && len(args) > 0 && args[0] == "list-windows" {
			return out, nil
		}
		return "", nil
	}}
}

func TestReserve_TwoLaunchersAtOneFreeSlotYieldOneReservation(t *testing.T) {
	dir := t.TempDir()
	mk := func() *Client {
		return New(capRunner(), WithSessionsDir(dir), WithFindingsDir(t.TempDir()),
			WithTmuxSession("forgectl"), WithLockWait(2*time.Second))
	}
	first, second := mk(), mk()
	refA := Ref{Owner: "o", Repo: "r", Number: 1}
	refB := Ref{Owner: "o", Repo: "r", Number: 2}

	pathA, err := first.reserve(context.Background(), refA, 1, PrepareOpts{Agent: "claude"})
	if err != nil {
		t.Fatalf("first reserve: %v", err)
	}
	if got := readRecord(t, pathA).Phase; got != PhasePreparing {
		t.Errorf("reserved record phase = %q, want preparing", got)
	}

	_, err = second.reserve(context.Background(), refB, 1, PrepareOpts{Agent: "claude"})
	var capped *errReviewCapReached
	if !errors.As(err, &capped) {
		t.Fatalf("second reserve = %v, want *errReviewCapReached", err)
	}
	if capped.Max != 1 || capped.Live != 1 {
		t.Errorf("cap error = {Max:%d Live:%d}, want {1 1}", capped.Max, capped.Live)
	}
	entries, _ := os.ReadDir(dir)
	records := 0
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".json" {
			records++
		}
	}
	if records != 1 {
		t.Errorf("records on disk = %d, want 1 — a refusal must write nothing", records)
	}
}

func TestReserve_RefusesOnAnUnreadableRecord(t *testing.T) {
	c := New(capRunner(), WithSessionsDir(t.TempDir()), WithFindingsDir(t.TempDir()),
		WithTmuxSession("forgectl"), WithLockWait(time.Second))
	bad := []byte(`{"workspace":"/tmp/forgectl-workflow-x","ref":"o/r#9","createdAt":"2026-09-11T00:00:00Z","futureKey":true}` + "\n")
	if err := os.WriteFile(filepath.Join(c.SessionsDir(), "o-r-9-1.json"), bad, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := c.reserve(context.Background(), Ref{Owner: "o", Repo: "r", Number: 1}, 4, PrepareOpts{Agent: "claude"})
	if err == nil {
		t.Fatal("expected a refusal on an unreadable record")
	}
	if !strings.Contains(err.Error(), "could not be read") {
		t.Errorf("refusal %q does not name the unreadable record", err)
	}
}

func TestReserve_RefusesARefThatAlreadyHasARecord(t *testing.T) {
	c := New(capRunner(), WithSessionsDir(t.TempDir()), WithFindingsDir(t.TempDir()),
		WithTmuxSession("forgectl"), WithLockWait(time.Second))
	ref := Ref{Owner: "o", Repo: "r", Number: 1}
	existing := seedPhaseRecord(t, c, ref, PhaseQueued, "")

	_, err := c.reserve(context.Background(), ref, 4, PrepareOpts{Agent: "claude"})
	if err == nil {
		t.Fatal("expected a refusal: the ref already has a queued record")
	}
	if !strings.Contains(err.Error(), existing) {
		t.Errorf("refusal %q does not name the existing record %q", err, existing)
	}
}

// phaseLaunchRunner is successfulLaunchRunner with two knobs: a new-window
// error, and a forged new-window identity.
func phaseLaunchRunner(newWindowErr error, identity string) *exec.FakeRunner {
	created := false
	if identity == "" {
		identity = "123\x1f456\x1f@1"
	}
	return &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		if name == "tmux" && len(args) > 0 {
			switch args[0] {
			case "-V":
				return "tmux 3.7b", nil
			case "display-message":
				return "123\x1f456\x1f@0", nil
			case "list-sessions":
				if created {
					return "123\x1f456\x1f$1\x1fforgectl\x1f1\x1f0\x1f0\x1f/tmp", nil
				}
				return "", nil
			case "new-session":
				created = true
				return "123\x1f456\x1f$1", nil
			case "new-window":
				if newWindowErr != nil {
					return "", newWindowErr
				}
				return identity, nil
			}
		}
		return "", nil
	}}
}

func launchSession(t *testing.T, c *Client, ref Ref) Session {
	t.Helper()
	ws := fakeWorkspace(t)
	path := seedPhaseRecord(t, c, ref, PhasePrepared, ws)
	return Session{Ref: ref, Workspace: ws, Agent: "claude", Path: path, CreatedAt: time.Now().UTC()}
}

func fakeClaude(t *testing.T) {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil { //nolint:gosec // test stub, never executed
		t.Fatalf("write fake claude: %v", err)
	}
	t.Setenv("FORGECTL_CLAUDE_BIN", bin)
}

func TestLaunch_WritesLaunchingBeforeNewWindowAndActiveAfter(t *testing.T) {
	fakeClaude(t)
	fake := phaseLaunchRunner(nil, "")
	c := New(fake, WithSessionsDir(t.TempDir()), WithFindingsDir(t.TempDir()),
		WithTmuxSession("forgectl"), WithLockWait(time.Second))
	ref := Ref{Owner: "o", Repo: "r", Number: 1}
	sess := launchSession(t, c, ref)

	// Observe the record at the instant new-window is issued.
	var atNewWindow Phase
	inner := fake.RunFunc
	fake.RunFunc = func(name string, args []string) (string, error) {
		if name == "tmux" && len(args) > 0 && args[0] == "new-window" {
			atNewWindow = readRecord(t, sess.Path).Phase
		}
		return inner(name, args)
	}

	d, err := c.Launch(context.Background(), sess, config.Config{})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if atNewWindow != PhaseLaunching {
		t.Errorf("phase at new-window = %q, want launching (fsynced BEFORE the window)", atNewWindow)
	}
	bc := readRecord(t, sess.Path)
	if bc.Phase != PhaseActive {
		t.Fatalf("phase after launch = %q, want active", bc.Phase)
	}
	if bc.WindowID != d.WindowID {
		t.Errorf("record windowId = %q, want the dispatch's %q", bc.WindowID, d.WindowID)
	}
	if !validWindowID(bc.WindowID) {
		t.Errorf("record windowId %q is not the newDispatch spelling", bc.WindowID)
	}
}

func TestLaunch_NewWindowErrorLeavesNeedsRepair(t *testing.T) {
	fakeClaude(t)
	c := New(phaseLaunchRunner(errors.New("tmux said no"), ""), WithSessionsDir(t.TempDir()),
		WithFindingsDir(t.TempDir()), WithTmuxSession("forgectl"), WithLockWait(time.Second))
	sess := launchSession(t, c, Ref{Owner: "o", Repo: "r", Number: 1})

	if _, err := c.Launch(context.Background(), sess, config.Config{}); err == nil {
		t.Fatal("expected Launch to fail")
	}
	bc := readRecord(t, sess.Path)
	if bc.Phase != PhaseNeedsRepair {
		t.Fatalf("phase = %q, want needs-repair", bc.Phase)
	}
	if !strings.HasPrefix(bc.RepairReason, "launch failed:") {
		t.Errorf("repairReason = %q, want a 'launch failed:' reason", bc.RepairReason)
	}
}

func TestLaunch_UnvalidatableReturnedIdentityLeavesNeedsRepair(t *testing.T) {
	fakeClaude(t)
	// A new-window reply with no server generation on it. internal/tmux
	// refuses it at its own boundary — which is the outer of the two gates —
	// so what this pins is that the record still lands in needs-repair with the
	// identity failure named, rather than staying stuck in launching.
	c := New(phaseLaunchRunner(nil, "\x1f\x1f@1"), WithSessionsDir(t.TempDir()),
		WithFindingsDir(t.TempDir()), WithTmuxSession("forgectl"), WithLockWait(time.Second))
	sess := launchSession(t, c, Ref{Owner: "o", Repo: "r", Number: 1})

	if _, err := c.Launch(context.Background(), sess, config.Config{}); err == nil {
		t.Fatal("expected Launch to fail on an unvalidatable identity")
	}
	bc := readRecord(t, sess.Path)
	if bc.Phase != PhaseNeedsRepair {
		t.Fatalf("phase = %q, want needs-repair", bc.Phase)
	}
	if !strings.Contains(bc.RepairReason, "identity") {
		t.Errorf("repairReason = %q, want the identity failure named", bc.RepairReason)
	}
}

// TestCompleteLaunch_RefusesAnIdentityThatIsNotGenerationQualified drives the
// INNER gate directly. internal/tmux refuses a malformed identity first, so
// this arm is unreachable through Launch today — it is defense in depth
// against a future path that mints a Dispatch without going through
// NewWindow's parse, and a test is what keeps it from reading as dead code.
func TestCompleteLaunch_RefusesAnIdentityThatIsNotGenerationQualified(t *testing.T) {
	c := testClient(t, &exec.FakeRunner{})
	ref := Ref{Owner: "o", Repo: "r", Number: 1}
	ws := fakeWorkspace(t)
	path := seedPhaseRecord(t, c, ref, PhaseLaunching, ws)
	sess := Session{Ref: ref, Workspace: ws, Agent: "claude", Path: path}

	err := c.completeLaunch(context.Background(), sess, Dispatch{Ref: ref, WindowID: "@7"})
	if err == nil {
		t.Fatal("expected a refusal: a bare @N names a different window after a server restart")
	}
	bc := readRecord(t, path)
	if bc.Phase != PhaseNeedsRepair {
		t.Fatalf("phase = %q, want needs-repair", bc.Phase)
	}
	if !strings.Contains(bc.RepairReason, "validation") {
		t.Errorf("repairReason = %q, want the validation reason", bc.RepairReason)
	}
}

func TestLaunch_RenameFailureAfterTheWindowLeavesNeedsRepairNamingIt(t *testing.T) {
	fakeClaude(t)
	dir := t.TempDir()
	// Fail the SECOND rename: the first is the launching write, the second is
	// the active write that follows a real new-window.
	rfs := newFaultFSAt(t, "Rename", 2)
	c := New(phaseLaunchRunner(nil, ""), WithSessionsDir(dir), WithFindingsDir(t.TempDir()),
		WithTmuxSession("forgectl"), WithRecordFS(rfs), WithLockWait(time.Second))
	sess := launchSession(t, c, Ref{Owner: "o", Repo: "r", Number: 1})

	if _, err := c.Launch(context.Background(), sess, config.Config{}); err == nil {
		t.Fatal("expected Launch to fail when the active write cannot land")
	}
	bc := readRecord(t, sess.Path)
	if bc.Phase != PhaseNeedsRepair {
		t.Fatalf("phase = %q, want needs-repair", bc.Phase)
	}
	if !strings.Contains(bc.RepairReason, "@1") {
		t.Errorf("repairReason = %q, want it to name the window that exists", bc.RepairReason)
	}
}

func TestPrepareMany_ReservesUnderOneHoldAndNotAcrossTheClones(t *testing.T) {
	var mu sync.Mutex
	var log []string
	note := func(s string) { mu.Lock(); log = append(log, s); mu.Unlock() }

	fake := capRunner()
	inner := fake.RunFunc
	fake.RunFunc = func(name string, args []string) (string, error) {
		note("run:" + name)
		return inner(name, args)
	}
	c := New(fake, WithSessionsDir(t.TempDir()), WithFindingsDir(t.TempDir()),
		WithTmuxSession("forgectl"), WithLockWait(2*time.Second))
	c.onLock = func(verb, event string) { note(event + ":" + verb) }

	refs := []Ref{
		{Owner: "o", Repo: "r", Number: 1},
		{Owner: "o", Repo: "r", Number: 2},
		{Owner: "o", Repo: "r", Number: 3},
	}
	_ = c.PrepareMany(context.Background(), refs, 8, PrepareOpts{Agent: "claude"})

	mu.Lock()
	defer mu.Unlock()
	start, end := -1, -1
	holds := 0
	for i, e := range log {
		if e == "acquire:reserve" {
			holds++
			if start < 0 {
				start = i
			}
		}
		if e == "release:reserve" && end < 0 && start >= 0 {
			end = i
		}
	}
	if holds != 1 {
		t.Fatalf("reserve holds = %d, want exactly 1 for %d refs: %v", holds, len(refs), log)
	}
	for _, e := range log[start:end] {
		if e == "run:git" || e == "run:gh" {
			t.Errorf("a clone ran inside the reservation hold: %v", log)
		}
	}
}
