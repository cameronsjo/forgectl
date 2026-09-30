package resume

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/redact"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// The environment a command hook receives. The values never reach its argv:
// a version string is disk-sourced, and argv is visible in ps.
const (
	HookEnvHarness    = "FORGECTL_HARNESS"
	HookEnvOldVersion = "FORGECTL_OLD_VERSION"
	HookEnvNewVersion = "FORGECTL_NEW_VERSION"
)

// Built-in timeouts for a hook that sets no timeout_seconds. The restart
// action keeps restart's own default, since it waits for busy sessions.
const DefaultCommandHookTimeout = 5 * time.Minute

// Settling. An update can move the symlink more than once, so a version is
// acted on only once two reads settleDelay apart agree. settleRounds bounds
// the wait for a version that keeps moving; RunHooks then checks again
// itself (maxHookPasses) rather than counting on another trigger.
const (
	DefaultSettleDelay  = 3 * time.Second
	DefaultSettleRounds = 5
)

// maxHookPasses bounds how many times one run re-checks the version: after
// acting, a run re-reads it, and a version that moved again during the run
// (an update landing while a restart waited) is handled in the same run.
const maxHookPasses = 3

// MaxRestartAttempts caps how often a restart hook that ended incomplete is
// retried for one version, so a session that stays busy for good does not
// hold a restart's whole wait on every later trigger.
const MaxRestartAttempts = 3

// HookSpec is one validated [[resume.on_update]] entry.
type HookSpec struct {
	// Index is the entry's 1-based position in config.toml, its identity in
	// logs and the audit trail.
	Index   int
	Harness string
	Action  string   // "restart", or "" for a command hook
	Command []string // argv, or nil for an action
	Timeout time.Duration
}

// HookSpecs converts validated config entries, applying default timeouts.
func HookSpecs(hooks []config.OnUpdateHook) []HookSpec {
	out := make([]HookSpec, 0, len(hooks))
	for i, h := range hooks {
		s := HookSpec{Index: i + 1, Harness: h.Harness, Action: h.Action, Command: h.Command}
		switch {
		case h.TimeoutSeconds > 0:
			s.Timeout = time.Duration(h.TimeoutSeconds) * time.Second
		case h.Action != "":
			s.Timeout = DefaultRestartTimeout
		default:
			s.Timeout = DefaultCommandHookTimeout
		}
		out = append(out, s)
	}
	return out
}

// Identity names a hook in logs and the audit trail: its position and kind,
// and for a command only the program's base name. The arguments are left
// out: an operator may pass a token or a webhook URL as one, and the audit
// trail outlives the run.
func (h HookSpec) Identity() string {
	if h.Action != "" {
		return fmt.Sprintf("#%d action=%s", h.Index, h.Action)
	}
	prog := ""
	if len(h.Command) > 0 {
		prog = filepath.Base(h.Command[0])
	}
	return fmt.Sprintf("#%d command=%s", h.Index, termsafe.SafeLineMax(prog, 64))
}

// HookEnv is the environment a command hook runs with, on top of the
// watcher's own.
func HookEnv(harness, oldVersion, newVersion string) map[string]string {
	return map[string]string{
		HookEnvHarness:    harness,
		HookEnvOldVersion: oldVersion,
		HookEnvNewVersion: newVersion,
	}
}

// ChangeKind is what a run decided about the installed version.
type ChangeKind string

const (
	// ChangeBaseline: nothing recorded yet. The version is recorded and no
	// hook fires — there is no "old" version to report, and an install is
	// not an update.
	ChangeBaseline ChangeKind = "baseline"
	// ChangeNone: the installed version is the one already recorded, and no
	// restart is left to retry.
	ChangeNone ChangeKind = "unchanged"
	// ChangeUnsettled: the version kept moving through every settle round.
	ChangeUnsettled ChangeKind = "unsettled"
	// ChangeUpdated: a settled version differs from the recorded one; every
	// hook fires.
	ChangeUpdated ChangeKind = "updated"
	// ChangeRetry: the version is unchanged, but a restart hook for it ended
	// incomplete last time; only those restart hooks run again. Command
	// hooks fire once per version and never retry.
	ChangeRetry ChangeKind = "retry"
)

// HarnessState is what the store keeps per harness.
type HarnessState struct {
	// Version is the last version whose hooks all ran.
	Version string `json:"version"`
	// Pending names the restart hooks (by Identity) that ended incomplete or
	// failed for Version, to be retried.
	Pending []string `json:"pending,omitempty"`
	// Attempts counts the restart attempts made for Version while Pending
	// was non-empty.
	Attempts int `json:"attempts,omitempty"`
}

// Decision is a run's verdict on the installed version.
type Decision struct {
	Kind ChangeKind
	Old  string // the recorded version; "" on a baseline
	New  string // the settled version; "" when unsettled
	// Retry is the pending restart hooks a ChangeRetry re-runs, and GaveUp
	// names pending hooks past MaxRestartAttempts (reported, not run).
	Retry    []string
	GaveUp   []string
	Attempts int
}

// Decide compares the recorded state with the settled read. It is the whole
// change-detection rule, kept pure so every branch has a table test.
func Decide(st HarnessState, haveRecord bool, settled string, isSettled bool) Decision {
	switch {
	case !isSettled:
		return Decision{Kind: ChangeUnsettled, Old: st.Version}
	case !haveRecord:
		return Decision{Kind: ChangeBaseline, New: settled}
	case st.Version != settled:
		return Decision{Kind: ChangeUpdated, Old: st.Version, New: settled}
	case len(st.Pending) > 0 && st.Attempts < MaxRestartAttempts:
		return Decision{Kind: ChangeRetry, Old: st.Version, New: settled, Retry: st.Pending, Attempts: st.Attempts}
	case len(st.Pending) > 0:
		return Decision{Kind: ChangeNone, Old: st.Version, New: settled, GaveUp: st.Pending, Attempts: st.Attempts}
	default:
		return Decision{Kind: ChangeNone, Old: st.Version, New: settled}
	}
}

// NextState is the state a pass saves after its hooks ran: the version, plus
// the restart hooks that did not end ok. Pure, so the retry rule has a table
// test: an updated version starts counting at one attempt, a retry adds one,
// and nothing pending clears the count.
func NextState(d Decision, runs []HookRun, restartIDs map[string]bool) HarnessState {
	st := HarnessState{Version: d.New}
	for _, r := range runs {
		if restartIDs[r.Hook] && r.Outcome != OutcomeOK {
			st.Pending = append(st.Pending, r.Hook)
		}
	}
	if len(st.Pending) > 0 {
		st.Attempts = 1
		if d.Kind == ChangeRetry {
			st.Attempts = d.Attempts + 1
		}
	}
	return st
}

// SettleVersion reads the version until two consecutive reads, delay apart,
// agree, up to rounds re-reads. A change that reverts inside the window
// settles on the reverted value, so the caller sees no change at all. A read
// error ends the attempt: a half-written symlink can fail to resolve, and
// that is a reason to wait for the next trigger, not to act.
func SettleVersion(ctx context.Context, read func(context.Context) (string, error), sleep func(context.Context, time.Duration) error, delay time.Duration, rounds int) (string, bool, error) {
	prev, err := read(ctx)
	if err != nil {
		return "", false, err
	}
	for range rounds {
		if err := sleep(ctx, delay); err != nil {
			return "", false, err
		}
		cur, err := read(ctx)
		if err != nil {
			return "", false, err
		}
		if cur == prev {
			return cur, true, nil
		}
		prev = cur
	}
	return "", false, nil
}

// Hook outcomes in the audit trail.
const (
	OutcomeOK         = "ok"
	OutcomeFailed     = "failed"
	OutcomeTimeout    = "timeout"
	OutcomeIncomplete = "incomplete" // restart ran, and some sessions failed or were left waiting
)

// HookRun is one audit-trail record: one hook's run for one update.
type HookRun struct {
	Time       time.Time `json:"time"`
	Harness    string    `json:"harness"`
	Old        string    `json:"old_version"`
	New        string    `json:"new_version"`
	Hook       string    `json:"hook"`
	Outcome    string    `json:"outcome"`
	Exit       int       `json:"exit"` // the process exit status; -1 when there is none
	DurationMS int64     `json:"duration_ms"`
	// Detail is a short, terminal-safe summary: the restart counts, or a
	// failed command's stderr tail (outputTail). Empty on a clean command.
	Detail string `json:"detail,omitempty"`
	// Trigger is what started the run: "launchd" (the watcher) or "manual".
	Trigger string `json:"trigger,omitempty"`
}

// hookTailRunes caps how much of a failed command's stderr the audit trail
// keeps.
const hookTailRunes = 240

// outputTail is what the audit trail keeps of a failed command's stderr: the
// last hookTailRunes runes, with any line that could hold a URL credential or
// auth header withheld (redact.Text) and control characters escaped. Stdout
// is never kept, and a command that succeeds keeps nothing: hook output can
// carry anything the hook's program prints, the file outlives the run, and
// the one thing the operator needs from it is why a hook failed.
func outputTail(stderr string) string {
	s := strings.TrimSpace(redact.Text(stderr))
	if n := utf8.RuneCountInString(s); n > hookTailRunes {
		r := []rune(s)
		s = "…" + string(r[n-hookTailRunes:])
	}
	return termsafe.SafeLine(s)
}

// RestartFunc runs the built-in restart action with the given timeout.
type RestartFunc func(ctx context.Context, timeout time.Duration) (RestartResult, error)

// hookRunner is what running one hook needs.
type hookRunner struct {
	runner  exec.Runner
	restart RestartFunc
	now     func() time.Time
}

// runHook runs one hook and returns its audit record. It never returns an
// error: every failure is an outcome in the record, so the caller can carry
// on with the next hook.
func (hr hookRunner) runHook(ctx context.Context, h HookSpec, d Decision) HookRun {
	start := hr.now()
	rec := HookRun{Time: start.UTC(), Harness: h.Harness, Old: d.Old, New: d.New, Hook: h.Identity(), Exit: -1}
	if h.Action == config.OnUpdateActionRestart {
		// The timeout goes to restart itself, not to a context deadline: it
		// bounds the waiting, and a restart already signalled must still be
		// relaunched and confirmed after it passes.
		res, err := hr.restart(ctx, h.Timeout)
		switch {
		case err != nil:
			rec.Outcome, rec.Detail = OutcomeFailed, termsafe.SafeLineMax(err.Error(), hookTailRunes)
		case res.Incomplete():
			rec.Outcome, rec.Exit = OutcomeIncomplete, 1
			rec.Detail = fmt.Sprintf("%d session(s) restarted or skipped, %d failed, %d left waiting", len(res.Finals)-res.Failed-res.Left, res.Failed, res.Left)
		default:
			rec.Outcome, rec.Exit = OutcomeOK, 0
			rec.Detail = fmt.Sprintf("%d session(s) restarted or skipped", len(res.Finals))
		}
	} else {
		hctx, cancel := context.WithTimeout(ctx, h.Timeout)
		defer cancel()
		// The arguments are the operator's own and may carry a token, so the
		// Runner's logs and errors show them as flag names only, and scrub
		// their values from the stderr kept for the audit tail.
		hctx = exec.WithOpaqueArgs(hctx, 0, len(h.Command)-1)
		// A hook is non-interactive, so it may leave the terminal's process
		// group: at the deadline its whole group is killed, helpers included.
		hctx = exec.WithProcessGroup(hctx)
		// RunWithEnv: argv goes to the program unchanged, with no shell, and
		// the versions only through the environment.
		_, err := hr.runner.RunWithEnv(hctx, HookEnv(h.Harness, d.Old, d.New), h.Command[0], h.Command[1:]...)
		var ce *exec.CommandError
		switch {
		case err == nil:
			rec.Outcome, rec.Exit = OutcomeOK, 0
		case errors.Is(hctx.Err(), context.DeadlineExceeded):
			rec.Outcome = OutcomeTimeout
			rec.Detail = "killed after " + h.Timeout.String()
		case errors.As(err, &ce):
			rec.Outcome, rec.Exit, rec.Detail = OutcomeFailed, ce.ExitCode, outputTail(ce.Stderr)
			if rec.Detail == "" && ce.ExitCode < 0 {
				// It never ran (a missing program): the reason is on Err.
				rec.Detail = termsafe.SafeLineMax(errors.Unwrap(ce).Error(), hookTailRunes)
			}
		default:
			rec.Outcome, rec.Detail = OutcomeFailed, termsafe.SafeLineMax(err.Error(), hookTailRunes)
		}
	}
	rec.DurationMS = hr.now().Sub(start).Milliseconds()
	return rec
}

// ErrHooksInterrupted reports a run cancelled (a signal) before its hooks
// all ran. The version is left unrecorded so the next run fires again.
var ErrHooksInterrupted = errors.New("hooks run interrupted")

// HooksRequest is one `resume hooks run`. Zero seams mean production.
type HooksRequest struct {
	Harness string
	Hooks   []HookSpec
	// Dir is forgectl's hook state directory (config.ResumeHooksDir).
	Dir    string
	DryRun bool
	// Trigger is stamped on every audit record: "launchd" or "manual".
	Trigger string
	// ReadVersion resolves the harness's installed version.
	ReadVersion func(context.Context) (string, error)
	Runner      exec.Runner
	// Restart runs the restart action. It must honor ctx: a cancelled ctx
	// ends its waiting, and RunHooks then leaves the version unrecorded.
	Restart RestartFunc
	// Log receives one line per decision and per hook; the launchd log under
	// the watcher.
	Log io.Writer

	SettleDelay  time.Duration
	SettleRounds int
	Sleep        func(context.Context, time.Duration) error
	Now          func() time.Time
	// Store persists the recorded state and the audit trail.
	Store HookStore
}

// HooksResult is what a run decided and did.
type HooksResult struct {
	// Decision is the last pass's verdict; Decisions holds every pass's.
	Decision  Decision
	Decisions []Decision
	// Planned is the hooks the last acting pass would fire (dry run) or did.
	Planned []HookSpec
	Runs    []HookRun
}

// Failed reports whether any hook that ran did not end ok.
func (r HooksResult) Failed() bool {
	for _, run := range r.Runs {
		if run.Outcome != OutcomeOK {
			return true
		}
	}
	return false
}

// RunHooks is `resume hooks run`: settle the installed version, compare it
// with the recorded state, and act.
//
//   - A changed version runs every hook for the harness, in config order;
//     one failing never stops the next.
//   - An unchanged version with a restart left incomplete re-runs only that
//     restart (up to MaxRestartAttempts per version); command hooks fire
//     once per version.
//
// The state is saved only after a pass's hooks have all run. A run cancelled
// partway (SIGTERM from launchd, Ctrl-C) stops starting hooks and saves
// nothing, so the next run fires again; a hook can therefore run twice for
// one update, which is why hooks should be safe to repeat. After acting, the
// run re-reads the version and handles an update that landed meanwhile, up
// to maxHookPasses. A real run holds the hooks lock for its whole life, so a
// manual run and the watcher's are serialized.
func RunHooks(ctx context.Context, req HooksRequest) (HooksResult, error) {
	req.fill()
	if req.ReadVersion == nil {
		return HooksResult{}, errors.New("no installed-version reader")
	}
	if !req.DryRun {
		release, err := req.Store.Lock(ctx, func() {
			logf(req.Log, "%s: waiting for another `forgectl resume hooks run` to finish", req.Harness)
		})
		if err != nil {
			return HooksResult{}, err
		}
		defer release()
	}
	var res HooksResult
	for pass := 1; ; pass++ {
		d, planned, runs, err := req.pass(ctx)
		res.Decision, res.Decisions = d, append(res.Decisions, d)
		res.Runs = append(res.Runs, runs...)
		if planned != nil {
			res.Planned = planned
		}
		if err != nil || req.DryRun || pass >= maxHookPasses {
			return res, err
		}
		again := d.Kind == ChangeUnsettled
		if d.Kind != ChangeNone && d.Kind != ChangeUnsettled {
			v, rerr := req.ReadVersion(ctx)
			again = rerr == nil && v != d.New
		}
		if !again {
			return res, nil
		}
		logf(req.Log, "%s: the installed version moved during the run; checking again", req.Harness)
	}
}

// pass is one settle-decide-act round.
func (req HooksRequest) pass(ctx context.Context) (Decision, []HookSpec, []HookRun, error) {
	st, haveRecord, err := req.Store.Load(req.Harness)
	if err != nil {
		return Decision{}, nil, nil, err
	}
	var settled string
	var isSettled bool
	if req.DryRun {
		// A dry run reads once: it previews, and waiting out the settle
		// window would only slow it down.
		settled, err = req.ReadVersion(ctx)
		isSettled = err == nil
	} else {
		settled, isSettled, err = SettleVersion(ctx, req.ReadVersion, req.Sleep, req.SettleDelay, req.SettleRounds)
	}
	if err != nil {
		return Decision{}, nil, nil, fmt.Errorf("read the installed %s version: %w", req.Harness, err)
	}
	d := Decide(st, haveRecord, settled, isSettled)
	logf(req.Log, "%s: %s", req.Harness, describeDecision(d, req.DryRun))

	switch d.Kind {
	case ChangeBaseline:
		if !req.DryRun {
			return d, nil, nil, req.Store.Save(req.Harness, HarnessState{Version: d.New})
		}
		return d, nil, nil, nil
	case ChangeNone, ChangeUnsettled:
		return d, nil, nil, nil
	}

	restartIDs := map[string]bool{}
	retry := map[string]bool{}
	for _, id := range d.Retry {
		retry[id] = true
	}
	planned := []HookSpec{}
	for _, h := range req.Hooks {
		if h.Harness != req.Harness {
			continue
		}
		if h.Action != "" {
			restartIDs[h.Identity()] = true
		}
		if d.Kind == ChangeRetry && (h.Action == "" || !retry[h.Identity()]) {
			continue
		}
		planned = append(planned, h)
	}
	if req.DryRun {
		return d, planned, nil, nil
	}

	hr := hookRunner{runner: req.Runner, restart: req.Restart, now: req.Now}
	var runs []HookRun
	for i, h := range planned {
		if ctx.Err() != nil {
			logf(req.Log, "%s: interrupted; %d hook(s) not run", req.Harness, len(planned)-i)
			break
		}
		logf(req.Log, "%s: running hook %s", req.Harness, h.Identity())
		run := hr.runHook(ctx, h, d)
		run.Trigger = req.Trigger
		runs = append(runs, run)
		logf(req.Log, "%s: hook %s: %s (exit %d, %dms)%s", req.Harness, run.Hook, run.Outcome, run.Exit, run.DurationMS, detailSuffix(run.Detail))
		if err := req.Store.Append(run); err != nil {
			// The hook already ran; losing its record must not skip the rest.
			logf(req.Log, "%s: could not record hook %s in the audit trail: %s", req.Harness, run.Hook, termsafe.SafeLine(err.Error()))
		}
	}
	if err := ctx.Err(); err != nil {
		// Hooks cut short by the cancel did not really run, so nothing is
		// saved: the next run fires the whole set again.
		logf(req.Log, "%s: %s left unrecorded so the next run fires again", req.Harness, termsafe.SafeLine(d.New))
		return d, planned, runs, fmt.Errorf("%w: %s %s left unrecorded so the next run fires again: %w", ErrHooksInterrupted, req.Harness, termsafe.SafeLine(d.New), err)
	}
	next := NextState(d, runs, restartIDs)
	if len(next.Pending) > 0 {
		if next.Attempts >= MaxRestartAttempts {
			logf(req.Log, "%s: restart still incomplete after %d attempt(s); giving up until the next version", req.Harness, next.Attempts)
		} else {
			logf(req.Log, "%s: restart incomplete; it runs again on the next trigger (attempt %d of %d)", req.Harness, next.Attempts+1, MaxRestartAttempts)
		}
	}
	return d, planned, runs, req.Store.Save(req.Harness, next)
}

func (r *HooksRequest) fill() {
	if r.Sleep == nil {
		r.Sleep = SleepContext
	}
	if r.Now == nil {
		r.Now = time.Now
	}
	if r.SettleDelay <= 0 {
		r.SettleDelay = DefaultSettleDelay
	}
	if r.SettleRounds <= 0 {
		r.SettleRounds = DefaultSettleRounds
	}
	if r.Log == nil {
		r.Log = io.Discard
	}
	if r.Store == nil {
		r.Store = FileHookStore{Dir: r.Dir}
	}
	if r.Trigger == "" {
		r.Trigger = "manual"
	}
	if r.Restart == nil {
		r.Restart = func(context.Context, time.Duration) (RestartResult, error) {
			return RestartResult{}, errors.New("no restart action is wired")
		}
	}
}

func describeDecision(d Decision, dryRun bool) string {
	switch d.Kind {
	case ChangeBaseline:
		if dryRun {
			return "no version recorded yet; a real run records " + termsafe.SafeLine(d.New) + " as the baseline and fires nothing"
		}
		return "no version recorded yet; recording " + termsafe.SafeLine(d.New) + " as the baseline, firing nothing"
	case ChangeNone:
		msg := "installed version " + termsafe.SafeLine(d.New) + " is unchanged"
		if len(d.GaveUp) > 0 {
			msg += fmt.Sprintf("; restart %s gave up after %d attempt(s)", termsafe.SafeLine(strings.Join(d.GaveUp, ", ")), d.Attempts)
		}
		return msg
	case ChangeUnsettled:
		return "installed version is still changing; firing nothing until it settles"
	case ChangeRetry:
		return fmt.Sprintf("installed version %s is unchanged; retrying incomplete restart %s (attempt %d of %d)",
			termsafe.SafeLine(d.New), termsafe.SafeLine(strings.Join(d.Retry, ", ")), d.Attempts+1, MaxRestartAttempts)
	default:
		return "updated " + termsafe.SafeLine(d.Old) + " -> " + termsafe.SafeLine(d.New)
	}
}

func detailSuffix(detail string) string {
	if detail == "" {
		return ""
	}
	return ": " + detail
}

// logf writes one timestamped line. A write error is dropped: the log is a
// launchd-owned file, and a full disk must not stop a restart mid-run.
func logf(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, "%s %s\n", time.Now().UTC().Format(time.RFC3339), fmt.Sprintf(format, args...))
}
