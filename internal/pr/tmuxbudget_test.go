package pr

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/config"
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

	t.Run("drain claim counts occupancy", func(t *testing.T) {
		c, _ := newClient(t)
		path := seedPhaseRecord(t, c, ref, PhaseQueued, "")
		var report DrainReport
		var claimed []SessionSummary
		runBounded(t, "drain claim", func() { report, claimed = c.claimQueuedPass(context.Background(), config.Config{}, DrainOpts{}) })
		if report.Refusal == "" || len(claimed) != 0 {
			t.Errorf("refusal %q, claimed %d: a drain pass must refuse, claiming nothing, when tmux never answered",
				report.Refusal, len(claimed))
		}
		if got := readRecord(t, path).Phase; got != PhaseQueued {
			t.Errorf("a refused claim moved the record to %q", got)
		}
	})

	t.Run("prune screens live windows", func(t *testing.T) {
		c, _ := newClient(t)
		future := []byte(`{"workspace":"/tmp/forgectl-workflow-x","ref":"o/r#31","agent":"claude",` +
			`"createdAt":"2026-09-12T00:00:00Z","version":3,"phase":"active","revision":4}` + "\n")
		aside := seedAside(t, c, "o-r-31-1.json", 60*24*time.Hour, future)
		var report PruneReport
		var err error
		runBounded(t, "Prune", func() { report, err = c.Prune(context.Background(), defaultPruneOpts()) })
		if err != nil {
			t.Fatalf("Prune: %v", err)
		}
		if len(report.Items) != 1 || report.Items[0].Outcome != pruneOutcomeRefused {
			t.Errorf("items = %+v, want the ref-bearing file refused: an unanswered window list is not an absent window", report.Items)
		}
		if _, serr := os.Stat(aside); serr != nil {
			t.Errorf("a refused set-aside file was removed: %v", serr)
		}
	})
}

// TestTeardown_DuplicateReviewWindowNamesParkAndKillNothing is the teardown
// half of forgectl#656: two windows in the review session carry the review's
// name, so resolution refuses rather than returning the first. Teardown must
// treat that refusal as "a window is live", not "nothing to kill": it kills
// none of the windows, parks the record, and removes nothing.
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

// testClock is a manual clock behind budgetTimeout. A bounded context expires
// only when Advance moves the clock to or past its deadline, never on wall
// time, and Advance cancels every context it expires before returning. So a
// caller that checks ctx.Err() right after advancing sees an exact verdict
// (DeadlineExceeded, as from a real deadline; see clockCtx),
// however loaded the machine is (forgectl#757).
type testClock struct {
	mu      sync.Mutex
	now     time.Duration
	nextID  int
	pending map[int]clockDeadline
}

type clockDeadline struct {
	at     time.Duration
	expire context.CancelCauseFunc
}

// useTestClock routes every tmux budget through a fresh testClock for the rest
// of the test. Tests using it must not run in parallel.
func useTestClock(t *testing.T) *testClock {
	t.Helper()
	clk := &testClock{pending: make(map[int]clockDeadline)}
	old := budgetTimeout
	budgetTimeout = clk.withTimeout
	t.Cleanup(func() { budgetTimeout = old })
	return clk
}

func (c *testClock) withTimeout(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancelCause(parent)
	c.mu.Lock()
	id := c.nextID
	c.nextID++
	c.pending[id] = clockDeadline{at: c.now + d, expire: cancel}
	c.mu.Unlock()
	return clockCtx{ctx}, func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		cancel(context.Canceled)
	}
}

// clockCtx is a testClock context. It reports the same Err a real
// context.WithTimeout does: DeadlineExceeded once Advance expired it, and
// Canceled once its cancel func ran first (forgectl#791). Underneath it is a
// WithCancelCause context, whose own Err is Canceled either way; the expiry
// is carried as its cause.
type clockCtx struct{ context.Context }

func (c clockCtx) Err() error {
	err := c.Context.Err()
	if err != nil && errors.Is(context.Cause(c.Context), context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return err
}

// Mutation that turns it red: return the bare WithCancelCause context from
// testClock.withTimeout instead of wrapping it in clockCtx (an expired
// context then reports Canceled).
func TestTestClock_ErrMatchesARealDeadline(t *testing.T) {
	clk := &testClock{pending: make(map[int]clockDeadline)}

	expired, cancelExpired := clk.withTimeout(context.Background(), time.Second)
	defer cancelExpired()
	clk.Advance(time.Second)
	if err := expired.Err(); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("expired context Err = %v, want context.DeadlineExceeded", err)
	}

	cancelled, cancelFirst := clk.withTimeout(context.Background(), time.Second)
	cancelFirst()
	if err := cancelled.Err(); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled context Err = %v, want context.Canceled", err)
	}
}

// Advance moves the clock forward by d and expires every deadline it reaches.
func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now += d
	var due []context.CancelCauseFunc
	for id, dl := range c.pending {
		if dl.at <= c.now {
			due = append(due, dl.expire)
			delete(c.pending, id)
		}
	}
	c.mu.Unlock()
	for _, expire := range due {
		expire(context.DeadlineExceeded)
	}
}

// Now reports how far the clock has been advanced.
func (c *testClock) Now() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// slowTmux answers every tmux call correctly, but only after delay on the
// test clock: a loaded machine, not a hung server. The delay is spent by
// advancing the clock before the call answers, so a call that pushes its unit
// past the bound sees its context expired, exactly as a real deadline would
// cut it off. No wall time passes. Everything else delegates.
type slowTmux struct {
	*exec.FakeRunner
	clock *testClock
	delay time.Duration
}

func (s *slowTmux) Run(ctx context.Context, name string, args ...string) (string, error) {
	if name == "tmux" {
		s.clock.Advance(s.delay)
		if err := ctx.Err(); err != nil {
			return "", err
		}
	}
	return s.FakeRunner.Run(ctx, name, args...)
}

// TestCleanup_ASlowButHealthyTmuxDiscardsEverySession: the sweep's shared
// budget detects an UNRESPONSIVE tmux; it must not cap the sweep's total tmux
// time. Every call here answers well inside the bound, but the sweep's tmux
// time adds up to several bounds, so a budget that accumulated would run dry
// partway, park the next session as "unresponsive", and skip the rest.
//
// The delays run on a test clock, not the wall clock, so "slow but inside the
// bound" is exact: before forgectl#757 this slept for real and a loaded runner
// could stretch one unit past the bound.
//
// Mutations that turn it red: make tmuxBudget.bound derive ONE deadline for
// the whole sweep and hand it to every unit (the accumulating budget), or set
// cutOff whether or not the unit's context ended.
func TestCleanup_ASlowButHealthyTmuxDiscardsEverySession(t *testing.T) {
	const bound = 300 * time.Millisecond
	shrinkLockedTmuxBudget(t, bound)
	clk := useTestClock(t)
	const n = 8
	names := make([]string, 0, n)
	for i := 0; i < n; i++ {
		names = append(names, mustWindowName(t, Ref{Owner: "o", Repo: "r", Number: 41 + i}))
	}
	s := &slowTmux{FakeRunner: reviewServer(names...), clock: clk, delay: bound / 10}
	c := New(s, WithSessionsDir(t.TempDir()), WithFindingsDir(t.TempDir()),
		WithApprover(func(string) (bool, error) { return false, nil }),
		WithTTYCheck(func() bool { return false }))
	live, _, _ := seedCleanupSweep(t, c, n)

	report, err := c.Cleanup(context.Background(), time.Now().UTC().Format("2006-01-02"))
	if err != nil {
		t.Fatalf("Cleanup: %v (failed %+v)", err, report.Failed)
	}
	if report.Discarded != n+1 || len(report.Failed) != 0 {
		t.Errorf("discarded %d, failed %d; want all %d live sessions and the stale one discarded, none parked or skipped",
			report.Discarded, len(report.Failed), n)
	}
	for _, path := range live {
		if _, serr := os.Stat(path); !os.IsNotExist(serr) {
			t.Errorf("%s survived a healthy sweep: %v", path, serr)
		}
	}
	// The premise the test rests on: the sweep's tmux time adds up to several
	// bounds, so an accumulating budget could not have passed it.
	if spent := clk.Now(); spent <= 2*bound {
		t.Errorf("the sweep spent %v of tmux time; want more than two bounds (%v) or the test cannot tell a per-unit budget from an accumulating one",
			spent, 2*bound)
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

// TestAttach_DuplicateReviewWindowNamesSayWhatToDo: attach refuses a review
// name more than one window carries, and says which step settles it.
func TestAttach_DuplicateReviewWindowNamesSayWhatToDo(t *testing.T) {
	ref := Ref{Owner: "o", Repo: "r", Number: 33}
	name := mustWindowName(t, ref)
	fake := reviewServer(name, name)
	c := testClient(t, fake)
	path, _ := seedSession(t, c, ref, time.Now().UTC())

	err := c.Attach(context.Background(), path)
	if !errors.Is(err, tmux.ErrAmbiguousWindow) || !strings.Contains(err.Error(), "more than one window carries this review's name") ||
		!strings.Contains(err.Error(), "forgectl pr repair") {
		t.Fatalf("Attach err = %v, want the duplicate-name refusal pointing at pr repair", err)
	}
	if _, ok := findCallVerb(fake.Calls, "tmux", "select-window"); ok {
		t.Error("attach selected a window it could not tell apart from its twin")
	}
}
