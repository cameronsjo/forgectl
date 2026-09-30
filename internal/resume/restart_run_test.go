package resume

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/exec"
)

// fakeEnv models one claude process in one pane. Its state moves only through
// Terminate and Relaunch, so the tests assert on the sequence the run drove.
type fakeEnv struct {
	obs Observation // what observe returns while the process runs

	// Knobs.
	ignoresSIGTERM bool   // the process survives the signal
	paneGoneAfter  bool   // the pane disappears once the process stops
	noShellReturn  bool   // the pane's shell never takes the foreground back
	shellAlwaysFG  bool   // the pane claims its shell holds the foreground even while claude runs
	relaunchErr    error  // herdr pane run fails
	noRegister     bool   // the relaunched session never registers
	prepareErr     error  // snapshot cannot record the session
	onTerminate    func() // runs inside Terminate (e.g. a Ctrl-C landing there)
	statusSeq      []string

	// Record.
	terminated, relaunched, prepared int
	paneReads, screenReads           int
	stopped                          bool
	order                            []string
}

func newFakeEnv() *fakeEnv { return &fakeEnv{obs: readyObservation()} }

func (f *fakeEnv) ReadEntry(int) (RegistryEntry, bool) {
	if f.stopped {
		return RegistryEntry{}, false
	}
	e := f.obs.Entry
	if len(f.statusSeq) > 0 {
		e.Status, f.statusSeq = f.statusSeq[0], f.statusSeq[1:]
	}
	return e, f.obs.EntryFound
}
func (f *fakeEnv) Alive(int) bool                     { return !f.stopped && f.obs.Alive }
func (f *fakeEnv) Identity(int) (ProcIdentity, error) { return f.obs.Proc, f.obs.ProcErr }

// Pane and Relaunch fail on a done context, as exec.CommandContext does, so a
// cancel that reached them would show.
func (f *fakeEnv) Pane(ctx context.Context, _ string) (PaneState, error) {
	f.paneReads++
	if err := ctx.Err(); err != nil {
		return PaneState{}, err
	}
	if f.shellAlwaysFG {
		st := f.obs.Pane
		st.ForegroundPGID = st.ShellPID
		return st, nil
	}
	if f.stopped {
		if f.paneGoneAfter {
			return PaneState{}, errors.New("pane_not_found")
		}
		st := f.obs.Pane
		if !f.noShellReturn {
			st.ForegroundPGID = st.ShellPID
		}
		return st, nil
	}
	return f.obs.Pane, f.obs.PaneErr
}
func (f *fakeEnv) Screen(context.Context, string) (string, error) {
	f.screenReads++
	return f.obs.Screen, f.obs.ScreenErr
}
func (f *fakeEnv) Prepare(string) error {
	f.prepared++
	f.order = append(f.order, "prepare")
	return f.prepareErr
}
func (f *fakeEnv) Terminate(int) error {
	f.terminated++
	f.order = append(f.order, "terminate")
	if f.onTerminate != nil {
		f.onTerminate()
	}
	if !f.ignoresSIGTERM {
		f.stopped = true
	}
	return nil
}
func (f *fakeEnv) Relaunch(ctx context.Context, _, _ string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.relaunched++
	f.order = append(f.order, "relaunch")
	return f.relaunchErr
}
func (f *fakeEnv) LiveSession(string) (RegistryEntry, bool) {
	if f.relaunched == 0 || f.noRegister || f.relaunchErr != nil {
		return RegistryEntry{}, false
	}
	return RegistryEntry{Pid: 95294, SessionID: testSID, Version: "2.1.285", Live: true}, true
}

// fakeClock advances only when the run sleeps, so bounded waits finish at once.
type fakeClock struct{ now time.Time }

func (c *fakeClock) opts(events *[]RestartEvent) RestartOptions {
	c.now = time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC)
	return RestartOptions{
		Timeout: 30 * time.Second,
		Now:     func() time.Time { return c.now },
		Sleep: func(ctx context.Context, d time.Duration) error {
			c.now = c.now.Add(d)
			return ctx.Err()
		},
		Progress: func(ev RestartEvent) { *events = append(*events, ev) },
	}
}

func runOne(t *testing.T, ctx context.Context, env *fakeEnv, mutate func(*RestartOptions)) (RestartEvent, []RestartEvent) {
	t.Helper()
	var events []RestartEvent
	var clock fakeClock
	opts := clock.opts(&events)
	if mutate != nil {
		mutate(&opts)
	}
	plan := PlanRestart([]OutdatedSession{testSession()}, nil)
	finals := RunRestart(ctx, env, plan, opts)
	if len(finals) != 1 {
		t.Fatalf("got %d finals", len(finals))
	}
	return finals[0], events
}

func states(events []RestartEvent) string {
	var s []string
	for _, ev := range events {
		s = append(s, string(ev.State))
	}
	return strings.Join(s, ",")
}

func TestRunRestart_HappyPath(t *testing.T) {
	env := newFakeEnv()
	final, events := runOne(t, context.Background(), env, nil)
	if final.State != StateResumed || !strings.Contains(final.Detail, "pid 95294") {
		t.Fatalf("final = %+v", final)
	}
	if got := states(events); got != "restarting,resumed" {
		t.Errorf("progress = %s", got)
	}
	if got := strings.Join(env.order, ","); got != "prepare,terminate,relaunch" {
		t.Errorf("order = %s; want the snapshot before the stop and the relaunch after it", got)
	}
}

func TestRunRestart_RefusalsNeverSignal(t *testing.T) {
	tests := map[string]func(*fakeEnv){
		"pid reused by a non-claude": func(f *fakeEnv) { f.obs.Proc.ExecPath = "/bin/sleep" },
		"pid reused by a new claude": func(f *fakeEnv) { f.obs.Proc.Start = f.obs.Proc.Start.Add(time.Hour) },
		"registry names another id":  func(f *fakeEnv) { f.obs.Entry.SessionID = "ffff" },
		"vanished pane":              func(f *fakeEnv) { f.obs.PaneErr = errors.New("pane_not_found") },
		"pane label disagrees":       func(f *fakeEnv) { f.obs.Pane.AgentSession = "ffff" },
		"not the foreground":         func(f *fakeEnv) { f.obs.Pane.ForegroundPIDs = nil },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			env := newFakeEnv()
			mutate(env)
			final, _ := runOne(t, context.Background(), env, nil)
			if final.State != StateSkipped || final.Manual != ManualResume(testSID) {
				t.Errorf("final = %+v; want skipped with the by-hand command", final)
			}
			if env.terminated != 0 || env.relaunched != 0 || env.prepared != 0 {
				t.Errorf("terminated=%d relaunched=%d prepared=%d; a refusal touches nothing", env.terminated, env.relaunched, env.prepared)
			}
		})
	}
}

func TestRunRestart_FailedStopIsNeverRelaunched(t *testing.T) {
	env := newFakeEnv()
	env.ignoresSIGTERM = true
	final, _ := runOne(t, context.Background(), env, nil)
	if final.State != StateFailed || final.Manual == "" {
		t.Fatalf("final = %+v", final)
	}
	if env.relaunched != 0 {
		t.Fatal("relaunched a session whose process never exited: two processes on one transcript")
	}
}

// The pid is the authority on whether the stop happened: a pane that claims
// its shell is back is not enough while the old process still runs.
func TestRunRestart_SurvivingPidBlocksRelaunchEvenWhenThePaneLooksReady(t *testing.T) {
	env := newFakeEnv()
	env.ignoresSIGTERM = true
	env.shellAlwaysFG = true
	env.obs.Pane.ForegroundPIDs = []int{testPid}
	final, _ := runOne(t, context.Background(), env, nil)
	if final.State != StateFailed || env.relaunched != 0 {
		t.Fatalf("final = %+v, relaunched = %d; want failed with no relaunch", final, env.relaunched)
	}
}

func TestRunRestart_PaneProblemsAfterTheStopFailLoudly(t *testing.T) {
	for name, mutate := range map[string]func(*fakeEnv){
		"pane vanished":        func(f *fakeEnv) { f.paneGoneAfter = true },
		"shell never returned": func(f *fakeEnv) { f.noShellReturn = true },
	} {
		t.Run(name, func(t *testing.T) {
			env := newFakeEnv()
			mutate(env)
			final, _ := runOne(t, context.Background(), env, nil)
			if final.State != StateFailed || final.Manual != ManualResume(testSID) {
				t.Errorf("final = %+v; a stopped session must be reported with the by-hand command", final)
			}
			if env.relaunched != 0 {
				t.Error("relaunched into a pane that was not ready")
			}
		})
	}
}

func TestRunRestart_RelaunchFailures(t *testing.T) {
	env := newFakeEnv()
	env.relaunchErr = errors.New("herdr down")
	if final, _ := runOne(t, context.Background(), env, nil); final.State != StateFailed || final.Manual == "" {
		t.Errorf("relaunch error: final = %+v", final)
	}
	env = newFakeEnv()
	env.noRegister = true
	if final, _ := runOne(t, context.Background(), env, nil); final.State != StateFailed || !strings.Contains(final.Detail, "no live session registered") {
		t.Errorf("unconfirmed relaunch: final = %+v", final)
	}
}

func TestRunRestart_PrepareFailureSkips(t *testing.T) {
	env := newFakeEnv()
	env.prepareErr = errors.New("disk full")
	final, _ := runOne(t, context.Background(), env, nil)
	if final.State != StateSkipped || env.terminated != 0 {
		t.Fatalf("final = %+v, terminated = %d", final, env.terminated)
	}
}

func TestRunRestart_WaitsForBusyThenRestarts(t *testing.T) {
	env := newFakeEnv()
	// ReadEntry is called once per observation; stay busy for three rounds.
	env.statusSeq = []string{"busy", "busy", "busy"}
	final, events := runOne(t, context.Background(), env, nil)
	if final.State != StateResumed {
		t.Fatalf("final = %+v", final)
	}
	if got := states(events); got != "waiting,restarting,resumed" {
		t.Errorf("progress = %s; want one waiting line, not one per poll", got)
	}
}

func TestRunRestart_TimeoutLeavesTheSessionRunning(t *testing.T) {
	env := newFakeEnv()
	env.obs.Screen = screen("❯ draft")
	final, _ := runOne(t, context.Background(), env, nil)
	if final.State != StateLeft || !strings.Contains(final.Detail, "draft") || final.Manual == "" {
		t.Fatalf("final = %+v", final)
	}
	if env.terminated != 0 {
		t.Fatal("signalled a session holding a draft")
	}
}

func TestRunRestart_CancelWhileWaitingTouchesNothing(t *testing.T) {
	env := newFakeEnv()
	env.obs.Entry.Status = "busy"
	ctx, cancel := context.WithCancel(context.Background())
	final, _ := runOne(t, ctx, env, func(o *RestartOptions) {
		sleep := o.Sleep
		o.Sleep = func(c context.Context, d time.Duration) error {
			cancel()
			return sleep(c, d)
		}
	})
	if final.State != StateLeft || !strings.Contains(final.Detail, "cancelled") {
		t.Fatalf("final = %+v", final)
	}
	if env.terminated != 0 {
		t.Fatal("signalled after a cancel")
	}
}

func TestRunRestart_CancelAfterTheSignalStillRelaunches(t *testing.T) {
	env := newFakeEnv()
	ctx, cancel := context.WithCancel(context.Background())
	env.onTerminate = cancel // Ctrl-C lands between the signal and the relaunch
	final, _ := runOne(t, ctx, env, nil)
	if final.State != StateResumed || env.relaunched != 1 {
		t.Fatalf("final = %+v, relaunched = %d; a signalled session must still be resumed", final, env.relaunched)
	}
}

func TestRunRestart_PlanOnlyItemsAreReported(t *testing.T) {
	nopane := testSession()
	nopane.Pane = ""
	var events []RestartEvent
	var clock fakeClock
	env := newFakeEnv()
	finals := RunRestart(context.Background(), env, PlanRestart([]OutdatedSession{nopane}, []string{testSID, "eeee"}), clock.opts(&events))
	if len(finals) != 2 || finals[0].State != StateSkipped || finals[0].Manual == "" || finals[1].SessionID != "eeee" || finals[1].State != StateSkipped {
		t.Fatalf("finals = %+v", finals)
	}
	if env.terminated != 0 {
		t.Fatal("touched a session with no pane")
	}
}

func TestSystemRestartEnv_Relaunch(t *testing.T) {
	run := &exec.FakeRunner{}
	env := SystemRestartEnv{Runner: run, RelaunchLine: func(id string) (string, error) { return "'/x/forgectl' 'resume' '" + id + "'", nil }}
	if err := env.Relaunch(context.Background(), testPane, testSID); err != nil {
		t.Fatal(err)
	}
	want := "herdr pane run w7P:p1 '/x/forgectl' 'resume' '" + testSID + "'"
	if len(run.Calls) != 1 {
		t.Fatalf("calls = %+v", run.Calls)
	}
	// The whole line is ONE argument: herdr joins its arguments with spaces
	// and types them, so splitting here would lose the quoting.
	if c := run.Calls[0]; c.Name+" "+strings.Join(c.Args, " ") != want || len(c.Args) != 4 {
		t.Fatalf("call = %+v, want %q as four args", c, want)
	}
	for name, tc := range map[string][2]string{
		"flag-shaped session id": {testPane, "-c"},
		"non-hex session id":     {testPane, "abc; rm -rf ~"},
		"flag-shaped pane":       {"-x", testSID},
		"pane with a space":      {"w7P p1", testSID},
	} {
		if err := env.Relaunch(context.Background(), tc[0], tc[1]); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if len(run.Calls) != 1 {
		t.Errorf("a refused relaunch reached herdr: %+v", run.Calls)
	}
}

func TestRunRestart_BusyAndRefusedSessionsCostNoPaneReads(t *testing.T) {
	busy := newFakeEnv()
	busy.obs.Entry.Status = "busy"
	runOne(t, context.Background(), busy, nil)
	if busy.paneReads+busy.screenReads != 0 {
		t.Errorf("busy session: %d pane and %d screen reads; want none", busy.paneReads, busy.screenReads)
	}
	mislabelled := newFakeEnv()
	mislabelled.obs.Pane.AgentSession = "ffff"
	runOne(t, context.Background(), mislabelled, nil)
	if mislabelled.screenReads != 0 {
		t.Errorf("refused pane: %d screen reads; want none", mislabelled.screenReads)
	}
}

func TestRunRestart_CancelBeforeTheFirstCheckHasACleanDetail(t *testing.T) {
	env := newFakeEnv()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	final, _ := runOne(t, ctx, env, nil)
	if final.State != StateLeft || strings.Contains(final.Detail, "()") || env.terminated != 0 {
		t.Fatalf("final = %+v, terminated = %d", final, env.terminated)
	}
}
