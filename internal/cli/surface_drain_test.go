//go:build unix

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	osexec "os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/herdr/ready"
	"github.com/cameronsjo/forgectl/internal/launch"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/procstart"
	"github.com/cameronsjo/forgectl/internal/surface"
	"github.com/cameronsjo/forgectl/internal/surface/backend"
	"github.com/cameronsjo/forgectl/internal/surface/drain"
	"github.com/cameronsjo/forgectl/internal/surface/herdradapter"
	"github.com/cameronsjo/forgectl/internal/surface/merge"
	"github.com/cameronsjo/forgectl/internal/surface/worker"
)

const drainTestSession = "s1"

var drainT0 = time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)

// fakeDrain is a drain's world with no herdr, git or GitHub: the queue and
// ledgers are real files under a temp XDG_STATE_HOME, the rest are fakes.
type fakeDrain struct {
	now      time.Time
	cfg      config.Config
	cfgErr   error
	herdrErr error
	launch   func(row worker.QueueRow) drain.Attempt
	probe    func(q worker.QueueRow, led worker.Row) drain.Probe
	launched []string
	notified []string
	cleared  []string
	// notifyErr and clearErr are what NeedsYou and Cleared return.
	notifyErr error
	clearErr  error
	events    []drain.Event
	ids       int
	t         *testing.T
	// prs answers the closers' PR reads by row name; a missing name has no
	// PR. prErr, when set for a name, is the read's error. prReads counts
	// reads by name.
	prs     map[string]merge.Candidate
	prErr   map[string]error
	prReads map[string]int
	// usage is what pricing a transcript returns. onPrice, when set, runs
	// while pricing: the window between the closer's read and its close.
	usage   *statusUsage
	onPrice func()
	// closeRes, when set, is what closeRow returns; otherwise the close
	// succeeds and removes the ledger row. closed lists the rows closed.
	closeRes *closeResult
	closed   []string
	// pruneRuns counts daily prunes; pruneDay is the recorded day.
	pruneRuns int
	pruneDay  string
	pruneErr  error
	// pruneDayErr is what reading the recorded day returns.
	pruneDayErr error
}

// checkOwnLedger fails the test when the drain hands the notifier a ledger
// row other than the queue row's own: a different row's ref would mark
// another worker's pane.
func (f *fakeDrain) checkOwnLedger(row worker.QueueRow, led worker.Row) {
	if led.Name != "" && (led.Name != row.Name || led.LaunchID != row.LaunchID) {
		f.t.Errorf("notifier for %s got ledger row %s (launch %q), want its own (launch %q)", row.Name, led.Name, led.LaunchID, row.LaunchID)
	}
}

func (f *fakeDrain) NeedsYou(_ context.Context, row worker.QueueRow, led worker.Row, reason string) error {
	f.checkOwnLedger(row, led)
	f.notified = append(f.notified, row.Name+": "+reason)
	return f.notifyErr
}

func (f *fakeDrain) Cleared(_ context.Context, row worker.QueueRow, led worker.Row) error {
	f.checkOwnLedger(row, led)
	f.cleared = append(f.cleared, row.Name)
	return f.clearErr
}

func newFakeDrain(t *testing.T) (*fakeDrain, *drainer, *worker.Queue) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	q, err := worker.OpenQueue()
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeDrain{now: drainT0, t: t, prs: map[string]merge.Candidate{}, prErr: map[string]error{}, prReads: map[string]int{}}
	f.launch = func(worker.QueueRow) drain.Attempt { return drain.Attempt{} }
	f.probe = func(worker.QueueRow, worker.Row) drain.Probe { return drain.Probe{} }
	io := drainIO{
		queue: q,
		ledgerRows: func(repo, s string) ([]worker.Row, error) {
			led, err := worker.Open(repo, s)
			if err != nil {
				return nil, err
			}
			return led.Rows()
		},
		prober: func() drainProber {
			return func(_ context.Context, q worker.QueueRow, led worker.Row) drain.Probe { return f.probe(q, led) }
		},
		launch: func(_ context.Context, _ config.Config, row worker.QueueRow) drain.Attempt {
			f.launched = append(f.launched, row.Name)
			return f.launch(row)
		},
		herdrReady: func(context.Context) error { return f.herdrErr },
		load:       func() (config.Config, error) { return f.cfg, f.cfgErr },
		notify:     f,
		emit:       func(e drain.Event) error { f.events = append(f.events, e); return nil },
		now:        func() time.Time { return f.now },
		launchID: func() (string, error) {
			f.ids++
			return fmt.Sprintf("launch-%d", f.ids), nil
		},
		prRead: func(_ context.Context, row merge.Row) (merge.Candidate, error) {
			f.prReads[row.Name]++
			if err := f.prErr[row.Name]; err != nil {
				return merge.Candidate{}, err
			}
			c, ok := f.prs[row.Name]
			if !ok {
				return merge.Candidate{}, merge.ErrNoPR
			}
			return c, nil
		},
		price: func(context.Context, string) *statusUsage {
			if f.onPrice != nil {
				f.onPrice()
			}
			return f.usage
		},
		closeRow: func(_ context.Context, q worker.QueueRow, led worker.Row) closeResult {
			if f.closeRes != nil {
				return *f.closeRes
			}
			f.closed = append(f.closed, q.Name)
			l, err := worker.Open(q.Repo, drainTestSession)
			if err != nil {
				t.Fatal(err)
			}
			if err := l.RemoveIf(led.Name, worker.SameRow(led)); err != nil {
				t.Fatal(err)
			}
			return closeResult{Name: q.Name, Closed: true, Workspace: closeWorkspaceClosed, Worktree: closeWorktreeRemoved, Forgotten: true}
		},
		prune: func(context.Context, time.Time) (pruneResult, error) {
			f.pruneRuns++
			return pruneResult{}, f.pruneErr
		},
		pruneDay:    func() (string, error) { return f.pruneDay, f.pruneDayErr },
		setPruneDay: func(day string) error { f.pruneDay = day; return nil },
	}
	return f, newDrainer(io, drainTestSession, func() bool { return false }), q
}

func enqueueAt(t *testing.T, q *worker.Queue, name, repo string, at time.Time) {
	t.Helper()
	if _, _, err := q.Enqueue(name, repo, "brief for "+name, "", at); err != nil {
		t.Fatal(err)
	}
}

func rowNamed(t *testing.T, q *worker.Queue, name string) worker.QueueRow {
	t.Helper()
	rows, err := q.Rows()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("no queue row %q", name)
	return worker.QueueRow{}
}

// seedLedger writes a ledger row for name at stage, carrying launchID.
func seedLedger(t *testing.T, repo, name, launchID string, stage worker.Stage, fill func(*worker.Row)) {
	t.Helper()
	led, err := worker.Open(repo, drainTestSession)
	if err != nil {
		t.Fatal(err)
	}
	if err := led.Begin(worker.Row{Name: name, Branch: drain.Branch(name), StartedAt: drainT0, LaunchID: launchID}); err != nil {
		t.Fatal(err)
	}
	if err := led.Update(name, func(r *worker.Row) {
		r.Stage = stage
		if fill != nil {
			fill(r)
		}
	}); err != nil {
		t.Fatal(err)
	}
}

// seedState puts a queued row into state with a launch id, as the drain
// would have, and returns its launch id.
func seedState(t *testing.T, q *worker.Queue, name string, state worker.QueueState) string {
	t.Helper()
	id := "launch-seed-" + name
	r, err := q.ClaimFor(name, id, drainTestSession, drainT0)
	if err != nil {
		t.Fatal(err)
	}
	if state == worker.QueueClaimed {
		return id
	}
	if _, err := q.UpdateIf(name, worker.SameRead(r), drainT0, func(r *worker.QueueRow) { r.State = state }); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestDrainTickCaps(t *testing.T) {
	f, d, q := newFakeDrain(t)
	enqueueAt(t, q, "held", "/repo/c", drainT0.Add(-time.Hour))
	id := seedState(t, q, "held", worker.QueueFailed)
	seedLedger(t, "/repo/c", "held", id, worker.StageLaunched, nil)
	enqueueAt(t, q, "a1", "/repo/a", drainT0.Add(1*time.Minute))
	enqueueAt(t, q, "a2", "/repo/a", drainT0.Add(2*time.Minute))
	enqueueAt(t, q, "b1", "/repo/b", drainT0.Add(3*time.Minute))
	enqueueAt(t, q, "c1", "/repo/c", drainT0.Add(4*time.Minute))
	enqueueAt(t, q, "d1", "/repo/d", drainT0.Add(5*time.Minute))
	d.tick(t.Context())
	// cap 3: the failed row with a live ledger row holds one slot (and its
	// repo's), so a1 and b1 launch; a2 waits on per_repo, c1 on held, d1 on
	// the cap.
	if got := strings.Join(f.launched, ","); got != "a1,b1" {
		t.Fatalf("launched %q, want a1,b1", got)
	}
	for name, want := range map[string]worker.QueueState{"a1": worker.QueueLaunched, "b1": worker.QueueLaunched, "a2": worker.QueueQueued, "c1": worker.QueueQueued, "d1": worker.QueueQueued, "held": worker.QueueFailed} {
		if got := rowNamed(t, q, name).State; got != want {
			t.Errorf("%s is %s, want %s", name, got, want)
		}
	}
	if r := rowNamed(t, q, "a1"); r.Session != drainTestSession || r.LaunchID == "" {
		t.Errorf("a launched row must record the session and its launch id: %+v", r)
	}
}

func TestDrainTickRetryRule(t *testing.T) {
	f, d, q := newFakeDrain(t)
	enqueueAt(t, q, "w", "/repo/a", drainT0)
	f.launch = func(worker.QueueRow) drain.Attempt {
		return drain.Attempt{Class: drain.ErrOther, Err: "base lookup failed", CreatedNothing: true}
	}
	for i, want := range []worker.QueueState{worker.QueueQueued, worker.QueueQueued, worker.QueueFailed} {
		d.tick(t.Context())
		r := rowNamed(t, q, "w")
		if r.State != want || r.Attempts != i+1 {
			t.Fatalf("after failure %d: %s attempts %d, want %s attempts %d", i+1, r.State, r.Attempts, want, i+1)
		}
		if want == worker.QueueQueued && r.LaunchID != "" {
			t.Errorf("a requeued row kept launch id %q", r.LaunchID)
		}
	}
	d.tick(t.Context())
	if len(f.launched) != 3 {
		t.Fatalf("launched %d times, want 3 (the cap)", len(f.launched))
	}
}

func TestDrainTickFailsAtOnce(t *testing.T) {
	cases := map[string]struct {
		a     drain.Attempt
		inErr string
	}{
		"created a worktree": {drain.Attempt{Class: drain.ErrOther, Err: "workspace create failed", Worktree: "/repo/a/.claude/worktrees/w",
			Row: &worker.Row{Stage: worker.StageFailed, Worktree: "/repo/a/.claude/worktrees/w"}}, "/repo/a/.claude/worktrees/w"},
		"name taken": {drain.Attempt{Class: drain.ErrNameTaken, Err: worker.ErrNameTaken.Error(), CreatedNothing: true}, "already exists"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f, d, q := newFakeDrain(t)
			enqueueAt(t, q, "w", "/repo/a", drainT0)
			f.launch = func(row worker.QueueRow) drain.Attempt {
				if c.a.Row != nil {
					// What a real attempt leaves: its own failed row naming the worktree.
					seedLedger(t, row.Repo, row.Name, row.LaunchID, worker.StageFailed, func(r *worker.Row) { r.Worktree = c.a.Worktree })
				}
				return c.a
			}
			d.tick(t.Context())
			d.tick(t.Context())
			r := rowNamed(t, q, "w")
			if r.State != worker.QueueFailed || r.Attempts != 0 || !strings.Contains(r.LastError, c.inErr) {
				t.Fatalf("row %s attempts %d error %q; want failed, 0 attempts, naming %q", r.State, r.Attempts, r.LastError, c.inErr)
			}
			if len(f.launched) != 1 {
				t.Fatalf("launched %d times; a failed row must not be retried", len(f.launched))
			}
		})
	}
}

func TestDrainTickPauses(t *testing.T) {
	t.Run("github auth: requeued, no attempt, paused until restart", func(t *testing.T) {
		f, d, q := newFakeDrain(t)
		enqueueAt(t, q, "w", "/repo/a", drainT0)
		enqueueAt(t, q, "x", "/repo/b", drainT0.Add(time.Minute))
		f.launch = func(worker.QueueRow) drain.Attempt {
			return drain.Attempt{Class: drain.ErrGitHubAuth, Err: "HTTP 401", CreatedNothing: true}
		}
		d.tick(t.Context())
		r := rowNamed(t, q, "w")
		if r.State != worker.QueueQueued || r.Attempts != 0 {
			t.Fatalf("row %s attempts %d; want queued with no attempt", r.State, r.Attempts)
		}
		if len(f.launched) != 1 {
			t.Fatalf("launched %v; the pause must stop the second claim in the same tick", f.launched)
		}
		d.tick(t.Context())
		if len(f.launched) != 1 || !strings.Contains(d.pauses.Reason(), "HTTP 401") {
			t.Fatalf("auth pause did not hold: launched %v, pause %q", f.launched, d.pauses.Reason())
		}
		if st := d.status(drain.Status{}, nil); st.Status != drain.StatusPaused {
			t.Fatalf("status %s, want paused", st.Status)
		}
	})
	t.Run("github unreadable: requeued, no attempt, one probe a tick, cleared by a launch", func(t *testing.T) {
		f, d, q := newFakeDrain(t)
		enqueueAt(t, q, "w", "/repo/a", drainT0)
		enqueueAt(t, q, "x", "/repo/b", drainT0.Add(time.Minute))
		down := true
		f.launch = func(worker.QueueRow) drain.Attempt {
			if down {
				return drain.Attempt{Class: drain.ErrGitHubRead, Err: "HTTP 502", CreatedNothing: true}
			}
			return drain.Attempt{}
		}
		for tick := 1; tick <= 3; tick++ {
			d.tick(t.Context())
			if len(f.launched) != tick {
				t.Fatalf("tick %d: launched %v; want one probe per tick while GitHub is unreadable", tick, f.launched)
			}
			for _, name := range []string{"w", "x"} {
				if r := rowNamed(t, q, name); r.State != worker.QueueQueued || r.Attempts != 0 {
					t.Fatalf("tick %d: row %s is %s with %d attempts; want queued, no attempt spent", tick, name, r.State, r.Attempts)
				}
			}
			if !strings.Contains(d.pauses.Reason(), "GitHub could not be read: HTTP 502") {
				t.Fatalf("tick %d: pause %q", tick, d.pauses.Reason())
			}
		}
		down = false
		d.tick(t.Context())
		if d.pauses.Paused() {
			t.Fatalf("GitHub back: pause %q still held", d.pauses.Reason())
		}
		if r := rowNamed(t, q, "w"); r.State != worker.QueueLaunched {
			t.Fatalf("GitHub back: row w is %s", r.State)
		}
	})
	t.Run("an identity read failure classifies as GitHub unreadable", func(t *testing.T) {
		err := fmt.Errorf("forgectl: read o/r's repository name and id from GitHub, which a worker launch records: %w: %w", errIdentityRead, errors.New("HTTP 502"))
		if got := classifyLaunchError(err); got != drain.ErrGitHubRead {
			t.Fatalf("class %v, want ErrGitHubRead", got)
		}
	})
	t.Run("herdr down: no claim, retried each tick", func(t *testing.T) {
		f, d, q := newFakeDrain(t)
		enqueueAt(t, q, "w", "/repo/a", drainT0)
		f.herdrErr = errors.New("herdr session not running")
		d.tick(t.Context())
		if r := rowNamed(t, q, "w"); r.State != worker.QueueQueued || r.Attempts != 0 || len(f.launched) != 0 {
			t.Fatalf("herdr down: row %+v launched %v", r, f.launched)
		}
		f.herdrErr = nil
		d.tick(t.Context())
		if r := rowNamed(t, q, "w"); r.State != worker.QueueLaunched || d.pauses.Paused() {
			t.Fatalf("herdr back: row %s, pause %q", r.State, d.pauses.Reason())
		}
		kinds := []string{}
		for _, e := range f.events {
			if e.Kind == drain.EventPause || e.Kind == drain.EventResume {
				kinds = append(kinds, e.Kind)
			}
		}
		if strings.Join(kinds, ",") != "pause,resume" {
			t.Fatalf("pause events %v, want pause,resume", kinds)
		}
	})
	t.Run("herdr down at launch: requeued, no attempt", func(t *testing.T) {
		f, d, q := newFakeDrain(t)
		enqueueAt(t, q, "w", "/repo/a", drainT0)
		f.launch = func(worker.QueueRow) drain.Attempt {
			return drain.Attempt{Class: drain.ErrHerdrDown, Err: "unreachable", CreatedNothing: true}
		}
		d.tick(t.Context())
		if r := rowNamed(t, q, "w"); r.State != worker.QueueQueued || r.Attempts != 0 || !d.pauses.Paused() {
			t.Fatalf("row %+v paused %v", r, d.pauses.Paused())
		}
	})
	t.Run("invalid config pauses, never defaults", func(t *testing.T) {
		f, d, q := newFakeDrain(t)
		enqueueAt(t, q, "w", "/repo/a", drainT0)
		zero := 0
		f.cfg.Surface.Drain.Cap = &zero
		d.tick(t.Context())
		if len(f.launched) != 0 || !strings.Contains(d.pauses.Reason(), "cap") {
			t.Fatalf("cap = 0: launched %v, pause %q", f.launched, d.pauses.Reason())
		}
		f.cfg.Surface.Drain.Cap = nil
		d.tick(t.Context())
		if len(f.launched) != 1 || d.pauses.Paused() {
			t.Fatalf("valid again: launched %v, pause %q", f.launched, d.pauses.Reason())
		}
	})
}

func TestDrainTickWatch(t *testing.T) {
	f, d, q := newFakeDrain(t)
	enqueueAt(t, q, "w", "/repo/a", drainT0)
	id := seedState(t, q, "w", worker.QueueLaunched)
	seedLedger(t, "/repo/a", "w", id, worker.StageLaunched, func(r *worker.Row) { r.Harness = "claude" })
	verdict := ready.StateBlocked
	f.probe = func(worker.QueueRow, worker.Row) drain.Probe {
		return drain.Probe{State: drain.ProbeRead, Verdict: ready.Verdict{State: verdict, Blocking: "permission prompt"}}
	}
	d.tick(t.Context())
	d.tick(t.Context())
	if r := rowNamed(t, q, "w"); r.State != worker.QueueNeedsYou || len(f.notified) != 1 {
		t.Fatalf("blocked: row %s, notified %v; want needs-you, notified once", r.State, f.notified)
	}
	verdict = ready.StateNotReady
	d.tick(t.Context())
	if r := rowNamed(t, q, "w"); r.State != worker.QueueLaunched {
		t.Fatalf("answered: row %s, want launched", r.State)
	}
	// Idle: at the prompt for idle_minutes with no report.
	verdict = ready.StateReady
	d.tick(t.Context())
	f.now = f.now.Add(config.DefaultDrainIdleMinutes * time.Minute)
	d.tick(t.Context())
	if r := rowNamed(t, q, "w"); r.State != worker.QueueNeedsYou || r.LastError != drain.ReasonIdle || len(f.notified) != 2 {
		t.Fatalf("idle: row %s %q, notified %v", r.State, r.LastError, f.notified)
	}
	// Ledger closed: the row closes.
	led, err := worker.Open("/repo/a", drainTestSession)
	if err != nil {
		t.Fatal(err)
	}
	if err := led.Update("w", func(r *worker.Row) { r.Stage = worker.StageClosed }); err != nil {
		t.Fatal(err)
	}
	d.tick(t.Context())
	if r := rowNamed(t, q, "w"); r.State != worker.QueueClosed {
		t.Fatalf("ledger closed: row %s, want closed", r.State)
	}
}

func TestDrainTickExpireAndPrune(t *testing.T) {
	f, d, q := newFakeDrain(t)
	f.herdrErr = errors.New("down") // nothing launches; only time moves
	enqueueAt(t, q, "old", "/repo/a", drainT0)
	f.now = drainT0.Add(drain.ExpireAfter + time.Minute)
	d.tick(t.Context())
	if r := rowNamed(t, q, "old"); r.State != worker.QueueExpired {
		t.Fatalf("after 7 days: %s, want expired", r.State)
	}
	// The daily prune is surface prune's own run over the real queue.
	d.io.prune = func(ctx context.Context, now time.Time) (pruneResult, error) {
		f.pruneRuns++
		return runPrune(ctx, testPruneDeps(t, q, nil, errors.New("no herdr in this test")), now, drain.PruneAfter, false)
	}
	f.now = f.now.Add(drain.PruneAfter + time.Minute)
	d.tick(t.Context())
	rows, err := q.Rows()
	if err != nil || len(rows) != 0 {
		t.Fatalf("after 30 more days: rows %+v, err %v; want pruned", rows, err)
	}
	pruned := false
	for _, e := range f.events {
		pruned = pruned || (e.Kind == drain.EventState && e.Name == "old" && e.State == "pruned")
	}
	if !pruned {
		t.Fatalf("no pruned event for old: %+v", f.events)
	}
}

// killLedger is the ledger a launch writes; it ends the launch's goroutine
// (runtime.Goexit, as if the process died) on the call numbered killAt.
type killLedger struct {
	*worker.Ledger
	calls, killAt int
}

func (k *killLedger) Begin(r worker.Row) error {
	k.calls++
	if k.calls == k.killAt {
		runtime.Goexit()
	}
	return k.Ledger.Begin(r)
}

func (k *killLedger) Update(name string, fn func(*worker.Row)) error {
	k.calls++
	if k.calls == k.killAt {
		runtime.Goexit()
	}
	return k.Ledger.Update(name, fn)
}

// TestDrainKillAtEachLaunchStage stops a real launch (real ledger, fake
// steps) at each ledger write, as if the drain died there, then runs a tick
// as the restarted drain and checks the reconcile table's outcome.
func TestDrainKillAtEachLaunchStage(t *testing.T) {
	cases := map[string]struct {
		killAt int // ledger call that never completes; 0 runs to the end
		failAt string
		want   worker.QueueState
	}{
		"before the pending row":         {killAt: 1, want: worker.QueueQueued},
		"before the worktree is noted":   {killAt: 2, want: worker.QueueFailed},
		"before the harness is noted":    {killAt: 3, want: worker.QueueFailed},
		"before the launched row":        {killAt: 4, want: worker.QueueFailed},
		"after the ledger, before queue": {killAt: 0, want: worker.QueueLaunched},
		"a launch that created nothing":  {failAt: "worktree", want: worker.QueueQueued},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f, d, q := newFakeDrain(t)
			f.herdrErr = errors.New("down") // the restarted drain must only reconcile
			enqueueAt(t, q, "w", "/repo/a", drainT0)
			claimed, err := q.ClaimFor("w", "launch-dead", drainTestSession, drainT0)
			if err != nil {
				t.Fatal(err)
			}
			led, err := worker.Open("/repo/a", drainTestSession)
			if err != nil {
				t.Fatal(err)
			}
			kl := &killLedger{Ledger: led, killAt: c.killAt}
			ref := testHerdrRef(t)
			steps := workerSteps{
				identify: func(context.Context) (repoIdentity, error) { return repoIdentity{}, nil },
				addWorktree: func(context.Context) (worker.Worktree, error) {
					if c.failAt == "worktree" {
						return worker.Worktree{}, errors.New("base lookup failed")
					}
					return worker.Worktree{Path: "/repo/a/.claude/worktrees/w"}, nil
				},
				build: func(string) (launch.BuiltInvocation, error) {
					return launch.BuiltInvocation{Worker: true, Invocation: launch.Invocation{Harness: "claude"}}, nil
				},
				launch: func(context.Context, launch.Invocation) (backend.Ref, error) {
					return ref, nil
				},
				now:      func() time.Time { return drainT0 },
				launchID: claimed.LaunchID,
			}
			var wg sync.WaitGroup
			wg.Go(func() { _, _ = runWorkerSteps(t.Context(), kl, "w", drain.Branch("w"), steps) })
			wg.Wait()
			if got := kl.calls; c.killAt > 0 && got != c.killAt {
				t.Fatalf("the launch made %d ledger calls; the kill at call %d never happened", got, c.killAt)
			}
			d.tick(t.Context())
			r := rowNamed(t, q, "w")
			if r.State != c.want || r.Attempts != 0 {
				t.Fatalf("after restart: %s attempts %d (%q); want %s, no attempt", r.State, r.Attempts, r.LastError, c.want)
			}
			if c.want == worker.QueueFailed && !strings.Contains(r.LastError, "/repo/a/.claude/worktrees/w") {
				t.Errorf("failed row does not name the worktree: %q", r.LastError)
			}
		})
	}
}

func TestClassifyLaunchError(t *testing.T) {
	ghAuth := &exec.CommandError{Name: "gh", Stderr: "HTTP 401: Bad credentials (https://api.github.com/repos/o/r)"}
	ghOther := &exec.CommandError{Name: "gh", Stderr: "HTTP 404: Not Found"}
	cases := map[string]struct {
		err  error
		want drain.ErrClass
	}{
		"name taken":                 {fmt.Errorf("wrap: %w", worker.ErrNameTaken), drain.ErrNameTaken},
		"herdr not on PATH":          {fmt.Errorf("%w: herdr not found", errBackendUnavailable), drain.ErrHerdrDown},
		"herdr session not running":  {fmt.Errorf("launch: %w", backend.NewStartCause(backend.FailureUnavailable, errors.New("x"))), drain.ErrHerdrDown},
		"herdr timeout is other":     {backend.NewStartCause(backend.FailureTimeout, errors.New("x")), drain.ErrOther},
		"gh 401":                     {fmt.Errorf("forgectl: read o/r's default branch from GitHub: %w", ghAuth), drain.ErrGitHubAuth},
		"gh 404 is other":            {fmt.Errorf("read: %w", ghOther), drain.ErrOther},
		"401 text from git is other": {&exec.CommandError{Name: "git", Stderr: "HTTP 401"}, drain.ErrOther},
		"surface launch error":       {&surface.LaunchError{}, drain.ErrOther},
		"run directory":              {fmt.Errorf("launch: %w: base is not absolute", surface.ErrRunDir), drain.ErrLaunchConfig},
		"socket path too long":       {fmt.Errorf("launch: %w: set TMPDIR shorter", surface.ErrSocketPathTooLong), drain.ErrLaunchConfig},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if got := classifyLaunchError(c.err); got != c.want {
				t.Fatalf("classifyLaunchError = %v, want %v", got, c.want)
			}
		})
	}
}

// --- start, stop ---

// drainCmdEnv gives start a herdr on PATH (a stub it only resolves) and a
// fresh state directory.
func drainCmdEnv(t *testing.T) worker.DrainFiles {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "herdr"), []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil { //nolint:gosec // G306: an executable stub
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HERDR_SESSION", drainTestSession)
	files, err := worker.OpenDrainFiles()
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func stubSpawn(t *testing.T, fn func(args, env []string) (int, error)) {
	t.Helper()
	saved, savedWait := drainSpawn, drainStartWait
	drainSpawn, drainStartWait = fn, 300*time.Millisecond
	t.Cleanup(func() { drainSpawn, drainStartWait = saved, savedWait })
}

func TestDrainStartRefusesASecondDrain(t *testing.T) {
	files := drainCmdEnv(t)
	lock, err := files.Lock()
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close() //nolint:errcheck // test
	data, err := drain.EncodeStatus(drain.Status{Status: drain.StatusRunning, PID: 4242, ProcessStart: 777, StartedAt: drainT0})
	if err != nil {
		t.Fatal(err)
	}
	if err := files.WriteStatus(lock, data); err != nil {
		t.Fatal(err)
	}
	spawned := false
	stubSpawn(t, func([]string, []string) (int, error) { spawned = true; return 1, nil })
	_, err = runQueueCmd(t, newSurfaceDrainStartCmd(module.Deps{}))
	if err == nil || !strings.Contains(err.Error(), "pid 4242") || !strings.Contains(err.Error(), "777") || ExitCode(err) != exitFailed {
		t.Fatalf("second start: err %v (exit %d), want exit 1 naming pid 4242 and start time 777", err, ExitCode(err))
	}
	if spawned {
		t.Fatal("a second start spawned a child")
	}
}

func TestDrainStartReportsAChildThatNeverRan(t *testing.T) {
	files := drainCmdEnv(t)
	lock, err := files.Lock()
	if err != nil {
		t.Fatal(err)
	}
	line, err := drain.EncodeEvent(drain.Event{Seq: 1, Kind: drain.EventStop, TS: drainT0, Error: "the herdr session resolves to \"x\""})
	if err != nil {
		t.Fatal(err)
	}
	if err := files.AppendEvent(lock, line); err != nil {
		t.Fatal(err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	var gotArgs, gotEnv []string
	stubSpawn(t, func(args, env []string) (int, error) { gotArgs, gotEnv = args, env; return 99999, nil })
	_, err = runQueueCmd(t, newSurfaceDrainStartCmd(module.Deps{}))
	if err == nil || ExitCode(err) != exitFailed || !strings.Contains(err.Error(), "did not report running") || !strings.Contains(err.Error(), "herdr session resolves") {
		t.Fatalf("start: err %v (exit %d), want exit 1 with the last event", err, ExitCode(err))
	}
	if strings.Join(gotArgs[:4], " ") != "surface _drain --session "+drainTestSession {
		t.Errorf("child argv %v", gotArgs)
	}
	if gotEnv[len(gotEnv)-1] != "HERDR_SESSION="+drainTestSession {
		t.Errorf("child env does not pin HERDR_SESSION last: %v", gotEnv[len(gotEnv)-1])
	}
}

func TestDrainStartWaitsForRunning(t *testing.T) {
	files := drainCmdEnv(t)
	stubSpawn(t, func([]string, []string) (int, error) {
		lock, err := files.Lock()
		if err != nil {
			return 0, err
		}
		defer lock.Close() //nolint:errcheck // test
		data, err := drain.EncodeStatus(drain.Status{Status: drain.StatusRunning, PID: 31337, ProcessStart: 5, HerdrSession: drainTestSession})
		if err != nil {
			return 0, err
		}
		return 31337, files.WriteStatus(lock, data)
	})
	out, err := runQueueCmd(t, newSurfaceDrainStartCmd(module.Deps{}), "--json")
	if err != nil {
		t.Fatal(err)
	}
	var res drainStartResult
	if err := json.Unmarshal([]byte(out), &res); err != nil || res.PID != 31337 || res.Status != drain.StatusRunning || res.HerdrSession != drainTestSession {
		t.Fatalf("start --json: %q, %v", out, err)
	}
}

func TestDrainStopRefusesUncheckedPIDs(t *testing.T) {
	self := os.Getpid()
	own, err := procstart.Of(self)
	if err != nil {
		t.Skipf("no readable start time here: %v", err)
	}
	cases := map[string]struct {
		st     drain.Status
		signal bool
		inErr  string
	}{
		"zero start time":     {drain.Status{Status: drain.StatusRunning, PID: self}, false, "no process start time"},
		"mismatched start":    {drain.Status{Status: drain.StatusRunning, PID: self, ProcessStart: own + 1}, false, fmt.Sprintf("expected process start time %d, saw %d", own+1, own)},
		"stopped":             {drain.Status{Status: drain.StatusStopped, PID: self, ProcessStart: own}, false, "stopped"},
		"matching start time": {drain.Status{Status: drain.StatusRunning, PID: self, ProcessStart: own}, true, ""},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			files := drainCmdEnv(t)
			lock, err := files.Lock()
			if err != nil {
				t.Fatal(err)
			}
			data, err := drain.EncodeStatus(c.st)
			if err != nil {
				t.Fatal(err)
			}
			if err := files.WriteStatus(lock, data); err != nil {
				t.Fatal(err)
			}
			if err := lock.Close(); err != nil {
				t.Fatal(err)
			}
			signaled := 0
			saved := drainSignal
			drainSignal = func(pid int) error { signaled = pid; return nil }
			t.Cleanup(func() { drainSignal = saved })
			_, err = runQueueCmd(t, newSurfaceDrainStopCmd())
			if c.signal {
				if err != nil || signaled != self {
					t.Fatalf("stop: err %v, signaled %d", err, signaled)
				}
				return
			}
			if signaled != 0 || err == nil || ExitCode(err) != exitFailed || !strings.Contains(err.Error(), c.inErr) {
				t.Fatalf("stop: err %v (exit %d), signaled %d; want exit 1 naming %q and no signal", err, ExitCode(err), signaled, c.inErr)
			}
		})
	}
}

func TestDrainStatusAndEvents(t *testing.T) {
	files := drainCmdEnv(t)
	q, err := worker.OpenQueue()
	if err != nil {
		t.Fatal(err)
	}
	enqueueAt(t, q, "w", "/repo/a", drainT0)
	out, err := runQueueCmd(t, newSurfaceDrainStatusCmd(), "--json")
	if err != nil {
		t.Fatal(err)
	}
	var v drainStatusView
	if err := json.Unmarshal([]byte(out), &v); err != nil || v.Status != drain.StatusStopped || v.Counts["queued"] != 1 {
		t.Fatalf("status with no drain.json: %q, %v", out, err)
	}
	lock, err := files.Lock()
	if err != nil {
		t.Fatal(err)
	}
	data, err := drain.EncodeStatus(drain.Status{Status: drain.StatusRunning, PID: 1, ProcessStart: 1, IntervalSeconds: 15, LastTick: time.Now().Add(-time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if err := files.WriteStatus(lock, data); err != nil {
		t.Fatal(err)
	}
	for i, e := range []drain.Event{{Seq: 1, Kind: drain.EventStart}, {Seq: 2, Kind: drain.EventState, Name: "w", State: "claimed"}, {Seq: 3, Kind: drain.EventState, Name: "w", State: "launched"}} {
		e.TS = drainT0.Add(time.Duration(i) * time.Second)
		line, err := drain.EncodeEvent(e)
		if err != nil {
			t.Fatal(err)
		}
		if err := files.AppendEvent(lock, line); err != nil {
			t.Fatal(err)
		}
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	out, err = runQueueCmd(t, newSurfaceDrainStatusCmd(), "--json")
	if err != nil || json.Unmarshal([]byte(out), &v) != nil || v.Status != drain.StatusStale {
		t.Fatalf("status with an hour-old tick: %q, %v; want stale", out, err)
	}
	out, err = runQueueCmd(t, newSurfaceDrainEventsCmd(), "--since", "2", "--json")
	var ev drainEventsResult
	if err != nil || json.Unmarshal([]byte(out), &ev) != nil || len(ev.Events) != 1 || ev.Events[0].State != "launched" {
		t.Fatalf("events --since 2: %q, %v", out, err)
	}
	out, err = runQueueCmd(t, newSurfaceDrainEventsCmd())
	if err != nil || strings.Count(out, "\n") != 3 || !strings.Contains(out, "#3 state w launched") {
		t.Fatalf("events text: %q, %v", out, err)
	}
}

type fakeScreen struct {
	session string
	screen  ready.Screen
	err     error
	reads   int
}

func (f *fakeScreen) Session() string { return f.session }

func (f *fakeScreen) WorkerScreen(context.Context, backend.Ref) (ready.Screen, error) {
	f.reads++
	return f.screen, f.err
}

// TestProbeWorker pins the probe's mapping: an errored read is unreadable,
// never gone; only ErrWorkerGone is gone; another session is unreadable
// without a read.
func TestProbeWorker(t *testing.T) {
	table, err := ready.Default()
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := testHerdrRef(t).MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	led := worker.Row{Name: "w", Harness: "claude", Stage: worker.StageLaunched, Ref: encoded}
	q := worker.QueueRow{Name: "w", Session: drainTestSession}
	cases := map[string]struct {
		screen *fakeScreen
		q      worker.QueueRow
		want   drain.ProbeState
		reads  int
	}{
		"read":            {&fakeScreen{session: drainTestSession, screen: ready.Screen{Text: "> ", Status: "idle", Agent: "claude"}}, q, drain.ProbeRead, 1},
		"gone":            {&fakeScreen{session: drainTestSession, err: herdradapter.ErrWorkerGone}, q, drain.ProbeGone, 1},
		"herdr error":     {&fakeScreen{session: drainTestSession, err: herdradapter.ErrScreenUnreadable}, q, drain.ProbeUnreadable, 1},
		"another session": {&fakeScreen{session: "other"}, q, drain.ProbeUnreadable, 0},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			p := probeWorker(t.Context(), c.screen, table, drainTestSession, c.q, led)
			if p.State != c.want || c.screen.reads != c.reads {
				t.Fatalf("probe %+v after %d reads; want state %v after %d", p, c.screen.reads, c.want, c.reads)
			}
		})
	}
}

// seedLive puts name in state with its own ledger row at stage.
func seedLive(t *testing.T, q *worker.Queue, name, repo string, state worker.QueueState, stage worker.Stage) string {
	t.Helper()
	enqueueAt(t, q, name, repo, drainT0)
	id := seedState(t, q, name, state)
	seedLedger(t, repo, name, id, stage, func(r *worker.Row) { r.Harness = "claude" })
	return id
}

// TestDrainNotifyOffSuppressesTheNotification drives a launched → needs-you
// change with notify = false: the row changes, nothing is notified.
func TestDrainNotifyOffSuppressesTheNotification(t *testing.T) {
	for _, notify := range []bool{true, false} {
		t.Run(fmt.Sprintf("notify=%v", notify), func(t *testing.T) {
			f, d, q := newFakeDrain(t)
			seedLive(t, q, "w", "/repo/a", worker.QueueLaunched, worker.StageLaunched)
			f.cfg.Surface.Drain.Notify = &notify
			f.probe = func(worker.QueueRow, worker.Row) drain.Probe {
				return drain.Probe{State: drain.ProbeRead, Verdict: ready.Verdict{State: ready.StateBlocked, Blocking: "permission prompt"}}
			}
			d.tick(t.Context())
			if r := rowNamed(t, q, "w"); r.State != worker.QueueNeedsYou {
				t.Fatalf("row %s, want needs-you", r.State)
			}
			want := 0
			if notify {
				want = 1
			}
			if len(f.notified) != want {
				t.Fatalf("notified %v, want %d notifications", f.notified, want)
			}
		})
	}
}

// errorEvents counts the error events whose text starts with prefix.
func errorEvents(f *fakeDrain, prefix string) int {
	n := 0
	for _, e := range f.events {
		if e.Kind == drain.EventError && strings.HasPrefix(e.Error, prefix) {
			n++
		}
	}
	return n
}

// TestDrainNotifyFailureIsOneEvent: a failed needs-you signal never fails
// the tick, and is one error event per entry into needs-you, not one per
// tick. Leaving needs-you clears the pane, once, and a failed clear is one
// event too.
func TestDrainNotifyFailureIsOneEvent(t *testing.T) {
	f, d, q := newFakeDrain(t)
	seedLive(t, q, "w", "/repo/a", worker.QueueLaunched, worker.StageLaunched)
	f.notifyErr = errors.New("herdr pane state: refused")
	f.clearErr = errors.New("herdr pane state: refused")
	verdict := ready.StateBlocked
	f.probe = func(worker.QueueRow, worker.Row) drain.Probe {
		return drain.Probe{State: drain.ProbeRead, Verdict: ready.Verdict{State: verdict, Blocking: "permission prompt"}}
	}
	for range 3 {
		d.tick(t.Context())
	}
	if r := rowNamed(t, q, "w"); r.State != worker.QueueNeedsYou {
		t.Fatalf("row %s, want needs-you", r.State)
	}
	if n := errorEvents(f, "needs-you notification: "); len(f.notified) != 1 || n != 1 {
		t.Fatalf("notified %v, %d error events; want one each", f.notified, n)
	}
	if len(f.cleared) != 0 {
		t.Fatalf("cleared %v while still in needs-you", f.cleared)
	}
	verdict = ready.StateNotReady
	for range 3 {
		d.tick(t.Context())
	}
	if r := rowNamed(t, q, "w"); r.State != worker.QueueLaunched {
		t.Fatalf("row %s, want launched", r.State)
	}
	if n := errorEvents(f, "clear the needs-you pane state: "); len(f.cleared) != 1 || n != 1 {
		t.Fatalf("cleared %v, %d error events; want one each", f.cleared, n)
	}
}

// TestDrainClearsWithNotifyOff: clearing is not gated on notify, so turning
// it off cannot strand a pane marked from before.
func TestDrainClearsWithNotifyOff(t *testing.T) {
	f, d, q := newFakeDrain(t)
	seedLive(t, q, "w", "/repo/a", worker.QueueNeedsYou, worker.StageLaunched)
	off := false
	f.cfg.Surface.Drain.Notify = &off
	f.probe = func(worker.QueueRow, worker.Row) drain.Probe {
		return drain.Probe{State: drain.ProbeRead, Verdict: ready.Verdict{State: ready.StateNotReady}}
	}
	d.tick(t.Context())
	if r := rowNamed(t, q, "w"); r.State != worker.QueueLaunched || len(f.cleared) != 1 || len(f.notified) != 0 {
		t.Fatalf("row %s, cleared %v, notified %v", r.State, f.cleared, f.notified)
	}
}

// TestDrainClearsOnlyOnLeavingNeedsYou: a launched row that reports was
// never marked, so nothing is released.
func TestDrainClearsOnlyOnLeavingNeedsYou(t *testing.T) {
	f, d, q := newFakeDrain(t)
	seedLive(t, q, "w", "/repo/a", worker.QueueLaunched, worker.StageLaunched)
	f.probe = func(worker.QueueRow, worker.Row) drain.Probe {
		return drain.Probe{State: drain.ProbeRead, Report: true, Verdict: ready.Verdict{State: ready.StateReady}}
	}
	d.tick(t.Context())
	if r := rowNamed(t, q, "w"); r.State != worker.QueueReported || len(f.cleared) != 0 {
		t.Fatalf("row %s, cleared %v; want reported, nothing cleared", r.State, f.cleared)
	}
}

// TestDrainSettlesClosedTerminalRows pins the Loop closer: a reported or
// failed row reads closed once surface close closes or removes its ledger row.
func TestDrainSettlesClosedTerminalRows(t *testing.T) {
	f, d, q := newFakeDrain(t)
	f.herdrErr = errors.New("down")
	seedLive(t, q, "rep", "/repo/a", worker.QueueReported, worker.StageLaunched)
	seedLive(t, q, "fail", "/repo/b", worker.QueueFailed, worker.StageFailed)
	d.tick(t.Context())
	if a, b := rowNamed(t, q, "rep").State, rowNamed(t, q, "fail").State; a != worker.QueueReported || b != worker.QueueFailed {
		t.Fatalf("before close: %s, %s", a, b)
	}
	ledA, err := worker.Open("/repo/a", drainTestSession)
	if err != nil {
		t.Fatal(err)
	}
	if err := ledA.Update("rep", func(r *worker.Row) { r.Stage = worker.StageClosed }); err != nil {
		t.Fatal(err)
	}
	ledB, err := worker.Open("/repo/b", drainTestSession)
	if err != nil {
		t.Fatal(err)
	}
	if err := ledB.RemoveIf("fail", func(worker.Row) bool { return true }); err != nil {
		t.Fatal(err)
	}
	d.tick(t.Context())
	if a, b := rowNamed(t, q, "rep").State, rowNamed(t, q, "fail").State; a != worker.QueueClosed || b != worker.QueueClosed {
		t.Fatalf("after close: reported row %s, failed row %s; want both closed", a, b)
	}
}

// TestDrainDoesNotAdoptAnotherLaunch: a ledger row under the name with
// another launch id is not the drain's. The launched row is not read, holds
// no slot, and is closed; a claimed row is requeued.
func TestDrainDoesNotAdoptAnotherLaunch(t *testing.T) {
	f, d, q := newFakeDrain(t)
	f.herdrErr = errors.New("down")
	enqueueAt(t, q, "w", "/repo/a", drainT0)
	seedState(t, q, "w", worker.QueueLaunched)
	seedLedger(t, "/repo/a", "w", "launch-someone-else", worker.StageLaunched, nil)
	enqueueAt(t, q, "c", "/repo/b", drainT0)
	seedState(t, q, "c", worker.QueueClaimed)
	seedLedger(t, "/repo/b", "c", "launch-someone-else", worker.StageLaunched, nil)
	probed := 0
	f.probe = func(worker.QueueRow, worker.Row) drain.Probe { probed++; return drain.Probe{} }
	rows, err := q.Rows()
	if err != nil {
		t.Fatal(err)
	}
	if held := drain.Held(rows, d.ledgers(rows)); strings.Join(held, ",") != "c" {
		t.Fatalf("held %v; only the claimed row may hold a slot", held)
	}
	d.tick(t.Context())
	if probed != 0 {
		t.Fatal("another launch's worker was read")
	}
	if w, c := rowNamed(t, q, "w"), rowNamed(t, q, "c"); w.State != worker.QueueClosed || c.State != worker.QueueQueued || c.LaunchID != "" {
		t.Fatalf("w %s; c %s launch %q; want closed and requeued", w.State, c.State, c.LaunchID)
	}
}

// TestDrainClearsWithoutAnotherLaunchsPane: a needs-you row whose ledger row
// now belongs to another launch closes, and its release is not aimed at that
// launch's pane (the fake fails the test on a mismatched ledger row).
func TestDrainClearsWithoutAnotherLaunchsPane(t *testing.T) {
	f, d, q := newFakeDrain(t)
	f.herdrErr = errors.New("down")
	enqueueAt(t, q, "w", "/repo/a", drainT0)
	seedState(t, q, "w", worker.QueueNeedsYou)
	seedLedger(t, "/repo/a", "w", "launch-someone-else", worker.StageLaunched, nil)
	d.tick(t.Context())
	if w := rowNamed(t, q, "w"); w.State != worker.QueueClosed || len(f.cleared) != 1 {
		t.Fatalf("w %s, cleared %v; want closed and one release", w.State, f.cleared)
	}
}

// TestDrainReconcileUnreadableIsOneEvent: a claimed row over an unreadable
// ledger is noted once across ticks.
func TestDrainReconcileUnreadableIsOneEvent(t *testing.T) {
	f, d, q := newFakeDrain(t)
	f.herdrErr = errors.New("down")
	enqueueAt(t, q, "w", "/repo/a", drainT0)
	seedState(t, q, "w", worker.QueueClaimed)
	d.io.ledgerRows = func(string, string) ([]worker.Row, error) { return nil, errors.New("ledger version 9") }
	for range 3 {
		d.tick(t.Context())
	}
	n := 0
	for _, e := range f.events {
		if e.Kind == drain.EventUnreadable {
			n++
		}
	}
	if n != 1 || rowNamed(t, q, "w").State != worker.QueueClaimed {
		t.Fatalf("%d unreadable events over 3 ticks, row %s; want 1 and still claimed", n, rowNamed(t, q, "w").State)
	}
}

// TestDrainPausesOnLaunchConfig: a launch config that cannot build a worker
// fails the row (naming its worktree) and pauses claiming, so the rest of
// the queue is not burned.
func TestDrainPausesOnLaunchConfig(t *testing.T) {
	f, d, q := newFakeDrain(t)
	enqueueAt(t, q, "a", "/repo/a", drainT0)
	enqueueAt(t, q, "b", "/repo/b", drainT0.Add(time.Minute))
	enqueueAt(t, q, "c", "/repo/c", drainT0.Add(2*time.Minute))
	f.launch = func(row worker.QueueRow) drain.Attempt {
		wt := worker.WorktreePath(row.Repo, row.Name)
		seedLedger(t, row.Repo, row.Name, row.LaunchID, worker.StageFailed, func(r *worker.Row) { r.Worktree = wt })
		return drain.Attempt{Class: drain.ErrLaunchConfig, Err: "harness binary was found on $PATH", Worktree: wt, Row: &worker.Row{Stage: worker.StageFailed, Worktree: wt}}
	}
	d.tick(t.Context())
	d.tick(t.Context())
	if len(f.launched) != 1 {
		t.Fatalf("launched %v; a launch-config failure must stop further claims", f.launched)
	}
	if r := rowNamed(t, q, "a"); r.State != worker.QueueFailed || !strings.Contains(r.LastError, "/repo/a/.claude/worktrees/a") {
		t.Fatalf("row a %s %q", r.State, r.LastError)
	}
	if b := rowNamed(t, q, "b"); b.State != worker.QueueQueued || !strings.Contains(d.pauses.Reason(), "launch-config") {
		t.Fatalf("row b %s, pause %q", b.State, d.pauses.Reason())
	}
}

// symlinkedRunDir is a short base that is a symlink to a private directory:
// the shape of /tmp on macOS, which every launch hit with TMPDIR unset in the
// atelier P2 live run (forgectl#1188).
func symlinkedRunDir(t *testing.T) string {
	t.Helper()
	target, err := os.MkdirTemp("", "rd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(target) })
	link := filepath.Join(target, "l")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	return link
}

// symlinkedRunDirErr is the real error NewRunDir returns for that base.
func symlinkedRunDirErr(t *testing.T) error {
	t.Helper()
	dir, err := surface.NewRunDir(symlinkedRunDir(t))
	if err == nil {
		_ = dir.Close()
		t.Fatal("NewRunDir accepted a symlinked base; the fixture cannot fail")
	}
	if !errors.Is(err, surface.ErrRunDir) {
		t.Fatalf("NewRunDir on a symlinked base = %v, want ErrRunDir", err)
	}
	return err
}

// TestDrainPausesOnARunDirectoryFailure is forgectl#1188: a run directory the
// environment cannot provide fails every launch the same way, so the first
// row fails naming its worktree and claiming pauses rather than burning the
// queue one worktree at a time.
func TestDrainPausesOnARunDirectoryFailure(t *testing.T) {
	rdErr := fmt.Errorf("surface: launch failed in setup (not-mutated): %w", symlinkedRunDirErr(t))
	f, d, q := newFakeDrain(t)
	enqueueAt(t, q, "a", "/repo/a", drainT0)
	enqueueAt(t, q, "b", "/repo/b", drainT0.Add(time.Minute))
	enqueueAt(t, q, "c", "/repo/c", drainT0.Add(2*time.Minute))
	f.launch = func(row worker.QueueRow) drain.Attempt {
		wt := worker.WorktreePath(row.Repo, row.Name)
		seedLedger(t, row.Repo, row.Name, row.LaunchID, worker.StageFailed, func(r *worker.Row) { r.Worktree = wt })
		return drain.Attempt{Class: classifyLaunchError(rdErr), Err: rdErr.Error(), Worktree: wt, Row: &worker.Row{Stage: worker.StageFailed, Worktree: wt}}
	}
	d.tick(t.Context())
	d.tick(t.Context())
	if len(f.launched) != 1 {
		t.Fatalf("launched %v; a run-directory failure must stop further claims", f.launched)
	}
	if r := rowNamed(t, q, "a"); r.State != worker.QueueFailed || !strings.Contains(r.LastError, "/repo/a/.claude/worktrees/a") {
		t.Fatalf("row a %s %q", r.State, r.LastError)
	}
	reason := d.pauses.Reason()
	if b := rowNamed(t, q, "b"); b.State != worker.QueueQueued || !strings.Contains(reason, "launch-config") || !strings.Contains(reason, "private run directory") {
		t.Fatalf("row b %s, pause %q", b.State, reason)
	}
}

// TestDrainStartRefusesARunDirectoryItCannotCreate: start makes the run
// directory a launch would, and refuses with exit 2 naming TMPDIR before any
// child starts.
func TestDrainStartRefusesARunDirectoryItCannotCreate(t *testing.T) {
	cases := map[string]func(t *testing.T){
		"TMPDIR is a symlink": func(t *testing.T) { t.Setenv("TMPDIR", symlinkedRunDir(t)) },
	}
	if runtime.GOOS == "darwin" {
		// The live run: no TMPDIR, so the base is /tmp, a symlink on macOS.
		cases["TMPDIR is unset on macOS"] = func(t *testing.T) {
			t.Setenv("TMPDIR", "")
			if err := os.Unsetenv("TMPDIR"); err != nil {
				t.Fatal(err)
			}
		}
	}
	for name, setTMPDIR := range cases {
		t.Run(name, func(t *testing.T) {
			drainCmdEnv(t)
			spawned := false
			stubSpawn(t, func([]string, []string) (int, error) { spawned = true; return 1, nil })
			setTMPDIR(t)
			_, err := runQueueCmd(t, newSurfaceDrainStartCmd(module.Deps{}))
			if err == nil || ExitCode(err) != exitUsage || !strings.Contains(err.Error(), "TMPDIR") || !strings.Contains(err.Error(), "private run directory") {
				t.Fatalf("start: err %v (exit %d), want exit 2 naming TMPDIR and the run directory", err, ExitCode(err))
			}
			if spawned {
				t.Fatal("start spawned a drain whose every launch would fail")
			}
		})
	}
}

// TestLaunchConfigErrorsClassify drives a real build-step failure through
// runWorkerSteps and checks the drain reads it as a launch-config failure.
func TestLaunchConfigErrorsClassify(t *testing.T) {
	cases := map[string]func(workerSteps) workerSteps{
		"build error": func(s workerSteps) workerSteps {
			s.build = func(string) (launch.BuiltInvocation, error) {
				return launch.BuiltInvocation{}, errors.New("launch: the harness profile does not resolve")
			}
			return s
		},
		"non-worker build": func(s workerSteps) workerSteps {
			build := s.build
			s.build = func(cwd string) (launch.BuiltInvocation, error) {
				b, err := build(cwd)
				b.Worker = false
				return b, err
			}
			return s
		},
		"binary on PATH at launch": func(s workerSteps) workerSteps {
			s.launch = func(context.Context, launch.Invocation) (backend.Ref, error) {
				return backend.Ref{}, fmt.Errorf("launch: %w: /usr/bin/claude", surface.ErrBinaryProvenance)
			}
			return s
		},
		"run directory under a symlinked base": func(s workerSteps) workerSteps {
			rdErr := symlinkedRunDirErr(t)
			s.launch = func(context.Context, launch.Invocation) (backend.Ref, error) {
				return backend.Ref{}, fmt.Errorf("surface: launch failed in setup: %w", rdErr)
			}
			return s
		},
	}
	for name, mod := range cases {
		t.Run(name, func(t *testing.T) {
			led := testWorkerLedger(t)
			attempt, err := attemptWorker(t.Context(), led, "w1", "feat/w1", mod(goodSteps(t)))
			if got := classifyLaunchError(err); got != drain.ErrLaunchConfig {
				t.Fatalf("classifyLaunchError(%v) = %v, want ErrLaunchConfig", err, got)
			}
			if attempt.createdNothing() {
				t.Fatal("a failure after the worktree read as creating nothing")
			}
		})
	}
	if got := classifyLaunchError(errors.New("workspace create failed")); got != drain.ErrOther {
		t.Fatalf("an unrelated error classified as %v", got)
	}
}

// TestDrainLaunchWiring drives the production launch wiring (drainLaunch's
// spec, and workerSetup.steps with git and herdr stubbed): the ledger row
// carries the claim's launch id, the harness is claude, $PATH binaries stay
// refused, and a repo whose top moved is refused before anything runs.
func TestDrainLaunchWiring(t *testing.T) {
	repo := t.TempDir()
	//nolint:gosec // G204: a fixed tool name with arguments this test constructed
	if out, err := osexec.CommandContext(t.Context(), "git", "-C", repo, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	top, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(top, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	brief := "fix it"
	row := worker.QueueRow{Name: "w1", Repo: top, Brief: brief, BriefSHA256: worker.BriefSHA256(brief), LaunchID: "launch-wired", State: worker.QueueClaimed}

	led := testWorkerLedger(t)
	var specs []workerSpec
	fake := func(ctx context.Context, warn io.Writer, deps module.Deps, spec workerSpec, text string) (workerAttempt, error) {
		specs = append(specs, spec)
		var launched []launch.Invocation
		steps := inProcessSteps(t, led, &launched)
		wired := workerSetup{top: testRepoTop, led: led, self: "/nonexistent/forgectl"}.steps(deps, spec, text, nil, warn)
		steps.launchID = wired.launchID
		return attemptWorker(ctx, led, spec.name, spec.branch, steps)
	}
	a := drainLaunchWith(exec.OSRunner{}, fake)(t.Context(), config.Config{}, row)
	if a.Class != drain.ErrNone || len(specs) != 1 {
		t.Fatalf("attempt %+v, specs %d", a, len(specs))
	}
	got := specs[0]
	if got.harness != "claude" || got.allowPATH || got.branch != "worker/w1" || got.target != top {
		t.Fatalf("spec %+v; want harness claude, no $PATH binary, branch worker/w1", got)
	}
	if r := onlyRow(t, led); r.LaunchID != row.LaunchID {
		t.Fatalf("ledger row launch_id %q, want the claim's %q", r.LaunchID, row.LaunchID)
	}

	moved := row
	moved.Repo = filepath.Join(top, "sub")
	a = drainLaunchWith(exec.OSRunner{}, fake)(t.Context(), config.Config{}, moved)
	if a.Class != drain.ErrRowInvalid || !a.CreatedNothing || len(specs) != 1 || !strings.Contains(a.Err, "expected the queued") {
		t.Fatalf("moved top: attempt %+v, launches %d; want refused before launch", a, len(specs))
	}
}

func TestDrainStatusShowsHeldSlots(t *testing.T) {
	drainCmdEnv(t)
	q, err := worker.OpenQueue()
	if err != nil {
		t.Fatal(err)
	}
	seedLive(t, q, "stuck", "/repo/a", worker.QueueFailed, worker.StageLaunched)
	enqueueAt(t, q, "waiting", "/repo/a", drainT0)
	out, err := runQueueCmd(t, newSurfaceDrainStatusCmd(), "--json")
	var v drainStatusView
	if err != nil || json.Unmarshal([]byte(out), &v) != nil || v.HeldSlots != 1 || strings.Join(v.Holding, ",") != "stuck" {
		t.Fatalf("status: %q, %v; want held_slots 1 holding [stuck]", out, err)
	}
	out, err = runQueueCmd(t, newSurfaceDrainStatusCmd())
	if err != nil || !strings.Contains(out, "slots held: 1 (stuck)") {
		t.Fatalf("status text: %q, %v", out, err)
	}
}
