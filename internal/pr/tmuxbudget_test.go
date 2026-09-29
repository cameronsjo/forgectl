package pr

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/tmux"
)

// shrinkLockedTmuxBudget makes a hung tmux cost milliseconds instead of
// seconds. Tests using it must not run in parallel.
func shrinkLockedTmuxBudget(t *testing.T, d time.Duration) {
	t.Helper()
	old := lockedTmuxBudget
	lockedTmuxBudget = d
	t.Cleanup(func() { lockedTmuxBudget = old })
}

// runBounded runs fn and fails the test if it is still blocked after 5 s — the
// shape of a lock holder waiting on tmux with no deadline.
func runBounded(t *testing.T, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s is still blocked on tmux; the lifecycle lock would be held indefinitely", what)
	}
}

// TestLockedTmuxReads_AreBoundedAndFailClosed is forgectl#656: every tmux read
// made under the lifecycle lock is cut off by lockedTmuxBudget, and the
// timeout lands on the existing fail-closed path ("a window may exist") rather
// than on a granted slot or a destroyed record. Each case would block forever
// against a hung tmux without the bound.
func TestLockedTmuxReads_AreBoundedAndFailClosed(t *testing.T) {
	shrinkLockedTmuxBudget(t, 100*time.Millisecond)
	ref := Ref{Owner: "o", Repo: "r", Number: 31}

	newClient := func(t *testing.T) (*Client, *hangingTmux) {
		h := &hangingTmux{FakeRunner: &exec.FakeRunner{}}
		return New(h, WithSessionsDir(t.TempDir()), WithFindingsDir(t.TempDir()),
			WithTmuxSession("forgectl"),
			WithApprover(func(string) (bool, error) { return false, nil }),
			WithTTYCheck(func() bool { return false })), h
	}

	t.Run("admit counts occupancy", func(t *testing.T) {
		c, _ := newClient(t)
		var ok bool
		runBounded(t, "Admit", func() { _, _, _, ok = c.Admit(context.Background(), 4) })
		if ok {
			t.Error("Admit reported free slots from a tmux that never answered")
		}
	})

	t.Run("reserve counts occupancy", func(t *testing.T) {
		c, _ := newClient(t)
		var err error
		runBounded(t, "reserve", func() { _, err = c.reserve(context.Background(), ref, 4, PrepareOpts{Agent: "claude"}) })
		if err == nil {
			t.Error("reserve granted a slot from a tmux that never answered")
		}
	})

	t.Run("repair inspect reads liveness", func(t *testing.T) {
		c, _ := newClient(t)
		seedPhaseRecord(t, c, ref, PhaseLaunching, fakeWorkspace(t))
		var report RepairReport
		var err error
		runBounded(t, "Repair inspect", func() { report, err = c.Repair(context.Background(), RepairOpts{}) })
		if err != nil {
			t.Fatalf("Repair inspect: %v", err)
		}
		if len(report.Items) != 1 || report.Items[0].WindowLive != nil {
			t.Errorf("items = %+v, want one row whose window liveness is unknown (nil)", report.Items)
		}
	})

	t.Run("repair rollback reads liveness", func(t *testing.T) {
		c, _ := newClient(t)
		ws := fakeWorkspace(t)
		path := seedPhaseRecord(t, c, ref, PhaseLaunching, ws)
		var err error
		runBounded(t, "Repair rollback", func() {
			_, err = c.Repair(context.Background(), RepairOpts{Record: path, Apply: true, Rollback: true, Yes: true})
		})
		if err == nil {
			t.Error("rollback went ahead on a window list it could not read")
		}
		if got := readRecord(t, path).Phase; got != PhaseLaunching {
			t.Errorf("a refused rollback moved the record to %q", got)
		}
		if _, serr := os.Stat(ws); serr != nil {
			t.Errorf("a refused rollback removed the workspace: %v", serr)
		}
	})

	t.Run("repair adopt resolves the window", func(t *testing.T) {
		c, _ := newClient(t)
		path := seedPhaseRecord(t, c, ref, PhaseLaunching, fakeWorkspace(t))
		var err error
		runBounded(t, "Repair adopt", func() {
			_, err = c.Repair(context.Background(), RepairOpts{Record: path, Apply: true, AdoptWindow: true})
		})
		if err == nil {
			t.Error("adopt went ahead on a window it could not resolve")
		}
		if got := readRecord(t, path).Phase; got != PhaseLaunching {
			t.Errorf("a refused adopt moved the record to %q", got)
		}
	})
}

// TestTeardown_DuplicateReviewWindowNamesParkAndKillNothing is the teardown
// half of forgectl#656: two windows in the review session carry the review's
// name, so resolution refuses rather than returning the first. Teardown must
// treat that refusal as "a window is live", not "nothing to kill": it kills
// neither window, parks the record, and removes nothing.
func TestTeardown_DuplicateReviewWindowNamesParkAndKillNothing(t *testing.T) {
	ref := Ref{Owner: "o", Repo: "r", Number: 32}
	name := mustWindowName(t, ref)
	fake := reviewServer(name, name)
	c := New(fake, WithSessionsDir(t.TempDir()), WithFindingsDir(t.TempDir()),
		WithApprover(func(string) (bool, error) { return false, nil }),
		WithTTYCheck(func() bool { return false }))
	ws := fakeWorkspace(t)
	path := seedPhaseRecord(t, c, ref, PhasePrepared, ws)

	err := c.Teardown(context.Background(), path)
	if !errors.Is(err, tmux.ErrAmbiguousWindow) {
		t.Fatalf("Teardown err = %v, want tmux.ErrAmbiguousWindow", err)
	}
	if errors.Is(err, ErrRecordNotParked) {
		t.Errorf("a v2 record should have been parked: %v", err)
	}
	if call, ok := findCallVerb(fake.Calls, "tmux", "kill-window"); ok {
		t.Errorf("teardown killed a window it could not tell apart from its twin: %+v", call)
	}
	if _, serr := os.Stat(ws); serr != nil {
		t.Errorf("the workspace must be kept while a same-named window is live: %v", serr)
	}
	bc := readRecord(t, path)
	if bc.Phase != PhaseNeedsRepair || bc.RepairReason != windowAmbiguousReason {
		t.Errorf("record = phase %q reason %q, want needs-repair with the ambiguity reason", bc.Phase, bc.RepairReason)
	}
}

// seedCleanupSweep seeds n live prepared records plus one stale record (its
// workspace already gone), all created today, and returns the live paths and
// workspaces and the stale path.
func seedCleanupSweep(t *testing.T, c *Client, n int) (live, wss []string, stale string) {
	t.Helper()
	for i := 0; i < n; i++ {
		ws := fakeWorkspace(t)
		live = append(live, seedPhaseRecord(t, c, Ref{Owner: "o", Repo: "r", Number: 41 + i}, PhasePrepared, ws))
		wss = append(wss, ws)
	}
	gone := fakeWorkspace(t)
	stale = seedPhaseRecord(t, c, Ref{Owner: "o", Repo: "r", Number: 40}, PhasePrepared, gone)
	if err := os.RemoveAll(gone); err != nil {
		t.Fatal(err)
	}
	return live, wss, stale
}

// TestCleanup_HungTmuxSharesOneBudgetAndSkipsTheRest is forgectl#648: a sweep
// holds the lifecycle lock throughout, so it shares ONE tmux budget rather
// than spending one per session. After the first timeout, the remaining live
// sessions are skipped untouched (not parked, not asked of tmux at all), while
// a stale session, which never calls tmux, is still settled.
func TestCleanup_HungTmuxSharesOneBudgetAndSkipsTheRest(t *testing.T) {
	shrinkLockedTmuxBudget(t, 150*time.Millisecond)
	h := &hangingTmux{FakeRunner: reviewServer()}
	c := New(h, WithSessionsDir(t.TempDir()), WithFindingsDir(t.TempDir()),
		WithApprover(func(string) (bool, error) { return false, nil }),
		WithTTYCheck(func() bool { return false }))
	live, wss, stale := seedCleanupSweep(t, c, 3)

	var err error
	runBounded(t, "Cleanup", func() { _, err = c.Cleanup(context.Background(), time.Now().UTC().Format("2006-01-02")) })
	if !errors.Is(err, ErrWindowKillTimedOut) {
		t.Fatalf("Cleanup err = %v, want the first session's ErrWindowKillTimedOut", err)
	}
	parked := 0
	for i, path := range live {
		switch got := readRecord(t, path).Phase; got {
		case PhaseNeedsRepair:
			parked++
		case PhasePrepared:
		default:
			t.Errorf("%s: phase %q, want parked (the one that timed out) or untouched", path, got)
		}
		if _, serr := os.Stat(wss[i]); serr != nil {
			t.Errorf("workspace %s was removed while tmux was unresponsive: %v", wss[i], serr)
		}
	}
	if parked != 1 {
		t.Errorf("%d sessions were attempted against a hung tmux, want 1: the rest must be skipped", parked)
	}
	asked := 0
	for _, call := range h.Calls {
		if call.Name == "tmux" && len(call.Args) > 0 && call.Args[0] == "list-sessions" {
			asked++
		}
	}
	if asked != 1 {
		t.Errorf("tmux was asked to resolve the review session %d times, want 1", asked)
	}
	if _, serr := os.Stat(stale); !os.IsNotExist(serr) {
		t.Errorf("the stale record needs no tmux and should still be swept: %v", serr)
	}
}

// TestCleanup_BudgetChargesOnlyTmuxTime: the shared budget is tmux time, not
// wall-clock time. A sweep whose local removals alone outlast the budget must
// still tear down every healthy session.
func TestCleanup_BudgetChargesOnlyTmuxTime(t *testing.T) {
	shrinkLockedTmuxBudget(t, 50*time.Millisecond)
	orig := sandboxTeardown
	sandboxTeardown = func(ctx context.Context, run exec.Runner, workspace string) error {
		time.Sleep(80 * time.Millisecond) // slower than the whole tmux budget
		return orig(ctx, run, workspace)
	}
	t.Cleanup(func() { sandboxTeardown = orig })
	refs := []Ref{{Owner: "o", Repo: "r", Number: 41}, {Owner: "o", Repo: "r", Number: 42}}
	c := New(reviewServer(mustWindowName(t, refs[0]), mustWindowName(t, refs[1])),
		WithSessionsDir(t.TempDir()), WithFindingsDir(t.TempDir()),
		WithApprover(func(string) (bool, error) { return false, nil }),
		WithTTYCheck(func() bool { return false }))
	live, _, _ := seedCleanupSweep(t, c, 2)

	if _, err := c.Cleanup(context.Background(), time.Now().UTC().Format("2006-01-02")); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	for _, path := range live {
		if _, serr := os.Stat(path); !os.IsNotExist(serr) {
			t.Errorf("%s survived a healthy sweep: %v", path, serr)
		}
	}
}

// TestTeardown_TimedOutKillNamesTheWindow is the other half of forgectl#648: a
// kill that timed out must leave the window recorded somewhere. The schema
// allows a windowId only on an active record, so the park names the window in
// its reason, and the error names it too, which is what reaches the operator
// when the record cannot be parked at all.
func TestTeardown_TimedOutKillNamesTheWindow(t *testing.T) {
	shrinkLockedTmuxBudget(t, 100*time.Millisecond)
	for _, tc := range []struct {
		name   string
		legacy bool
	}{{"v2 record parks with the window", false}, {"legacy record names it in the error", true}} {
		t.Run(tc.name, func(t *testing.T) {
			ref := Ref{Owner: "o", Repo: "r", Number: 45}
			name := mustWindowName(t, ref)
			h := &hangingTmux{FakeRunner: reviewServer(name), blockVerb: "kill-window"}
			c := New(h, WithSessionsDir(t.TempDir()), WithFindingsDir(t.TempDir()),
				WithApprover(func(string) (bool, error) { return false, nil }),
				WithTTYCheck(func() bool { return false }))
			var path string
			if tc.legacy {
				path, _ = seedSession(t, c, ref, time.Now().UTC())
			} else {
				path = seedPhaseRecord(t, c, ref, PhasePrepared, fakeWorkspace(t))
			}
			want := name + " (@5)"
			err := c.Teardown(context.Background(), path)
			if !errors.Is(err, ErrWindowKillTimedOut) || !strings.Contains(err.Error(), want) {
				t.Fatalf("err = %v, want ErrWindowKillTimedOut naming %q", err, want)
			}
			if tc.legacy {
				return
			}
			if reason := readRecord(t, path).RepairReason; !strings.Contains(reason, want) {
				t.Errorf("parked reason %q does not name the window %q", reason, want)
			}
		})
	}
}
