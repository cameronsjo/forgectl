package resume

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// RestartEnv is every side effect a restart run has, so the sequencing below
// is testable without a live claude, herdr, or signal.
type RestartEnv interface {
	// ReadEntry re-reads a pid's registry file.
	ReadEntry(pid int) (RegistryEntry, bool)
	// Alive probes a pid.
	Alive(pid int) bool
	// Identity reads a pid's exec path and kernel start time.
	Identity(pid int) (ProcIdentity, error)
	// Pane reads herdr's view of a pane.
	Pane(ctx context.Context, pane string) (PaneState, error)
	// Screen reads a pane's visible text.
	Screen(ctx context.Context, pane string) (string, error)
	// Prepare captures what the session's exit would destroy (its /rename name
	// and task bodies) and confirms `forgectl resume <id>` will still find the
	// session once its registry file is gone.
	Prepare(sessionID string) error
	// Terminate sends SIGTERM.
	Terminate(pid int) error
	// ClearInput kills whatever is on the pane's shell input line — keys the
	// operator typed after the stop, or bytes claude left unread in the tty
	// queue — so they cannot merge into the relaunch line.
	ClearInput(ctx context.Context, pane string) error
	// Relaunch types `forgectl resume <id>` into a pane and presses Enter.
	// An error wrapping ErrHerdrTimeout means the line may or may not have
	// been delivered; any other error means it was not.
	Relaunch(ctx context.Context, pane, sessionID string) error
	// LiveSession returns the live registry entry holding a session id.
	LiveSession(sessionID string) (RegistryEntry, bool)
}

// RestartState names a progress line's state.
type RestartState string

// The states a session moves through. Waiting is the only non-final one.
const (
	StateWaiting    RestartState = "waiting"
	StateRestarting RestartState = "restarting"
	StateResumed    RestartState = "resumed"
	StateSkipped    RestartState = "skipped"
	StateFailed     RestartState = "failed"
	// StateLeft: the timeout or a cancel ended the run while the session was
	// still waiting. It was never signalled and is still running.
	StateLeft RestartState = "left"
)

// RestartEvent is one progress line: a session entering a state.
type RestartEvent struct {
	SessionID string
	State     RestartState
	Detail    string
	// Manual is the by-hand command, set whenever this run did not resume the
	// session and a person may want to.
	Manual string
}

// RestartOptions bounds a run.
type RestartOptions struct {
	// Timeout is how long waiting sessions are re-checked before the run gives
	// up on them and leaves them running.
	Timeout time.Duration
	// Poll is the interval between re-checks of waiting sessions.
	Poll time.Duration
	// StopWait bounds the wait for a signalled pid to exit and its registry
	// file to disappear.
	StopWait time.Duration
	// ReadyWait bounds the wait for the pane's shell to take the foreground
	// back after the stop.
	ReadyWait time.Duration
	// ConfirmWait bounds the wait for the resumed session to register.
	ConfirmWait time.Duration
	// Tick is the poll interval inside the bounded waits.
	Tick time.Duration

	Now      func() time.Time
	Sleep    func(ctx context.Context, d time.Duration) error
	Progress func(RestartEvent)
}

// Defaults for RestartOptions' bounded waits. StopWait: a SIGTERM'd claude
// was measured exiting at once, so 15s is generous. ConfirmWait: a resumed
// session registers when claude starts, which is seconds, not tens of them.
const (
	DefaultRestartTimeout = 30 * time.Minute
	DefaultRestartPoll    = 4 * time.Second
	DefaultStopWait       = 15 * time.Second
	DefaultReadyWait      = 10 * time.Second
	DefaultConfirmWait    = 30 * time.Second
	DefaultRestartTick    = 250 * time.Millisecond
)

// SleepContext waits d or until ctx is done, whichever is first.
func SleepContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (o *RestartOptions) fill() {
	if o.Timeout <= 0 {
		o.Timeout = DefaultRestartTimeout
	}
	if o.Poll <= 0 {
		o.Poll = DefaultRestartPoll
	}
	if o.StopWait <= 0 {
		o.StopWait = DefaultStopWait
	}
	if o.ReadyWait <= 0 {
		o.ReadyWait = DefaultReadyWait
	}
	if o.ConfirmWait <= 0 {
		o.ConfirmWait = DefaultConfirmWait
	}
	if o.Tick <= 0 {
		o.Tick = DefaultRestartTick
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Sleep == nil {
		o.Sleep = SleepContext
	}
	if o.Progress == nil {
		o.Progress = func(RestartEvent) {}
	}
}

// RunRestart carries out a plan and returns each session's final event, in
// plan order.
//
// One goroutine, one session at a time: waiting sessions are re-checked in
// rounds, and a session that turns ready is restarted to completion before the
// next is looked at. Restarts take seconds and waits take minutes, so
// parallelism would buy nothing and cost the ordering guarantees below.
//
// Cancellation (Ctrl-C) stops the waiting, never a restart in flight: once a
// session has been signalled, the stop-relaunch-confirm sequence runs on with
// its own bounded waits, so every session ends either untouched or resumed, or
// reported loudly with the command to resume it by hand.
func RunRestart(ctx context.Context, env RestartEnv, plan []RestartPlanItem, opts RestartOptions) []RestartEvent {
	opts.fill()
	finals := make(map[string]RestartEvent, len(plan))
	finish := func(ev RestartEvent) {
		finals[ev.SessionID] = ev
		opts.Progress(ev)
	}

	var pending []RestartPlanItem
	for _, item := range plan {
		switch item.Action {
		case ActionRestart:
			pending = append(pending, item)
		case ActionManual:
			finish(RestartEvent{SessionID: item.SessionID, State: StateSkipped, Detail: item.Reason, Manual: ManualResume(item.SessionID)})
		default:
			finish(RestartEvent{SessionID: item.SessionID, State: StateSkipped, Detail: item.Reason})
		}
	}

	waiting := map[string]string{} // session id -> last reported wait reason
	prepared := map[string]bool{}  // session id -> Prepare already succeeded
	deadline := opts.Now().Add(opts.Timeout)
	for len(pending) > 0 {
		var still []RestartPlanItem
		for _, item := range pending {
			if ctx.Err() != nil {
				still = append(still, item)
				continue
			}
			ev, done := attemptRestart(ctx, env, item.Session, opts, prepared)
			if done {
				finish(ev)
				continue
			}
			if waiting[item.SessionID] != ev.Detail {
				waiting[item.SessionID] = ev.Detail
				opts.Progress(ev)
			}
			still = append(still, item)
		}
		pending = still
		if len(pending) == 0 {
			break
		}
		var why string
		switch {
		case ctx.Err() != nil:
			why = "cancelled while waiting"
		case !opts.Now().Before(deadline):
			why = fmt.Sprintf("still waiting after %s", opts.Timeout)
		}
		if why != "" {
			for _, item := range pending {
				detail := why
				if w := waiting[item.SessionID]; w != "" {
					detail += " (" + w + ")"
				}
				finish(RestartEvent{
					SessionID: item.SessionID, State: StateLeft,
					Detail: detail + "; never signalled, still running",
					Manual: ManualResume(item.SessionID),
				})
			}
			break
		}
		// An error here is a cancel, handled at the top of the next round.
		_ = opts.Sleep(ctx, opts.Poll)
	}

	out := make([]RestartEvent, 0, len(plan))
	for _, item := range plan {
		out = append(out, finals[item.SessionID])
	}
	return out
}

// observe gathers one Observation. Every read is fresh: nothing a previous
// round saw is reused, because the point is to check immediately before the
// signal.
//
// It stops reading at the first stage Evaluate would stop at, in Evaluate's
// own order, so a busy or refused session costs no herdr calls and its screen
// — another program's output — is not read every poll for nothing. The
// fields left zero are ones Evaluate never reaches.
func observe(ctx context.Context, env RestartEnv, s OutdatedSession) Observation {
	var obs Observation
	obs.Entry, obs.EntryFound = env.ReadEntry(s.Pid)
	obs.Alive = env.Alive(s.Pid)
	obs.Proc, obs.ProcErr = env.Identity(s.Pid)
	if _, ok := checkIdentity(s, obs); !ok || IsBusy(obs.Entry.Status) {
		return obs
	}
	obs.Pane, obs.PaneErr = env.Pane(ctx, s.Pane)
	if _, ok := checkPane(s, obs); !ok || obs.Pane.ScrolledBack {
		return obs
	}
	obs.Screen, obs.ScreenErr = env.Screen(ctx, s.Pane)
	return obs
}

// Preview evaluates the predicate once without acting, for --dry-run. It uses
// only reads.
func Preview(ctx context.Context, env RestartEnv, s OutdatedSession) Check {
	return Evaluate(s, observe(ctx, env, s))
}

// reasonCancelled is the wait reason when a cancel lands during a check. The
// check is abandoned rather than judged: a herdr call cut short by the cancel
// fails, and reading that failure as "pane closed" would report a false skip.
const reasonCancelled = "cancelled during a check"

// attemptRestart evaluates a session and, when it is ready, restarts it. It
// reports done=false only for a session that should be checked again.
func attemptRestart(ctx context.Context, env RestartEnv, s OutdatedSession, opts RestartOptions, prepared map[string]bool) (RestartEvent, bool) {
	waitFor := func(reason string) (RestartEvent, bool) {
		return RestartEvent{SessionID: s.SessionID, State: StateWaiting, Detail: reason}, false
	}
	verdict := func(c Check) (RestartEvent, bool) {
		if c.Readiness == Refused {
			return RestartEvent{SessionID: s.SessionID, State: StateSkipped, Detail: c.Reason, Manual: ManualResume(s.SessionID)}, true
		}
		return waitFor(c.Reason)
	}
	obs := observe(ctx, env, s)
	if ctx.Err() != nil {
		return waitFor(reasonCancelled)
	}
	if c := Evaluate(s, obs); c.Readiness != Ready {
		return verdict(c)
	}
	// Prepared once per session, when it first reads ready, so a long wait
	// does not snapshot every poll; then checked again, so the last check is
	// the one immediately before the signal.
	if !prepared[s.SessionID] {
		if err := env.Prepare(s.SessionID); err != nil {
			return RestartEvent{
				SessionID: s.SessionID, State: StateSkipped,
				Detail: fmt.Sprintf("not stopped: could not prepare its relaunch (%v)", err),
				Manual: ManualResume(s.SessionID),
			}, true
		}
		prepared[s.SessionID] = true
	}
	obs = observe(ctx, env, s)
	if ctx.Err() != nil {
		return waitFor(reasonCancelled)
	}
	if c := Evaluate(s, obs); c.Readiness != Ready {
		return verdict(c)
	}
	return restartNow(ctx, env, s, obs.Pane.ShellPID, opts), true
}

// restartNow stops a session that just passed the predicate and resumes it in
// its pane. It never relaunches unless the old process is confirmed gone and
// no other process has taken the session: two processes on one transcript
// corrupt it. shellPID is the pane's shell as the last pre-stop check saw it;
// the relaunch goes only to a pane still owned by that shell.
func restartNow(ctx context.Context, env RestartEnv, s OutdatedSession, shellPID int, opts RestartOptions) RestartEvent {
	opts.Progress(RestartEvent{SessionID: s.SessionID, State: StateRestarting, Detail: fmt.Sprintf("stopping pid %d in pane %s", s.Pid, paneLabel(s))})
	manual := ManualResume(s.SessionID)
	fail := func(format string, a ...any) RestartEvent {
		return RestartEvent{SessionID: s.SessionID, State: StateFailed, Detail: fmt.Sprintf(format, a...), Manual: manual}
	}

	// From the signal on, cancellation no longer applies: every wait below is
	// bounded instead, so a Ctrl-C cannot strand a stopped session. The
	// polling waits carry their own limits, and each herdr call the env makes
	// is bounded by the env (HerdrCallTimeout for SystemRestartEnv).
	ctx = context.WithoutCancel(ctx)

	if err := env.Terminate(s.Pid); err != nil {
		return fail("could not signal pid %d (%v); not relaunched", s.Pid, err)
	}
	var exited bool
	stopped := waitUntil(opts, opts.StopWait, func() bool {
		if env.Alive(s.Pid) {
			return false
		}
		exited = true
		e, ok := env.ReadEntry(s.Pid)
		return !ok || e.SessionID != s.SessionID
	})
	if !stopped {
		if exited {
			// Claude Code removes its registry file on a clean exit; one that
			// lingers means the exit was not the one measured, so the state is
			// not known well enough to relaunch into.
			return fail("pid %d exited but left its registry file; not relaunched — check pane %s, then resume it by hand", s.Pid, s.Pane)
		}
		return fail("sent SIGTERM, but pid %d did not exit within %s; not relaunched — check pane %s, and once it has exited resume it by hand", s.Pid, opts.StopWait, s.Pane)
	}

	// paneReady: the pane still exists, is owned by the same shell as before
	// the stop (a closed pane's id could be reused by another), and that shell
	// holds the foreground.
	var paneErr error
	paneReady := func() bool {
		st, err := env.Pane(ctx, s.Pane)
		paneErr = err
		return err == nil && st.ShellPID == shellPID && st.ShellForeground()
	}
	if !waitUntil(opts, opts.ReadyWait, paneReady) {
		if paneErr != nil {
			return fail("stopped, but herdr can no longer show pane %s (%s); not relaunched", s.Pane, clipDetail(paneErr.Error()))
		}
		return fail("stopped, but pane %s's own shell did not take the foreground back within %s; not relaunched", s.Pane, opts.ReadyWait)
	}

	// Someone else may have resumed it in the gap — another run, or the
	// operator in another pane. A second process would corrupt the transcript.
	if e, ok := env.LiveSession(s.SessionID); ok {
		return RestartEvent{
			SessionID: s.SessionID, State: StateFailed,
			Detail: fmt.Sprintf("stopped, but it is already running again as pid %d (resumed elsewhere during the gap); not relaunched in pane %s", e.Pid, s.Pane),
		}
	}

	if err := env.ClearInput(ctx, s.Pane); err != nil {
		return fail("stopped, but clearing pane %s's input line failed (%v); not relaunched", s.Pane, err)
	}
	// The line-kill itself cannot start a command, but the operator can press
	// Enter at any moment; check the shell still holds the foreground last.
	if !paneReady() {
		return fail("stopped, but pane %s's shell lost the foreground just before the relaunch (someone started a command?); not relaunched", s.Pane)
	}
	// A send killed at its bound may already have typed the line, so it
	// waits for the session like a send that returned; only the report
	// differs if nothing registers. Any other send failure is final.
	var sendErr error
	if err := env.Relaunch(ctx, s.Pane, s.SessionID); err != nil {
		if !errors.Is(err, ErrHerdrTimeout) {
			return fail("stopped, but sending `%s` to pane %s failed (%v)", manual, s.Pane, err)
		}
		sendErr = err
	}

	var resumed RegistryEntry
	confirmed := waitUntil(opts, opts.ConfirmWait, func() bool {
		e, ok := env.LiveSession(s.SessionID)
		if ok && e.Pid != s.Pid {
			resumed = e
			return true
		}
		return false
	})
	if !confirmed && sendErr != nil {
		return fail("stopped, but sending `%s` to pane %s timed out and may or may not have arrived (%v); no live session registered within %s — check the pane before running it by hand", manual, s.Pane, sendErr, opts.ConfirmWait)
	}
	if !confirmed {
		return fail("stopped, and `%s` was sent to pane %s, but no live session registered within %s — check the pane", manual, s.Pane, opts.ConfirmWait)
	}
	detail := fmt.Sprintf("pid %d, version %s, pane %s", resumed.Pid, resumed.Version, s.Pane)
	if v, err := ParseVersion(resumed.Version); err == nil {
		if iv, err := ParseVersion(s.InstalledVersion); err == nil && v.Compare(iv) < 0 {
			detail += fmt.Sprintf(" — still older than the installed %s (does the launch profile pin a binary?)", s.InstalledVersion)
		}
	}
	return RestartEvent{SessionID: s.SessionID, State: StateResumed, Detail: detail}
}

// waitUntil polls cond every opts.Tick until it holds or limit elapses. It
// ignores cancellation by design; see restartNow.
func waitUntil(opts RestartOptions, limit time.Duration, cond func() bool) bool {
	start := opts.Now()
	for {
		if cond() {
			return true
		}
		if opts.Now().Sub(start) >= limit {
			return false
		}
		_ = opts.Sleep(context.Background(), opts.Tick)
	}
}
