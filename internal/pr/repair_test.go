package pr

// Test plan for repair.go and repairlog.go (forgectl#299 Task 2)
//
// Repair, inspect (Classification: read-only survey)
//   [x] Lists preparing/prepared/launching/needs-repair and nothing else
//   [x] Reports observed window liveness and workspace existence per row
// Repair --adopt-window (Classification: promotes a record to actionable)
//   [x] Refuses when no window carries the derived name — zero tmux mutation
//   [x] Refuses when the only window with that name is in another session
//   [x] Refuses when the workspace does not classify LIVE
//   [x] Converts a legacy record to a v2 active record with the resolved id
// Repair --rollback (Classification: destructive; refuses on uncertainty)
//   [x] Refuses on a live window
//   [x] Refuses on an unreadable window list (unreadable is not absent)
//   [x] --dry-run prints nothing to disk and touches no workspace
//   [x] Writes the intent row BEFORE teardown and completes it after
//   [x] A failure between the two leaves the workspace readable from the log
// Repair --forget-if-absent (Classification: record-only removal)
//   [x] Refuses while a workspace exists
// Non-reentrancy
//   [x] repair and cleanup each complete without a lock timeout

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/exec"
)

// repairRunner fakes tmux list-windows plus the session identity probes an
// adoption needs.
func repairRunner(listErr error, rows ...string) *exec.FakeRunner {
	out := strings.Join(rows, "\n")
	return &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		if name != "tmux" || len(args) == 0 {
			return "", nil
		}
		switch args[0] {
		case "-V":
			return "tmux 3.7b", nil
		case "display-message":
			return "123\x1f456\x1f@0", nil
		case "list-sessions":
			return "123\x1f456\x1f$1\x1fforgectl\x1f1\x1f0\x1f0\x1f/tmp", nil
		case "list-windows":
			if listErr != nil {
				return "", listErr
			}
			return out, nil
		}
		return "", nil
	}}
}

// winRow builds one list-windows fixture row under session/$id.
func sessionWinRow(session, sessionID, name string) string {
	return strings.Join([]string{"123", "456", "@1", sessionID, session, "1", name, "0", "1"}, "\x1f")
}

func repairClient(t *testing.T, fake *exec.FakeRunner) *Client {
	t.Helper()
	return New(fake, WithSessionsDir(t.TempDir()), WithFindingsDir(t.TempDir()),
		WithTmuxSession("forgectl"), WithLockWait(2*time.Second),
		WithTTYCheck(func() bool { return false }))
}

func mutatingTmuxCalls(calls []exec.Call) []exec.Call {
	var out []exec.Call
	for _, c := range calls {
		if c.Name != "tmux" || len(c.Args) == 0 {
			continue
		}
		switch c.Args[0] {
		case "new-window", "kill-window", "select-window", "rename-window", "move-window", "new-session":
			out = append(out, c)
		}
	}
	return out
}

func TestRepair_InspectListsOnlyTheUnsettledPhases(t *testing.T) {
	c := repairClient(t, repairRunner(nil))
	seedPhaseRecord(t, c, Ref{Owner: "o", Repo: "r", Number: 1}, PhasePreparing, "")
	seedPhaseRecord(t, c, Ref{Owner: "o", Repo: "r", Number: 2}, PhasePrepared, fakeWorkspace(t))
	seedPhaseRecord(t, c, Ref{Owner: "o", Repo: "r", Number: 3}, PhaseLaunching, fakeWorkspace(t))
	queuedRef := Ref{Owner: "o", Repo: "r", Number: 4}
	seedPhaseRecord(t, c, queuedRef, PhaseQueued, "")

	report, err := c.Repair(context.Background(), RepairOpts{})
	if err != nil {
		t.Fatalf("Repair inspect: %v", err)
	}
	if len(report.Items) != 3 {
		t.Fatalf("items = %d, want 3 (queued is not a repair candidate): %+v", len(report.Items), report.Items)
	}
	for _, it := range report.Items {
		if it.Ref == queuedRef.String() {
			t.Errorf("a queued record must not appear in repair: %+v", it)
		}
		if it.RecordPath == "" || it.FromPhase == "" {
			t.Errorf("item missing record path or phase: %+v", it)
		}
	}
}

func TestRepair_InspectReportsWorkspaceAndWindowObservations(t *testing.T) {
	ref := Ref{Owner: "o", Repo: "r", Number: 1}
	name := mustWindowName(t, ref)
	c := repairClient(t, repairRunner(nil, sessionWinRow("forgectl", "$1", name)))
	ws := fakeWorkspace(t)
	seedPhaseRecord(t, c, ref, PhaseLaunching, ws)

	report, err := c.Repair(context.Background(), RepairOpts{})
	if err != nil {
		t.Fatalf("Repair inspect: %v", err)
	}
	if len(report.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(report.Items))
	}
	if !report.Items[0].WindowLive {
		t.Error("WindowLive = false, want true — the derived window is in the listing")
	}
	if !report.Items[0].WorkspaceExists {
		t.Error("WorkspaceExists = false, want true")
	}
}

func TestRepairAdoptWindow_RefusesWhenNoWindowCarriesTheName(t *testing.T) {
	ref := Ref{Owner: "o", Repo: "r", Number: 1}
	fake := repairRunner(nil, sessionWinRow("forgectl", "$1", "unrelated"))
	c := repairClient(t, fake)
	path := seedPhaseRecord(t, c, ref, PhaseLaunching, fakeWorkspace(t))

	_, err := c.Repair(context.Background(), RepairOpts{Record: path, Apply: true, AdoptWindow: true})
	if err == nil {
		t.Fatal("expected a refusal: no window carries the derived name")
	}
	if got := readRecord(t, path).Phase; got != PhaseLaunching {
		t.Errorf("a refusal mutated the record: phase = %q", got)
	}
	if muts := mutatingTmuxCalls(fake.Calls); len(muts) != 0 {
		t.Errorf("a refusal issued tmux mutations: %+v", muts)
	}
}

func TestRepairAdoptWindow_RefusesAWindowInAnotherSession(t *testing.T) {
	ref := Ref{Owner: "o", Repo: "r", Number: 1}
	name := mustWindowName(t, ref)
	// Same name, different parent session id.
	fake := repairRunner(nil, sessionWinRow("other", "$9", name))
	c := repairClient(t, fake)
	path := seedPhaseRecord(t, c, ref, PhaseLaunching, fakeWorkspace(t))

	if _, err := c.Repair(context.Background(), RepairOpts{Record: path, Apply: true, AdoptWindow: true}); err == nil {
		t.Fatal("expected a refusal: the window belongs to another session")
	}
	if got := readRecord(t, path).Phase; got != PhaseLaunching {
		t.Errorf("a refusal mutated the record: phase = %q", got)
	}
	if muts := mutatingTmuxCalls(fake.Calls); len(muts) != 0 {
		t.Errorf("a refusal issued tmux mutations: %+v", muts)
	}
}

func TestRepairAdoptWindow_RefusesWhenTheWorkspaceIsNotLive(t *testing.T) {
	ref := Ref{Owner: "o", Repo: "r", Number: 1}
	name := mustWindowName(t, ref)
	fake := repairRunner(nil, sessionWinRow("forgectl", "$1", name))
	c := repairClient(t, fake)
	ws := fakeWorkspace(t)
	path := seedPhaseRecord(t, c, ref, PhaseLaunching, ws)
	if err := os.RemoveAll(ws); err != nil {
		t.Fatal(err)
	}

	_, err := c.Repair(context.Background(), RepairOpts{Record: path, Apply: true, AdoptWindow: true})
	if err == nil {
		t.Fatal("expected a refusal: adopting promotes the record to something teardown will RemoveAll")
	}
	if !strings.Contains(err.Error(), "workspace") {
		t.Errorf("refusal %q does not name the workspace", err)
	}
	if got := readRecord(t, path).Phase; got != PhaseLaunching {
		t.Errorf("a refusal mutated the record: phase = %q", got)
	}
}

func TestRepairAdoptWindow_ConvertsALegacyRecordToActive(t *testing.T) {
	ref := Ref{Owner: "o", Repo: "r", Number: 1}
	name := mustWindowName(t, ref)
	c := repairClient(t, repairRunner(nil, sessionWinRow("forgectl", "$1", name)))
	path, _ := seedSession(t, c, ref, time.Now().UTC())

	report, err := c.Repair(context.Background(), RepairOpts{Record: path, Apply: true, AdoptWindow: true})
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	bc := readRecord(t, path)
	if bc.Version != breadcrumbVersion || bc.Phase != PhaseActive {
		t.Fatalf("record = version %d phase %q, want a v2 active record", bc.Version, bc.Phase)
	}
	if !validWindowID(bc.WindowID) {
		t.Errorf("windowId %q is not generation-qualified", bc.WindowID)
	}
	if len(report.Items) != 1 || report.Items[0].ToPhase != string(PhaseActive) {
		t.Errorf("report = %+v, want one item landing on active", report.Items)
	}
}

func TestRepairRollback_RefusesOnALiveWindow(t *testing.T) {
	ref := Ref{Owner: "o", Repo: "r", Number: 1}
	name := mustWindowName(t, ref)
	c := repairClient(t, repairRunner(nil, sessionWinRow("forgectl", "$1", name)))
	ws := fakeWorkspace(t)
	path := seedPhaseRecord(t, c, ref, PhaseLaunching, ws)

	_, err := c.Repair(context.Background(), RepairOpts{Record: path, Apply: true, Rollback: true, Yes: true})
	if err == nil {
		t.Fatal("expected a refusal: the window is live")
	}
	if _, serr := os.Stat(ws); serr != nil {
		t.Errorf("a refusal removed the workspace: %v", serr)
	}
	if _, serr := os.Stat(path); serr != nil {
		t.Errorf("a refusal removed the record: %v", serr)
	}
}

func TestRepairRollback_RefusesOnAnUnreadableWindowList(t *testing.T) {
	ref := Ref{Owner: "o", Repo: "r", Number: 1}
	c := repairClient(t, repairRunner(errors.New("tmux is gone")))
	ws := fakeWorkspace(t)
	path := seedPhaseRecord(t, c, ref, PhaseLaunching, ws)

	_, err := c.Repair(context.Background(), RepairOpts{Record: path, Apply: true, Rollback: true, Yes: true})
	if err == nil {
		t.Fatal("expected a refusal: an unreadable window list is not an absent window")
	}
	if _, serr := os.Stat(ws); serr != nil {
		t.Errorf("a refusal removed the workspace: %v", serr)
	}
}

func TestRepairRollback_DryRunTouchesNothing(t *testing.T) {
	ref := Ref{Owner: "o", Repo: "r", Number: 1}
	c := repairClient(t, repairRunner(nil))
	ws := fakeWorkspace(t)
	path := seedPhaseRecord(t, c, ref, PhaseLaunching, ws)

	report, err := c.Repair(context.Background(), RepairOpts{Record: path, Apply: true, Rollback: true, DryRun: true, Yes: true})
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if len(report.Items) != 1 || !strings.HasPrefix(report.Items[0].Outcome, "would-") {
		t.Errorf("report = %+v, want a would-* outcome", report.Items)
	}
	if _, serr := os.Stat(ws); serr != nil {
		t.Errorf("--dry-run removed the workspace: %v", serr)
	}
	if _, serr := os.Stat(path); serr != nil {
		t.Errorf("--dry-run removed the record: %v", serr)
	}
	if _, serr := os.Stat(filepath.Join(c.SessionsDir(), repairLogName)); serr == nil {
		t.Error("--dry-run wrote an audit row")
	}
}

func TestRepairRollback_WritesIntentBeforeTeardownAndCompletesAfter(t *testing.T) {
	ref := Ref{Owner: "o", Repo: "r", Number: 1}
	c := repairClient(t, repairRunner(nil))
	ws := fakeWorkspace(t)
	path := seedPhaseRecord(t, c, ref, PhaseLaunching, ws)

	var rowsAtTeardown int
	orig := sandboxTeardown
	sandboxTeardown = func(ctx context.Context, run exec.Runner, workspace string) error {
		rows, _ := c.readRepairLog()
		rowsAtTeardown = len(rows)
		return orig(ctx, run, workspace)
	}
	t.Cleanup(func() { sandboxTeardown = orig })

	if _, err := c.Repair(context.Background(), RepairOpts{Record: path, Apply: true, Rollback: true, Yes: true}); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if rowsAtTeardown != 1 {
		t.Errorf("audit rows at teardown time = %d, want the intent row already written", rowsAtTeardown)
	}
	rows, err := c.readRepairLog()
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want intent + completion", len(rows))
	}
	if rows[0].Outcome != repairOutcomeIntent || rows[1].Outcome == repairOutcomeIntent {
		t.Errorf("rows = %+v, want intent then a completion", rows)
	}
	if _, serr := os.Stat(ws); serr == nil {
		t.Error("rollback left the workspace behind")
	}
}

func TestRepairRollback_AFailureMidwayLeavesTheWorkspaceInTheLog(t *testing.T) {
	ref := Ref{Owner: "o", Repo: "r", Number: 1}
	c := repairClient(t, repairRunner(nil))
	ws := fakeWorkspace(t)
	path := seedPhaseRecord(t, c, ref, PhaseLaunching, ws)

	orig := sandboxTeardown
	sandboxTeardown = func(context.Context, exec.Runner, string) error { return errors.New("disk said no") }
	t.Cleanup(func() { sandboxTeardown = orig })

	if _, err := c.Repair(context.Background(), RepairOpts{Record: path, Apply: true, Rollback: true, Yes: true}); err == nil {
		t.Fatal("expected the rollback to fail")
	}
	rows, err := c.readRepairLog()
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("no audit rows: the clean room is now unreachable")
	}
	if rows[0].Workspace != ws {
		t.Errorf("intent row workspace = %q, want %q — the only pointer left to the clean room", rows[0].Workspace, ws)
	}
	if _, serr := os.Stat(path); serr != nil {
		t.Errorf("a failed rollback removed the record: %v", serr)
	}
}

func TestRepairForgetIfAbsent_RefusesWhileAWorkspaceExists(t *testing.T) {
	ref := Ref{Owner: "o", Repo: "r", Number: 1}
	c := repairClient(t, repairRunner(nil))
	ws := fakeWorkspace(t)
	path := seedPhaseRecord(t, c, ref, PhaseLaunching, ws)

	_, err := c.Repair(context.Background(), RepairOpts{Record: path, Apply: true, ForgetIfAbsent: true})
	if err == nil {
		t.Fatal("expected a refusal: the workspace still exists")
	}
	if _, serr := os.Stat(path); serr != nil {
		t.Errorf("a refusal removed the record: %v", serr)
	}
}

func TestRepairForgetIfAbsent_RemovesTheRecordWhenNothingRemains(t *testing.T) {
	ref := Ref{Owner: "o", Repo: "r", Number: 1}
	c := repairClient(t, repairRunner(nil))
	path := seedPhaseRecord(t, c, ref, PhasePreparing, "")

	if _, err := c.Repair(context.Background(), RepairOpts{Record: path, Apply: true, ForgetIfAbsent: true}); err != nil {
		t.Fatalf("forget: %v", err)
	}
	if _, serr := os.Stat(path); !errors.Is(serr, os.ErrNotExist) {
		t.Errorf("record still present: %v", serr)
	}
}

// TestRepairAndCleanup_CompleteWithoutALockTimeout is the non-reentrancy
// regression: both are composite verbs that take the lifecycle lock ONCE and
// call the unlocked cores. A re-entered lock would surface as a timeout here.
func TestRepairAndCleanup_CompleteWithoutALockTimeout(t *testing.T) {
	ref := Ref{Owner: "o", Repo: "r", Number: 1}
	c := New(repairRunner(nil), WithSessionsDir(t.TempDir()), WithFindingsDir(t.TempDir()),
		WithTmuxSession("forgectl"), WithLockWait(300*time.Millisecond),
		WithTTYCheck(func() bool { return false }))
	path := seedPhaseRecord(t, c, ref, PhaseLaunching, fakeWorkspace(t))

	if _, err := c.Repair(context.Background(), RepairOpts{}); err != nil {
		t.Fatalf("repair inspect deadlocked or failed: %v", err)
	}
	if _, err := c.Repair(context.Background(), RepairOpts{Record: path, Apply: true, Rollback: true, Yes: true}); err != nil {
		t.Fatalf("repair --rollback deadlocked or failed: %v", err)
	}
	if err := c.Cleanup(context.Background(), time.Now().UTC().Format("2006-01-02")); err != nil {
		t.Fatalf("cleanup deadlocked or failed: %v", err)
	}
}
