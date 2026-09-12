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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

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
	if got := report.Items[0].WindowLive; got == nil || !*got {
		t.Errorf("WindowLive = %v, want a known true — the derived window is in the listing", fmtBoolPtr(got))
	}
	if got := report.Items[0].WorkspaceExists; got == nil || !*got {
		t.Errorf("WorkspaceExists = %v, want a known true", fmtBoolPtr(got))
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

// TestRepairAdoptWindow_CannotWriteThroughASubdirectoryOperand is the
// wrong-record write, pinned. The atomic writer addresses a record as
// sessionsDir + basename, so an operand that merely RESOLVES into the sessions
// dir — a subdirectory entry, a symlink pointing in — would be read from one
// file and written over a different one sharing its basename. The victim's
// clean-room pointer would be destroyed and a record would read `active` for a
// ref it was never written for.
func TestRepairAdoptWindow_CannotWriteThroughASubdirectoryOperand(t *testing.T) {
	victimRef := Ref{Owner: "o", Repo: "r", Number: 1}
	decoyRef := Ref{Owner: "o", Repo: "r", Number: 99}
	name := mustWindowName(t, decoyRef)
	c := repairClient(t, repairRunner(nil, sessionWinRow("forgectl", "$1", name)))

	// The victim: a real member whose basename the decoy will share.
	victimPath := seedPhaseRecord(t, c, victimRef, PhasePrepared, fakeWorkspace(t))
	victimBefore, err := os.ReadFile(victimPath) //nolint:gosec // test-owned temp dir
	if err != nil {
		t.Fatal(err)
	}

	// The decoy: same basename, one directory down, naming a different ref.
	sub := filepath.Join(c.SessionsDir(), "sub")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	decoy := Breadcrumb{
		Workspace: fakeWorkspace(t), Ref: decoyRef.String(), Agent: "claude",
		CreatedAt: time.Now().UTC(), Version: breadcrumbVersion, Phase: PhasePrepared, Revision: 1,
	}
	decoyData, err := encodeBreadcrumb(decoy)
	if err != nil {
		t.Fatal(err)
	}
	decoyPath := filepath.Join(sub, filepath.Base(victimPath))
	if err := os.WriteFile(decoyPath, decoyData, 0o600); err != nil {
		t.Fatal(err)
	}

	_, err = c.Repair(context.Background(), RepairOpts{Record: decoyPath, Apply: true, AdoptWindow: true})
	if err == nil {
		t.Fatal("expected a refusal: the operand is not an enumerated member of the sessions dir")
	}
	after, rerr := os.ReadFile(victimPath) //nolint:gosec // test-owned temp dir
	if rerr != nil {
		t.Fatalf("the victim record was removed: %v", rerr)
	}
	if !bytes.Equal(victimBefore, after) {
		t.Fatalf("a sub-directory operand rewrote the top-level record:\nbefore %s\nafter  %s", victimBefore, after)
	}
}

// TestTransition_RefusesAPathThatIsNotADirectSessionsDirEntry drives the
// backstop directly: even handed a decoded, valid record one level down,
// transition must refuse rather than write to sessionsDir/<basename>.
func TestTransition_RefusesAPathThatIsNotADirectSessionsDirEntry(t *testing.T) {
	c := testClient(t, &exec.FakeRunner{})
	ref := Ref{Owner: "o", Repo: "r", Number: 1}
	top := seedPhaseRecord(t, c, ref, PhasePrepared, fakeWorkspace(t))
	before, err := os.ReadFile(top) //nolint:gosec // test-owned temp dir
	if err != nil {
		t.Fatal(err)
	}

	sub := filepath.Join(c.SessionsDir(), "sub")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(sub, filepath.Base(top))
	if err := os.WriteFile(nested, before, 0o600); err != nil { //nolint:gosec // a path this test built inside its own t.TempDir
		t.Fatal(err)
	}

	err = c.transition(context.Background(), nested, PhasePrepared, PhaseLaunching, nil)
	if err == nil {
		t.Fatal("expected a refusal: the record is not a direct entry of the sessions dir")
	}
	if !strings.Contains(err.Error(), "directly") {
		t.Errorf("refusal %q does not say the record must sit directly in the session directory", err)
	}
	after, _ := os.ReadFile(top) //nolint:gosec // test-owned temp dir
	if !bytes.Equal(before, after) {
		t.Error("the top-level record was rewritten through a nested path")
	}
}

// TestRepairRollback_InteractiveGateNamesTheRemovalAndHonorsBothAnswers covers
// the gate a human actually meets. It must be the REMOVAL confirmer, never the
// review-posting approver: a caller that wired an auto-approver for posting
// must not thereby have consented to deleting a clean room.
func TestRepairRollback_InteractiveGateNamesTheRemovalAndHonorsBothAnswers(t *testing.T) {
	for _, tc := range []struct {
		name     string
		answer   bool
		wantGone bool
		wantOut  string
	}{
		{"approved", true, true, "rolled-back"},
		{"declined", false, false, "declined"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ref := Ref{Owner: "o", Repo: "r", Number: 1}
			var prompted string
			postApprovals := 0
			c := New(repairRunner(nil),
				WithSessionsDir(t.TempDir()), WithFindingsDir(t.TempDir()),
				WithTmuxSession("forgectl"), WithLockWait(2*time.Second),
				WithTTYCheck(func() bool { return true }),
				WithApprover(func(string) (bool, error) { postApprovals++; return true, nil }),
				WithRemovalConfirmer(func(prompt string) (bool, error) { prompted = prompt; return tc.answer, nil }),
			)
			ws := fakeWorkspace(t)
			path := seedPhaseRecord(t, c, ref, PhaseLaunching, ws)

			report, err := c.Repair(context.Background(), RepairOpts{Record: path, Apply: true, Rollback: true})
			if err != nil {
				t.Fatalf("rollback: %v", err)
			}
			if postApprovals != 0 {
				t.Errorf("the review-POSTING approver was consulted %d time(s) for a deletion", postApprovals)
			}
			if !strings.Contains(prompted, ref.String()) || !strings.Contains(prompted, ws) {
				t.Errorf("prompt %q does not name both the ref and the clean room being removed", prompted)
			}
			if len(report.Items) != 1 || report.Items[0].Outcome != tc.wantOut {
				t.Fatalf("report = %+v, want outcome %q", report.Items, tc.wantOut)
			}
			_, serr := os.Stat(ws)
			if tc.wantGone && serr == nil {
				t.Error("an approved rollback left the clean room behind")
			}
			if !tc.wantGone && serr != nil {
				t.Errorf("a declined rollback removed the clean room: %v", serr)
			}
			if _, serr := os.Stat(path); (serr == nil) == tc.wantGone {
				t.Errorf("record present=%v, want present=%v", serr == nil, !tc.wantGone)
			}
		})
	}
}

// TestRollbackPrompt_ClampsControlBytes: a workspace only has to be an absolute
// path to validate, so it can carry control or bidi bytes — and this is the one
// surface a human is asked to approve a deletion on.
func TestRollbackPrompt_ClampsControlBytes(t *testing.T) {
	bc := Breadcrumb{Workspace: "/tmp/forgectl-workflow-\x1b[2Kevil", Ref: "o/r#1"}
	got := rollbackPrompt(Ref{Owner: "o", Repo: "r", Number: 1}, bc)
	if strings.Contains(got, "\x1b") {
		t.Errorf("prompt carries a raw escape byte: %q", got)
	}
}

// TestMarkNeedsRepair_ParksAReservationWithNoWorkspace is the park that could
// never run: a reservation has no workspace, and needs-repair used to require
// one, so every failed prepare left its slot held forever with no reason
// recorded.
func TestMarkNeedsRepair_ParksAReservationWithNoWorkspace(t *testing.T) {
	c := testClient(t, &exec.FakeRunner{})
	ref := Ref{Owner: "o", Repo: "r", Number: 1}
	path := seedPhaseRecord(t, c, ref, PhasePreparing, "")

	if err := c.markNeedsRepair(context.Background(), path, "prepare failed: gh said no"); err != nil {
		t.Fatalf("markNeedsRepair on a reservation: %v", err)
	}
	bc := readRecord(t, path)
	if bc.Phase != PhaseNeedsRepair {
		t.Fatalf("phase = %q, want needs-repair", bc.Phase)
	}
	if !strings.Contains(bc.RepairReason, "gh said no") {
		t.Errorf("repairReason = %q, want the prepare failure", bc.RepairReason)
	}
	// And it no longer holds a slot: needs-repair is released for the operator.
	summaries, _, err := c.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if occ := occupancyFromSnapshot(summaries, 0, map[string]bool{}); occ != 0 {
		t.Errorf("occupied = %d, want 0 — a parked record must release its slot", occ)
	}
}

// TestRepair_UnreadableRecordIsAReportRow: such a record refuses every counting
// arm, so it blocks every launch. Reporting "no records need repair" while
// `pr pick` is refused is a survey verb answering the opposite of the truth.
func TestRepair_UnreadableRecordIsAReportRow(t *testing.T) {
	c := repairClient(t, repairRunner(nil))
	bad := filepath.Join(c.SessionsDir(), "o-r-9-1.json")
	if err := os.WriteFile(bad, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := c.Repair(context.Background(), RepairOpts{})
	if err != nil {
		t.Fatalf("Repair inspect: %v", err)
	}
	if len(report.Items) != 1 {
		t.Fatalf("items = %d, want the unreadable row: %+v", len(report.Items), report.Items)
	}
	got := report.Items[0]
	if got.Outcome != repairOutcomeUnreadable {
		t.Errorf("outcome = %q, want %q", got.Outcome, repairOutcomeUnreadable)
	}
	if got.RecordPath != bad {
		t.Errorf("record path = %q, want %q — a count names no way out", got.RecordPath, bad)
	}
	if got.Error == "" {
		t.Error("the row carries no decode error")
	}
}

func TestRepairForgetIfAbsent_SetsAnUnreadableRecordAside(t *testing.T) {
	c := repairClient(t, repairRunner(nil))
	bad := filepath.Join(c.SessionsDir(), "o-r-9-1.json")
	raw := []byte("{not json")
	if err := os.WriteFile(bad, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	// The other two arms cannot read it, so they must refuse and name the one
	// that can.
	if _, err := c.Repair(context.Background(), RepairOpts{Record: bad, Apply: true, Rollback: true, Yes: true}); err == nil {
		t.Fatal("expected --rollback to refuse a record it cannot read")
	} else if !strings.Contains(err.Error(), RepairModeForgetIfAbsent) {
		t.Errorf("refusal %q does not name the mode that can settle it", err)
	}
	// And this arm is gated: it is the only one that cannot prove what it acts on.
	if _, err := c.Repair(context.Background(), RepairOpts{Record: bad, Apply: true, ForgetIfAbsent: true}); err == nil {
		t.Fatal("expected a refusal off a TTY without --yes")
	} else if !strings.Contains(err.Error(), "--yes") {
		t.Errorf("refusal %q does not name --yes", err)
	}
	if _, serr := os.Stat(bad); serr != nil {
		t.Fatalf("a refusal moved the record: %v", serr)
	}

	report, err := c.Repair(context.Background(), RepairOpts{Record: bad, Apply: true, ForgetIfAbsent: true, Yes: true})
	if err != nil {
		t.Fatalf("set aside an unreadable record: %v", err)
	}
	if len(report.Items) != 1 || report.Items[0].Outcome != repairOutcomeSetAside {
		t.Fatalf("report = %+v, want one set-aside item", report.Items)
	}
	assertSetAside(t, c, bad, raw)

	rows, err := c.readRepairLog()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("audit rows = %d, want intent + completion", len(rows))
	}
	if rows[0].RecordPath != bad {
		t.Errorf("intent row path = %q, want %q", rows[0].RecordPath, bad)
	}
	if rows[0].Record != string(raw) {
		t.Errorf("intent row record = %q, want the record's own bytes %q — the only field that can name what was set aside",
			rows[0].Record, raw)
	}
}

// assertSetAside proves the rename happened rather than an unlink: no .json
// entry remains (so nothing is blocked), and a sibling carrying the original
// bytes does.
func assertSetAside(t *testing.T, c *Client, was string, want []byte) {
	t.Helper()
	entries, err := os.ReadDir(c.SessionsDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, serr := os.Stat(was); !errors.Is(serr, os.ErrNotExist) {
		t.Errorf("the original name still exists: %v", serr)
	}
	var aside string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), filepath.Base(was)+unreadableSuffix) {
			aside = filepath.Join(c.SessionsDir(), e.Name())
		}
		if filepath.Ext(e.Name()) == ".json" {
			t.Errorf("a .json entry survives, so the launch block is not cleared: %s", e.Name())
		}
	}
	if aside == "" {
		t.Fatalf("no set-aside file in %v — the record was unlinked, not preserved", entries)
	}
	got, err := os.ReadFile(aside) //nolint:gosec // a path this test just enumerated in its own t.TempDir
	if err != nil {
		t.Fatalf("read the set-aside file: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("set-aside bytes = %q, want the original %q", got, want)
	}
}

// TestRepairSetAside_RefusesWhileANewerBuildsWindowIsLive is the reviewer's
// probe. A record written by a NEWER forgectl is intact and describes a running
// session, but this build cannot decode it (the version is unknown), so it
// lands on the same arm a torn file does. Removing it would orphan a live clean
// room and hide the session from the build that owns it.
func TestRepairSetAside_RefusesWhileANewerBuildsWindowIsLive(t *testing.T) {
	ref := Ref{Owner: "o", Repo: "r", Number: 1}
	name := mustWindowName(t, ref)
	ws := fakeWorkspace(t)
	future := []byte(`{"workspace":"` + ws + `","ref":"o/r#1","agent":"claude",` +
		`"createdAt":"2026-09-12T00:00:00Z","version":3,"phase":"active","revision":4}` + "\n")

	// Window live: the refusal.
	c := repairClient(t, repairRunner(nil, sessionWinRow("forgectl", "$1", name)))
	rec := filepath.Join(c.SessionsDir(), "o-r-1-1.json")
	if err := os.WriteFile(rec, future, 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := c.Repair(context.Background(), RepairOpts{})
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if len(report.Items) != 1 || report.Items[0].Outcome != repairOutcomeUnreadable {
		t.Fatalf("report = %+v, want one unreadable row", report.Items)
	}
	_, err = c.Repair(context.Background(), RepairOpts{Record: rec, Apply: true, ForgetIfAbsent: true, Yes: true})
	if err == nil {
		t.Fatal("expected a refusal: the record names a ref whose review window is live")
	}
	if !strings.Contains(err.Error(), ref.String()) {
		t.Errorf("refusal %q does not name the ref it read out of the record", err)
	}
	if _, serr := os.Stat(rec); serr != nil {
		t.Errorf("a refusal moved the record: %v", serr)
	}
	if _, serr := os.Stat(ws); serr != nil {
		t.Errorf("a refusal touched the clean room: %v", serr)
	}

	// Window gone: it sets aside, and the launch block clears.
	dead := repairClient(t, repairRunner(nil))
	deadRec := filepath.Join(dead.SessionsDir(), "o-r-1-1.json")
	if err := os.WriteFile(deadRec, future, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := dead.reserve(context.Background(), Ref{Owner: "o", Repo: "r", Number: 2}, 4, PrepareOpts{Agent: "claude"}); err == nil {
		t.Fatal("setup: an unreadable record should block a reservation")
	}
	if _, err := dead.Repair(context.Background(), RepairOpts{Record: deadRec, Apply: true, ForgetIfAbsent: true, Yes: true}); err != nil {
		t.Fatalf("set aside: %v", err)
	}
	entries, _ := os.ReadDir(dead.SessionsDir())
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".json" {
			t.Errorf("a .json entry survives: %s", e.Name())
		}
	}
	if _, err := dead.reserve(context.Background(), Ref{Owner: "o", Repo: "r", Number: 2}, 4, PrepareOpts{Agent: "claude"}); err != nil {
		t.Errorf("launches are still blocked after setting the record aside: %v", err)
	}
	rows, err := dead.readRepairLog()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) < 1 {
		t.Fatal("no audit row")
	}
	if rows[0].Ref != ref.String() {
		t.Errorf("intent row ref = %q, want the best-effort %q", rows[0].Ref, ref.String())
	}
	if !strings.Contains(rows[0].Record, `"version":3`) {
		t.Errorf("intent row record = %q, want the record's own bytes", rows[0].Record)
	}
}

// TestRepairSetAside_InteractiveGateHonorsBothAnswers: this arm is gated for
// the same reason rollback is, and by the same confirmer.
func TestRepairSetAside_InteractiveGateHonorsBothAnswers(t *testing.T) {
	for _, tc := range []struct {
		name      string
		answer    bool
		wantAside bool
	}{
		{"approved", true, true},
		{"declined", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var prompted string
			postApprovals := 0
			c := New(repairRunner(nil),
				WithSessionsDir(t.TempDir()), WithFindingsDir(t.TempDir()),
				WithTmuxSession("forgectl"), WithLockWait(2*time.Second),
				WithTTYCheck(func() bool { return true }),
				WithApprover(func(string) (bool, error) { postApprovals++; return true, nil }),
				WithRemovalConfirmer(func(prompt string) (bool, error) { prompted = prompt; return tc.answer, nil }),
			)
			bad := filepath.Join(c.SessionsDir(), "o-r-9-1.json")
			raw := []byte("{not json")
			if err := os.WriteFile(bad, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			report, err := c.Repair(context.Background(), RepairOpts{Record: bad, Apply: true, ForgetIfAbsent: true})
			if err != nil {
				t.Fatalf("set aside: %v", err)
			}
			if postApprovals != 0 {
				t.Errorf("the review-POSTING approver was consulted %d time(s)", postApprovals)
			}
			if !strings.Contains(prompted, "cannot read") {
				t.Errorf("prompt %q does not say the record could not be read", prompted)
			}
			if tc.wantAside {
				if report.Items[0].Outcome != repairOutcomeSetAside {
					t.Fatalf("outcome = %q, want %q", report.Items[0].Outcome, repairOutcomeSetAside)
				}
				assertSetAside(t, c, bad, raw)
				return
			}
			if report.Items[0].Outcome != "declined" {
				t.Fatalf("outcome = %q, want declined", report.Items[0].Outcome)
			}
			if _, serr := os.Stat(bad); serr != nil {
				t.Errorf("a declined set-aside moved the record: %v", serr)
			}
		})
	}
}

// TestRepairForgetIfAbsent_RefusesOnALiveWindow is the plan matrix's other
// forget refusal: a window still exists, so the record names something.
func TestRepairForgetIfAbsent_RefusesOnALiveWindow(t *testing.T) {
	ref := Ref{Owner: "o", Repo: "r", Number: 1}
	name := mustWindowName(t, ref)
	c := repairClient(t, repairRunner(nil, sessionWinRow("forgectl", "$1", name)))
	path := seedPhaseRecord(t, c, ref, PhasePreparing, "")

	_, err := c.Repair(context.Background(), RepairOpts{Record: path, Apply: true, ForgetIfAbsent: true})
	if err == nil {
		t.Fatal("expected a refusal: the review window still exists")
	}
	if !strings.Contains(err.Error(), RepairModeAdoptWindow) {
		t.Errorf("refusal %q does not name the mode that settles a live window", err)
	}
	if _, serr := os.Stat(path); serr != nil {
		t.Errorf("a refusal removed the record: %v", serr)
	}
	if _, serr := os.Stat(filepath.Join(c.SessionsDir(), repairLogName)); serr == nil {
		t.Error("a refusal wrote an intent row — that forges the died-mid-delete signal")
	}
}

// TestRepairRollback_DryRunNeedsNoConfirmationOffATTY: --dry-run mutates
// nothing, so gating the preview on a destructive confirmation refuses the one
// invocation that was always safe.
func TestRepairRollback_DryRunNeedsNoConfirmationOffATTY(t *testing.T) {
	ref := Ref{Owner: "o", Repo: "r", Number: 1}
	c := repairClient(t, repairRunner(nil)) // isTTY false, and no --yes below
	ws := fakeWorkspace(t)
	path := seedPhaseRecord(t, c, ref, PhaseLaunching, ws)

	report, err := c.Repair(context.Background(), RepairOpts{Record: path, Apply: true, Rollback: true, DryRun: true})
	if err != nil {
		t.Fatalf("--dry-run off a TTY should need no --yes: %v", err)
	}
	if len(report.Items) != 1 || report.Items[0].Outcome != "would-rollback" {
		t.Fatalf("report = %+v, want would-rollback", report.Items)
	}
	if _, serr := os.Stat(ws); serr != nil {
		t.Errorf("--dry-run removed the clean room: %v", serr)
	}
}

// TestRepairRollback_RefusesAnUnactionableWorkspaceBeforeTheIntentRow: a
// dangling intent with no completion is the signal that a rollback died
// mid-delete, so a refusal that wrote one would forge it.
func TestRepairRollback_RefusesAnUnactionableWorkspaceBeforeTheIntentRow(t *testing.T) {
	ref := Ref{Owner: "o", Repo: "r", Number: 1}
	c := repairClient(t, repairRunner(nil))
	// An existing directory with no sandbox prefix: neither live nor cleanly
	// absent, so teardown cannot act on it.
	notASandbox := t.TempDir()
	path := seedPhaseRecord(t, c, ref, PhaseLaunching, notASandbox)

	_, err := c.Repair(context.Background(), RepairOpts{Record: path, Apply: true, Rollback: true, Yes: true})
	if err == nil {
		t.Fatal("expected a refusal: the workspace is neither a clean room nor cleanly absent")
	}
	if _, serr := os.Stat(filepath.Join(c.SessionsDir(), repairLogName)); serr == nil {
		t.Error("a refusal wrote an intent row — that forges the died-mid-delete signal")
	}
	if _, serr := os.Stat(path); serr != nil {
		t.Errorf("a refusal removed the record: %v", serr)
	}
}

// fmtBoolPtr renders an observation for a failure message, keeping "unknown"
// distinguishable from "false" — which is the whole point of the pointer.
func fmtBoolPtr(v *bool) string {
	if v == nil {
		return "unknown"
	}
	return strconv.FormatBool(*v)
}

// TestMarshalRepairRow_FitsByConstruction is the reviewer's table. termsafe
// preserves every graphic rune and json.Marshal then doubles `"` and `\\` and
// expands `<`, `>` and `&` to six bytes each — so a byte cap applied before
// encoding measured the wrong thing, and an escape-dense record made its own
// audit row too big. Since beginRepairRow refuses to mutate without a trail,
// that turned the only verb that can clear an unreadable record into one that
// could not, with no forgectl escape at all.
func TestMarshalRepairRow_FitsByConstruction(t *testing.T) {
	for _, fill := range []string{"<", ">", "&", `"`, `\\`, "a", "\u00e9"} {
		t.Run("fill="+fill, func(t *testing.T) {
			raw := []byte(strings.Repeat(fill, 6000))
			row := RepairRow{
				TS: fixedTime(), ID: "abcdef0123456789", Actor: repairActor(),
				Ref: "o/r#1", RecordPath: "/tmp/sessions/o-r-1-1.json",
				FromPhase: repairPhaseUnreadable, Mode: RepairModeForgetIfAbsent,
				Outcome: repairOutcomeIntent,
				Record:  cappedRecordBytes(raw), RecordBytes: len(raw),
			}
			data, err := marshalRepairRow(row)
			if err != nil {
				t.Fatalf("the row size refused the write: %v", err)
			}
			if len(data) > maxRepairLogLineBytes {
				t.Fatalf("encoded row is %d bytes, over the %d limit", len(data), maxRepairLogLineBytes)
			}
			if n := strings.Count(string(data), "\n"); n != 1 || data[len(data)-1] != '\n' {
				t.Fatalf("row is not exactly one newline-terminated line (%d newlines)", n)
			}
			var back RepairRow
			if err := json.Unmarshal(data, &back); err != nil {
				t.Fatalf("row does not parse: %v", err)
			}
			if back.RecordBytes != len(raw) {
				t.Errorf("record_bytes = %d, want the untruncated %d", back.RecordBytes, len(raw))
			}
			// Whenever the payload was shrunk, the row must say so rather than
			// presenting a truncated record as the whole thing.
			if len(back.Record) < len(row.Record) && back.RecordNote == "" {
				t.Error("the record was shrunk with no note saying so")
			}
		})
	}
}

// TestMarshalRepairRow_ElidesRatherThanRefusing pins the floor: a payload that
// cannot fit at any size yields a row with an empty Record and a note, never an
// error that would block the set-aside.
func TestMarshalRepairRow_ElidesRatherThanRefusing(t *testing.T) {
	row := RepairRow{
		TS: fixedTime(), ID: "abcdef0123456789", Actor: repairActor(),
		RecordPath: "/tmp/sessions/o-r-1-1.json", Mode: RepairModeForgetIfAbsent,
		Outcome: repairOutcomeIntent,
		// Every byte expands six-fold, so even one eighth of this will not fit.
		Record: strings.Repeat("<", maxRepairLogLineBytes), RecordBytes: 99999,
	}
	data, err := marshalRepairRow(row)
	if err != nil {
		t.Fatalf("the floor refused a row: %v", err)
	}
	var back RepairRow
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("row does not parse: %v", err)
	}
	if back.RecordNote == "" {
		t.Error("the payload was dropped with no note saying so")
	}
	if back.RecordPath == "" || back.RecordBytes == 0 {
		t.Error("the row lost the fields that name what was acted on")
	}
}

// TestRepairSetAside_AnEscapeDenseRecordStillSettles is finding 6 end to end:
// the record is set aside, and the audit trail survives.
func TestRepairSetAside_AnEscapeDenseRecordStillSettles(t *testing.T) {
	c := repairClient(t, repairRunner(nil))
	bad := filepath.Join(c.SessionsDir(), "o-r-9-1.json")
	raw := []byte("{" + strings.Repeat("<", 6000))
	if err := os.WriteFile(bad, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := c.Repair(context.Background(), RepairOpts{Record: bad, Apply: true, ForgetIfAbsent: true, Yes: true})
	if err != nil {
		t.Fatalf("an escape-dense record must still be settleable: %v", err)
	}
	if report.Items[0].Outcome != repairOutcomeSetAside {
		t.Fatalf("outcome = %q, want %q", report.Items[0].Outcome, repairOutcomeSetAside)
	}
	assertSetAside(t, c, bad, raw)
	rows, err := c.readRepairLog()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("audit rows = %d, want intent + completion", len(rows))
	}
	if rows[0].RecordBytes != len(raw) {
		t.Errorf("record_bytes = %d, want %d", rows[0].RecordBytes, len(raw))
	}
	if rows[0].RecordNote == "" {
		t.Error("the payload was shrunk with no note saying so")
	}
}

// TestMarshalRepairRow_NeverErrorsOnAnyFieldCombination is the contract stated
// in docs/commands/pr.md: the row is never the reason a set-aside fails. Ref is
// the field that reopened it — read best-effort from a record nothing could
// decode, charset-validated but never length-validated — so the table walks
// every combination of oversized variable-length fields, not just the payload.
// The fill is 12000 bytes so a plain-ASCII field overflows the line on its own:
// at 8000 the row only crossed the limit when the actor carried a session id,
// which CI does not set, so the note assertion failed there and nowhere else.
func TestMarshalRepairRow_NeverErrorsOnAnyFieldCombination(t *testing.T) {
	big := func(fill string, n int) string { return strings.Repeat(fill, n) }
	for _, fill := range []string{"<", `"`, "a"} {
		for _, tc := range []struct {
			name string
			row  RepairRow
		}{
			{"oversized ref", RepairRow{Ref: big(fill, 12000)}},
			{"oversized record", RepairRow{Record: big(fill, 12000)}},
			{"oversized error", RepairRow{Error: big(fill, 12000)}},
			{"oversized record path", RepairRow{RecordPath: big(fill, 12000)}},
			{"everything oversized", RepairRow{
				Ref: big(fill, 12000), Record: big(fill, 12000),
				Error: big(fill, 12000), RecordPath: big(fill, 12000),
			}},
		} {
			t.Run(tc.name+"/fill="+fill, func(t *testing.T) {
				row := tc.row
				row.TS, row.ID, row.Actor = fixedTime(), "abcdef0123456789", repairActor()
				row.FromPhase, row.Mode, row.Outcome = repairPhaseUnreadable, RepairModeForgetIfAbsent, repairOutcomeIntent
				data, err := marshalRepairRow(row)
				if err != nil {
					t.Fatalf("the row refused to encode: %v", err)
				}
				if len(data) > maxRepairLogLineBytes {
					t.Fatalf("encoded row is %d bytes, over the %d limit", len(data), maxRepairLogLineBytes)
				}
				if n := strings.Count(string(data), "\n"); n != 1 || data[len(data)-1] != '\n' {
					t.Fatalf("row is not exactly one newline-terminated line (%d newlines)", n)
				}
				var back RepairRow
				if err := json.Unmarshal(data, &back); err != nil {
					t.Fatalf("row does not parse: %v", err)
				}
				if back.RecordNote == "" {
					t.Error("a field was shrunk with no note saying so — a truncated ref reads as a real one")
				}
				if !utf8.ValidString(back.Ref) || !utf8.ValidString(back.Record) {
					t.Error("a cut landed mid-rune")
				}
			})
		}
	}
}

// TestRepairSetAside_ARecordWithAnEnormousRefStillSettles is the reviewer's
// crafted record: a valid but ~7.8 KiB ref that strict decoding rejects. Before
// the fix its intent row could not be written, so the one verb that clears an
// unreadable record refused and every launch stayed blocked.
func TestRepairSetAside_ARecordWithAnEnormousRefStillSettles(t *testing.T) {
	c := repairClient(t, repairRunner(nil))
	// 4000, not 3900. MEASURED: at 3900 the row still fits once the record
	// payload alone is elided, so the pre-fix loop survived it and a test built
	// on that length would pin nothing. At 4000 the row is 8258 bytes with no
	// record at all, which is where shrinking only the payload runs out and the
	// ref has to give.
	huge := strings.Repeat("a", 4000)
	raw := []byte(`{"ref":"` + huge + "/" + huge + `#1","version":9}`)
	if len(raw) > maxBreadcrumbRecordBytes {
		t.Fatalf("setup: the fixture is %d bytes, past the %d record limit", len(raw), maxBreadcrumbRecordBytes)
	}
	bad := filepath.Join(c.SessionsDir(), "o-r-9-1.json")
	if err := os.WriteFile(bad, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	// It really does block launches first.
	if _, err := c.reserve(context.Background(), Ref{Owner: "o", Repo: "r", Number: 2}, 4, PrepareOpts{Agent: "claude"}); err == nil {
		t.Fatal("setup: an unreadable record should block a reservation")
	}

	report, err := c.Repair(context.Background(), RepairOpts{Record: bad, Apply: true, ForgetIfAbsent: true, Yes: true})
	if err != nil {
		t.Fatalf("a record with an enormous ref must still be settleable: %v", err)
	}
	if report.Items[0].Outcome != repairOutcomeSetAside {
		t.Fatalf("outcome = %q, want %q", report.Items[0].Outcome, repairOutcomeSetAside)
	}
	assertSetAside(t, c, bad, raw)
	if _, err := c.reserve(context.Background(), Ref{Owner: "o", Repo: "r", Number: 2}, 4, PrepareOpts{Agent: "claude"}); err != nil {
		t.Errorf("launches are still blocked after the set-aside: %v", err)
	}

	rows, err := c.readRepairLog()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("audit rows = %d, want intent + completion", len(rows))
	}
	for i, r := range rows {
		if r.RecordPath != bad {
			t.Errorf("row %d lost the path that names its subject: %q", i, r.RecordPath)
		}
		// The REF note specifically: a shrunken ref stays charset-valid, so
		// without the note it reads as a real, shorter ref. Asserting on the
		// note's content is also what keeps this test discriminating — a note
		// about the record alone is what the pre-fix code already produced.
		if !strings.Contains(r.RecordNote, "the ref was truncated") {
			t.Errorf("row %d note = %q, want it to name the truncated ref", i, r.RecordNote)
		}
		if fullRef := huge + "/" + huge + "#1"; len(r.Ref) >= len(fullRef) {
			t.Errorf("row %d ref is %d bytes; it was not shrunk from %d", i, len(r.Ref), len(fullRef))
		}
	}
}

// TestCappedRecordBytes_CutsOnARuneBoundary: a split rune would be folded to
// U+FFFD downstream — the one silent byte rewrite this payload exists to avoid.
func TestCappedRecordBytes_CutsOnARuneBoundary(t *testing.T) {
	// Multi-byte runes straddling the cap in both the raw and the clamped cut.
	raw := []byte(strings.Repeat("é", maxAuditRecordBytes))
	got := cappedRecordBytes(raw)
	if !utf8.ValidString(got) {
		t.Fatalf("capped payload is not valid UTF-8: %q", got[max(0, len(got)-8):])
	}
	if len(got) > maxAuditRecordBytes {
		t.Errorf("capped payload is %d bytes, over the %d cap", len(got), maxAuditRecordBytes)
	}
}

// TestRepairSetAside_ARefLessRecordProceedsAndSaysTheCheckDidNotRun is the
// honest-uncertainty case. A torn write is the CANONICAL corrupt record and it
// yields no ref, so the liveness refusal — the one guard that reads the record
// at all — cannot run. Refusing there would refuse exactly the record class
// this verb exists to clear, leaving `rm` as the only escape again. So it
// proceeds, reports the liveness as UNKNOWN rather than as absent, and says so
// in the prompt a human approves.
func TestRepairSetAside_ARefLessRecordProceedsAndSaysTheCheckDidNotRun(t *testing.T) {
	var prompted string
	c := New(repairRunner(nil),
		WithSessionsDir(t.TempDir()), WithFindingsDir(t.TempDir()),
		WithTmuxSession("forgectl"), WithLockWait(2*time.Second),
		WithTTYCheck(func() bool { return true }),
		WithRemovalConfirmer(func(prompt string) (bool, error) { prompted = prompt; return true, nil }),
	)
	bad := filepath.Join(c.SessionsDir(), "o-r-9-1.json")
	// A torn write: the bytes stop mid-record, so no field parses at all.
	raw := []byte(`{"workspace":"/tmp/forgectl-workflow-x","ref":"o/r#`)
	if err := os.WriteFile(bad, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	report, err := c.Repair(context.Background(), RepairOpts{Record: bad, Apply: true, ForgetIfAbsent: true})
	if err != nil {
		t.Fatalf("set aside a torn-write record: %v — a ref-less record must not refuse", err)
	}
	if len(report.Items) != 1 || report.Items[0].Outcome != repairOutcomeSetAside {
		t.Fatalf("report = %+v, want one set-aside item", report.Items)
	}
	// nil, not false: no window was checked, and rendering "no window" would
	// state a fact nobody established.
	if got := report.Items[0].WindowLive; got != nil {
		t.Errorf("WindowLive = %v, want nil — no ref means no liveness check ran", fmtBoolPtr(got))
	}
	if report.Items[0].Ref != "" {
		t.Errorf("Ref = %q, want empty — nothing readable named one", report.Items[0].Ref)
	}
	if !strings.Contains(prompted, "no ref could be read") ||
		!strings.Contains(prompted, "was not checked") {
		t.Errorf("prompt %q does not say the liveness check could not run", prompted)
	}
	assertSetAside(t, c, bad, raw)
}
