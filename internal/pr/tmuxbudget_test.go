package pr

import (
	"context"
	"errors"
	"os"
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
