package resume

import (
	"context"
	"errors"
	"fmt"
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
	relaunchLanded bool   // the line reached the pane although pane run returned relaunchErr
	noRegister     bool   // the relaunched session never registers
	prepareErr     error  // snapshot cannot record the session
	onTerminate    func() // runs inside Terminate (e.g. a Ctrl-C landing there)
	statusSeq      []string
	screenSeq      []string       // consumed one per Screen call, then obs.Screen
	afterPrepare   func(*fakeEnv) // runs inside Prepare: the world changes before the re-check
	onPane         func(call int) // runs at the start of each Pane call (1-based)
	newShellAfter  bool           // after the stop, a different shell owns the pane id
	liveElsewhere  bool           // after the stop, the session is already running as another pid
	lingerFile     bool           // the pid exits but its registry file stays
	clearErr       error          // send-keys fails
	fgLostOnClear  bool           // the operator starts a command just as the line is cleared
	cleared        bool

	// Record.
	terminated, relaunched, prepared int
	paneReads, screenReads           int
	stopped                          bool
	order                            []string
}

func newFakeEnv() *fakeEnv { return &fakeEnv{obs: readyObservation()} }

func (f *fakeEnv) ReadEntry(int) (RegistryEntry, bool) {
	if f.stopped && !f.lingerFile {
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
	if f.onPane != nil {
		f.onPane(f.paneReads)
	}
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
		if f.newShellAfter {
			st.ShellPID = 99999
		}
		if !f.noShellReturn && (!f.cleared || !f.fgLostOnClear) {
			st.ForegroundPGID = st.ShellPID
		}
		return st, nil
	}
	return f.obs.Pane, f.obs.PaneErr
}
func (f *fakeEnv) Screen(context.Context, string) (string, error) {
	f.screenReads++
	if len(f.screenSeq) > 0 {
		sc := f.screenSeq[0]
		f.screenSeq = f.screenSeq[1:]
		return sc, nil
	}
	return f.obs.Screen, f.obs.ScreenErr
}
func (f *fakeEnv) ClearInput(ctx context.Context, _ string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.order = append(f.order, "clear")
	f.cleared = true
	return f.clearErr
}
func (f *fakeEnv) Prepare(string) error {
	f.prepared++
	f.order = append(f.order, "prepare")
	if f.afterPrepare != nil {
		f.afterPrepare(f)
	}
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
	if f.liveElsewhere && f.stopped {
		return RegistryEntry{Pid: 777, SessionID: testSID, Live: true}, true
	}
	if f.relaunched == 0 || f.noRegister || (f.relaunchErr != nil && !f.relaunchLanded) {
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
	if got := strings.Join(env.order, ","); got != "prepare,terminate,clear,relaunch" {
		t.Errorf("order = %s; want the snapshot before the stop, and the line cleared right before the relaunch", got)
	}
}

func TestRunRestart_RefusalsNeverSignal(t *testing.T) {
	tests := map[string]func(*fakeEnv){
		"pid reused by a non-claude": func(f *fakeEnv) { f.obs.Proc.ExecPath = "/bin/sleep" },
		"pid reused by a new claude": func(f *fakeEnv) { f.obs.Proc.Start = f.obs.Proc.Start.Add(time.Hour) },
		"registry names another id":  func(f *fakeEnv) { f.obs.Entry.SessionID = "ffff" },
		"vanished pane":              func(f *fakeEnv) { f.obs.PaneErr = ErrPaneGone },
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

// errTimedOutSend is a pane run killed at its bound, as SystemRestartEnv
// reports it.
var errTimedOutSend = fmt.Errorf("herdr pane run: %w after 10s: signal: killed", ErrHerdrTimeout)

// A pane run killed at its bound may have typed the line before it died, so
// the run waits for the session instead of reporting a failed send
// (forgectl#951).
func TestRunRestart_TimedOutSendWaitsForTheSession(t *testing.T) {
	env := newFakeEnv()
	env.relaunchErr, env.relaunchLanded = errTimedOutSend, true
	final, _ := runOne(t, context.Background(), env, nil)
	if final.State != StateResumed || !strings.Contains(final.Detail, "pid 95294") {
		t.Fatalf("final = %+v; a timed-out send whose line landed is a resume", final)
	}

	env = newFakeEnv()
	env.relaunchErr = errTimedOutSend
	final, _ = runOne(t, context.Background(), env, nil)
	if final.State != StateFailed || final.Manual != ManualResume(testSID) ||
		!strings.Contains(final.Detail, "may or may not have arrived") || !strings.Contains(final.Detail, "check the pane") {
		t.Fatalf("final = %+v; want failed, delivery unknown, with the by-hand command", final)
	}
}

// Any send failure other than the bound is final: the run does not wait, even
// when a session would register.
func TestRunRestart_OrdinarySendFailureIsFinal(t *testing.T) {
	env := newFakeEnv()
	env.relaunchErr, env.relaunchLanded = errors.New("herdr down"), true
	final, _ := runOne(t, context.Background(), env, nil)
	if final.State != StateFailed || final.Manual != ManualResume(testSID) ||
		!strings.Contains(final.Detail, "failed (herdr down)") {
		t.Fatalf("final = %+v; want the send failure reported as before", final)
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
	env, err := NewSystemRestartEnv(Paths{}, run, "/x/forgectl")
	if err != nil {
		t.Fatal(err)
	}
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
	if err := env.ClearInput(context.Background(), testPane); err != nil {
		t.Fatal(err)
	}
	if c := run.Calls[1]; strings.Join(c.Args, " ") != "pane send-keys w7P:p1 ctrl+u" {
		t.Errorf("clear = %+v", c)
	}
}

// I-6: the check after Prepare is the one immediately before the signal. A
// session that turns unsafe between the two checks must not be signalled.
func TestRunRestart_RecheckAfterPrepareBlocksTheSignal(t *testing.T) {
	for name, change := range map[string]func(*fakeEnv){
		"turned busy":      func(f *fakeEnv) { f.obs.Entry.Status = "busy" },
		"draft typed":      func(f *fakeEnv) { f.obs.Screen = screen("❯ just started typing") },
		"identity changed": func(f *fakeEnv) { f.obs.Proc.ExecPath = "/bin/sleep" },
	} {
		t.Run(name, func(t *testing.T) {
			env := newFakeEnv()
			env.afterPrepare = change
			final, _ := runOne(t, context.Background(), env, nil)
			if env.terminated != 0 || env.prepared != 1 {
				t.Fatalf("terminated=%d prepared=%d (final %+v); a change after Prepare must stop the signal", env.terminated, env.prepared, final)
			}
		})
	}
}

// N-2: a session that flaps between the two checks is prepared once.
func TestRunRestart_PreparesOncePerSession(t *testing.T) {
	env := newFakeEnv()
	env.screenSeq = []string{screen("❯"), screen("❯ typing"), screen("❯"), screen("❯")}
	final, _ := runOne(t, context.Background(), env, nil)
	if final.State != StateResumed || env.prepared != 1 {
		t.Fatalf("final=%+v prepared=%d; want resumed after a single Prepare", final, env.prepared)
	}
}

// I-5: a cancel landing inside a herdr call is a cancel, not a closed pane.
func TestRunRestart_CancelDuringACheckIsNotASkip(t *testing.T) {
	// Pane is read once per observe; call 1 is the first check, call 2 the
	// re-check after Prepare.
	for _, call := range []int{1, 2} {
		env := newFakeEnv()
		ctx, cancel := context.WithCancel(context.Background())
		env.onPane = func(n int) {
			if n == call {
				cancel()
			}
		}
		final, _ := runOne(t, ctx, env, nil)
		if final.State != StateLeft || !strings.Contains(final.Detail, reasonCancelled) || env.terminated != 0 {
			t.Errorf("cancel in pane read %d: final=%+v terminated=%d; want left (cancelled), never skipped or signalled", call, final, env.terminated)
		}
	}
}

func TestRunRestart_TransientHerdrErrorWaitsNotRefuses(t *testing.T) {
	env := newFakeEnv()
	env.obs.PaneErr = errors.New("herdr: connection refused")
	final, _ := runOne(t, context.Background(), env, nil)
	if final.State != StateLeft || env.terminated != 0 {
		t.Fatalf("final=%+v; a herdr hiccup must wait, not refuse for good", final)
	}
}

// Security nit 1: the relaunch goes only to a pane still owned by the shell
// the pre-stop check saw.
func TestRunRestart_ReusedPaneIDGetsNoRelaunch(t *testing.T) {
	env := newFakeEnv()
	env.newShellAfter = true
	final, _ := runOne(t, context.Background(), env, nil)
	if final.State != StateFailed || env.relaunched != 0 || final.Manual == "" {
		t.Fatalf("final=%+v relaunched=%d", final, env.relaunched)
	}
}

// I-4: resumed elsewhere during the gap.
func TestRunRestart_AlreadyRunningAgainIsNotRelaunched(t *testing.T) {
	env := newFakeEnv()
	env.liveElsewhere = true
	final, _ := runOne(t, context.Background(), env, nil)
	if final.State != StateFailed || env.relaunched != 0 || !strings.Contains(final.Detail, "already running again") {
		t.Fatalf("final=%+v relaunched=%d", final, env.relaunched)
	}
}

// I-2: the input line is cleared, and a shell that loses the foreground at
// that moment gets no relaunch line.
func TestRunRestart_ClearInputGuards(t *testing.T) {
	env := newFakeEnv()
	env.clearErr = errors.New("send-keys failed")
	if final, _ := runOne(t, context.Background(), env, nil); final.State != StateFailed || env.relaunched != 0 {
		t.Errorf("clear error: final=%+v relaunched=%d", final, env.relaunched)
	}
	env = newFakeEnv()
	env.fgLostOnClear = true
	if final, _ := runOne(t, context.Background(), env, nil); final.State != StateFailed || env.relaunched != 0 {
		t.Errorf("foreground lost on clear: final=%+v relaunched=%d", final, env.relaunched)
	}
}

// N-5.
func TestRunRestart_ExitedButLeftItsRegistryFile(t *testing.T) {
	env := newFakeEnv()
	env.lingerFile = true
	final, _ := runOne(t, context.Background(), env, nil)
	if final.State != StateFailed || env.relaunched != 0 || !strings.Contains(final.Detail, "exited but left its registry file") {
		t.Fatalf("final=%+v relaunched=%d", final, env.relaunched)
	}
}

// I-1: a SystemRestartEnv with no relaunch binary refuses in Prepare, before
// any signal.
type prepareThroughSystem struct {
	*fakeEnv
	sys SystemRestartEnv
}

func (p prepareThroughSystem) Prepare(id string) error {
	p.prepared++
	return p.sys.Prepare(id)
}

func TestRunRestart_UnrenderableRelaunchRefusesBeforeTheSignal(t *testing.T) {
	// The store already holds the session, so the relaunch line is the only
	// thing Prepare can refuse on — the test reaches that guard and no other.
	p := Paths{ClaudeHome: t.TempDir(), StoreDir: t.TempDir()}
	if err := Save(p.StoreDir, &Record{ID: testSID, Cwd: "/w", LastSeen: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := (SystemRestartEnv{paths: p, runner: &exec.FakeRunner{}, forgectl: "/x/forgectl"}).Prepare(testSID); err != nil {
		t.Fatalf("control: a renderable line with a stored session must prepare: %v", err)
	}
	env := newFakeEnv()
	var events []RestartEvent
	var clock fakeClock
	finals := RunRestart(context.Background(), prepareThroughSystem{env, SystemRestartEnv{paths: p, runner: &exec.FakeRunner{}}},
		PlanRestart([]OutdatedSession{testSession()}, nil), clock.opts(&events))
	if env.terminated != 0 || finals[0].State != StateSkipped || !strings.Contains(finals[0].Detail, "not stopped") {
		t.Fatalf("terminated=%d final=%+v", env.terminated, finals[0])
	}
}

func TestNewSystemRestartEnv_RefusesABadBinary(t *testing.T) {
	run := &exec.FakeRunner{}
	for name, path := range map[string]string{
		"empty":        "",
		"relative":     "bin/forgectl",
		"single quote": "/Users/o'brien/bin/forgectl",
		"newline":      "/tmp/x\nrm -rf ~/forgectl",
	} {
		if _, err := NewSystemRestartEnv(Paths{}, run, path); err == nil {
			t.Errorf("%s: accepted %q", name, path)
		}
	}
	if _, err := NewSystemRestartEnv(Paths{}, nil, "/x/forgectl"); err == nil {
		t.Error("nil runner: accepted")
	}
	if _, err := NewSystemRestartEnv(Paths{}, run, "/x/forgectl"); err != nil {
		t.Errorf("good path refused: %v", err)
	}
}

func TestPaneNotFound(t *testing.T) {
	gone := &exec.CommandError{Stderr: `{"error":{"code":"pane_not_found","message":"pane w0Q:p99 not found"},"id":"cli:pane:get"}`}
	if !paneNotFound(fmt.Errorf("wrapped: %w", gone)) {
		t.Error("herdr's pane_not_found not recognized")
	}
	for _, err := range []error{
		&exec.CommandError{Stderr: `{"error":{"code":"workspace_not_found"}}`},
		&exec.CommandError{Stderr: "pane_not_found"}, // prose, not the structured code
		errors.New("pane_not_found"),
		context.Canceled,
	} {
		if paneNotFound(err) {
			t.Errorf("%v read as pane_not_found", err)
		}
	}
}

// TestRestartHerdrBin pins that a set HerdrBin is the binary every herdr call
// runs, so the watcher never resolves herdr through its own PATH.
func TestRestartHerdrBin(t *testing.T) {
	run := &exec.FakeRunner{RunFunc: func(string, []string) (string, error) { return "", errors.New("stop") }}
	req := RestartRequest{Runner: run, DryRun: true, HerdrBin: "/opt/tools/herdr"}
	req.fill()
	env, err := req.Env("")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = env.Pane(context.Background(), "w1:p1")
	if len(run.Calls) == 0 || run.Calls[0].Name != "/opt/tools/herdr" {
		t.Fatalf("calls %+v", run.Calls)
	}
	if _, err := RestartOutdated(context.Background(), RestartRequest{HerdrBin: "herdr"}); err == nil {
		t.Fatal("a relative herdr path was accepted")
	}
}
