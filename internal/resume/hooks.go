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
// the wait for a version that keeps moving; the next watcher trigger (the
// symlink moving again) starts a fresh run.
const (
	DefaultSettleDelay  = 3 * time.Second
	DefaultSettleRounds = 5
)

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
	// ChangeNone: the installed version is the one already recorded.
	ChangeNone ChangeKind = "unchanged"
	// ChangeUnsettled: the version kept moving through every settle round.
	ChangeUnsettled ChangeKind = "unsettled"
	// ChangeUpdated: a settled version differs from the recorded one.
	ChangeUpdated ChangeKind = "updated"
)

// Decision is a run's verdict on the installed version.
type Decision struct {
	Kind ChangeKind
	Old  string // the recorded version; "" on a baseline
	New  string // the settled version; "" when unsettled
}

// Decide compares the recorded version with the settled read. It is the
// whole change-detection rule, kept pure so every branch has a table test.
func Decide(recorded string, haveRecord bool, settled string, isSettled bool) Decision {
	switch {
	case !isSettled:
		return Decision{Kind: ChangeUnsettled, Old: recorded}
	case !haveRecord:
		return Decision{Kind: ChangeBaseline, New: settled}
	case recorded == settled:
		return Decision{Kind: ChangeNone, Old: recorded, New: settled}
	default:
		return Decision{Kind: ChangeUpdated, Old: recorded, New: settled}
	}
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

// HooksRequest is one `resume hooks run`. Zero seams mean production.
type HooksRequest struct {
	Harness string
	Hooks   []HookSpec
	// Dir is forgectl's hook state directory (config.ResumeHooksDir).
	Dir    string
	DryRun bool
	// ReadVersion resolves the harness's installed version.
	ReadVersion func(context.Context) (string, error)
	Runner      exec.Runner
	Restart     RestartFunc
	// Log receives one line per decision and per hook; the launchd log under
	// the watcher.
	Log io.Writer

	SettleDelay  time.Duration
	SettleRounds int
	Sleep        func(context.Context, time.Duration) error
	Now          func() time.Time
	// Store persists the recorded versions and the audit trail.
	Store HookStore
}

// HooksResult is what a run decided and did.
type HooksResult struct {
	Decision Decision
	// Planned is the hooks that would fire (dry run) or did fire.
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
// with the one recorded, and on a change run every hook for the harness.
//
// Order matters for crash safety. The new version is recorded only after
// every hook has run, so a run killed partway re-fires on the next trigger
// rather than silently skipping the update; the cost is that a hook may run
// twice for one update, which is why hooks should be safe to repeat (restart
// is: a session already current is no longer outdated). Hooks run in config
// order and one failing never stops the next. A real run holds the hooks
// lock for its whole life, so a manual run and the watcher's are serialized.
func RunHooks(ctx context.Context, req HooksRequest) (HooksResult, error) {
	req.fill()
	if req.ReadVersion == nil {
		return HooksResult{}, errors.New("no installed-version reader")
	}
	var planned []HookSpec
	for _, h := range req.Hooks {
		if h.Harness == req.Harness {
			planned = append(planned, h)
		}
	}

	if !req.DryRun {
		release, err := req.Store.Lock(ctx)
		if err != nil {
			return HooksResult{}, err
		}
		defer release()
	}

	recorded, haveRecord, err := req.Store.Recorded(req.Harness)
	if err != nil {
		return HooksResult{}, err
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
		return HooksResult{}, fmt.Errorf("read the installed %s version: %w", req.Harness, err)
	}
	d := Decide(recorded, haveRecord, settled, isSettled)
	res := HooksResult{Decision: d}
	logf(req.Log, "%s: %s", req.Harness, describeDecision(d, req.DryRun))

	switch d.Kind {
	case ChangeBaseline:
		if !req.DryRun {
			if err := req.Store.Record(req.Harness, d.New); err != nil {
				return res, err
			}
		}
		return res, nil
	case ChangeNone, ChangeUnsettled:
		return res, nil
	}

	res.Planned = planned
	if req.DryRun {
		return res, nil
	}
	hr := hookRunner{runner: req.Runner, restart: req.Restart, now: req.Now}
	for _, h := range planned {
		logf(req.Log, "%s: running hook %s", req.Harness, h.Identity())
		run := hr.runHook(ctx, h, d)
		res.Runs = append(res.Runs, run)
		logf(req.Log, "%s: hook %s: %s (exit %d, %dms)%s", req.Harness, run.Hook, run.Outcome, run.Exit, run.DurationMS, detailSuffix(run.Detail))
		if err := req.Store.Append(run); err != nil {
			// The hook already ran; losing its record must not skip the rest.
			logf(req.Log, "%s: could not record hook %s in the audit trail: %s", req.Harness, run.Hook, termsafe.SafeLine(err.Error()))
		}
	}
	if err := ctx.Err(); err != nil {
		// Cancelled partway: the later hooks failed without really running,
		// so the update is left unrecorded for the next run to fire again.
		return res, fmt.Errorf("run interrupted; %s %s left unrecorded so the next run fires again: %w", req.Harness, termsafe.SafeLine(d.New), err)
	}
	if err := req.Store.Record(req.Harness, d.New); err != nil {
		return res, err
	}
	return res, nil
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
		return "installed version " + termsafe.SafeLine(d.New) + " is unchanged"
	case ChangeUnsettled:
		return "installed version is still changing; firing nothing until it settles (the next change triggers a new run)"
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
