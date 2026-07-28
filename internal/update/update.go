// Package update is the ops layer for `forgectl update`: weekly
// package-manager and OS maintenance run as independently-scoped Steps
// (forgectl#25) — a broken step's error is recorded on its own Result and
// never aborts the others. It knows nothing of Cobra — the house pattern
// (see internal/net, internal/clean).
package update

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/cameronsjo/forgectl/internal/exec"
)

// Step is one independently-scoped maintenance action. Check is always safe
// to run — report-only, never mutates anything — and backs `update check`.
// Apply performs the actual maintenance and backs `update run`.
//
// Destructive marks a Step whose Apply mutates installed software or system
// state (brew upgrade, npm update, …). Run skips such a Step's Apply unless
// the caller opts in via Options.Yes, recording it as a Skipped Result
// rather than silently omitting it from the Report — the skip is always
// visible, never quiet.
//
// A Step with no destructive counterpart (softwareupdate: CHECK-ONLY, never
// installs) sets Destructive: false and gives Apply the identical function
// as Check — Run always executes it under `update run`, and "apply" never
// differs from "check" for that Step.
type Step struct {
	Name        string
	Destructive bool
	Check       func(ctx context.Context, run exec.Runner) (string, error)
	Apply       func(ctx context.Context, run exec.Runner) (string, error)
}

// Result is one Step's outcome from a single Run call.
type Result struct {
	Name        string
	Destructive bool

	// Skipped is true only when Destructive && !Options.Yes under `update
	// run` — the step never ran at all this pass. It is recorded, never
	// silently dropped from the Report.
	Skipped    bool
	SkipReason string

	Output   string
	Err      error
	Duration time.Duration
}

// Failed reports whether this Result represents a step that actually ran
// and returned an error. A Skipped result is never Failed.
func (r Result) Failed() bool {
	return !r.Skipped && r.Err != nil
}

// Report is Run's outcome: one Result per selected Step, in roster order.
type Report struct {
	Results []Result
}

// Failed reports whether any Result in the report failed.
func (r Report) Failed() bool {
	for _, res := range r.Results {
		if res.Failed() {
			return true
		}
	}
	return false
}

// Err joins every failed Result's error into one error for the caller's
// exit-code decision. This is the ONLY place per-step errors are combined —
// every other consumer of Report (the CLI's printer, --json) reads Results
// directly, so one step's failure is never hidden by another's. Returns nil
// when nothing failed (errors.Join's own nil-on-empty behavior).
func (r Report) Err() error {
	var errs []error
	for _, res := range r.Results {
		if res.Failed() {
			errs = append(errs, fmt.Errorf("%s: %w", res.Name, res.Err))
		}
	}
	return errors.Join(errs...)
}

// Options configures Run.
type Options struct {
	// Yes gates every Destructive step's Apply under a non-CheckOnly Run.
	// false (the default): destructive steps are Skipped (recorded, not
	// silently omitted); non-destructive steps still run.
	Yes bool

	// Only restricts Run to the named steps (matched against Step.Name),
	// preserving roster order. Empty means every step in the roster.
	Only []string

	// CheckOnly runs every selected step's Check instead of Apply — `update
	// check`. No step is ever Skipped in this mode: Check is definitionally
	// safe, so there is nothing to gate on Yes.
	CheckOnly bool

	// OnStep, if non-nil, is invoked synchronously immediately after each
	// step's Result is recorded — the CLI's hook for streaming a transcript
	// line per step as `update run`/`update check` executes, rather than
	// only after the whole roster completes.
	OnStep func(Result)
}

// Client runs a roster of maintenance Steps over a Runner.
type Client struct {
	run   exec.Runner
	steps []Step
}

// Option configures a Client at construction.
type Option func(*Client)

// WithSteps overrides the default roster (DefaultSteps()) — tests inject a
// narrower or fully-faked roster.
func WithSteps(steps []Step) Option {
	return func(c *Client) { c.steps = steps }
}

// New builds a Client over the given Runner, with the default roster
// (DefaultSteps()).
func New(run exec.Runner, opts ...Option) *Client {
	c := &Client{run: run, steps: DefaultSteps()}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// StepNames returns the Client's roster names, in order — used by callers
// that need the raw roster (validation, --help rendering).
func (c *Client) StepNames() []string {
	names := make([]string, len(c.steps))
	for i, s := range c.steps {
		names[i] = s.Name
	}
	return names
}

// DestructiveStepNames returns, in roster order, the names of steps in the
// selected subset (only's semantics match Run's — same filter, same "empty
// means everything") whose Destructive is true. A caller that needs to
// decide whether to prompt before a run (and what to name in that prompt)
// uses this instead of re-deriving the filter itself.
func (c *Client) DestructiveStepNames(only []string) []string {
	var names []string
	for _, s := range selectSteps(c.steps, only) {
		if s.Destructive {
			names = append(names, s.Name)
		}
	}
	return names
}

// ValidateOnly errors when a requested step name matches nothing in the
// Client's roster — a typo (or a stale [update] roster entry after a
// roster change) should surface as a clear error, not silently run an
// empty subset. Exposed on Client — not left to the CLI layer alone — so
// any future non-CLI consumer of this package gets the same validation for
// free (mirrors internal/clean's ParseKind: validated in-package, surfaced
// by the caller).
func (c *Client) ValidateOnly(only []string) error {
	return validateOnly(only, c.StepNames())
}

// validateOnly is ValidateOnly's pure body: only ⊆ valid, or a named error
// listing what didn't match plus the full valid set.
func validateOnly(only []string, valid []string) error {
	if len(only) == 0 {
		return nil
	}
	validSet := make(map[string]bool, len(valid))
	for _, name := range valid {
		validSet[name] = true
	}
	var unknown []string
	for _, name := range only {
		if !validSet[name] {
			unknown = append(unknown, name)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	return fmt.Errorf("unknown step name(s) %s — roster is %s", strings.Join(unknown, ", "), strings.Join(valid, ", "))
}

// Run executes the Client's roster (or Options.Only's subset, in roster
// order) against opts. Every step is isolated: a step's error — or panic —
// is recorded on its own Result and never aborts the loop or returns up
// directly. Report.Err joins the failures only afterward, for the caller's
// exit-code decision.
func (c *Client) Run(ctx context.Context, opts Options) Report {
	steps := selectSteps(c.steps, opts.Only)

	report := Report{Results: make([]Result, 0, len(steps))}
	for _, s := range steps {
		res := runStep(ctx, c.run, s, opts)
		report.Results = append(report.Results, res)
		if opts.OnStep != nil {
			opts.OnStep(res)
		}
	}
	return report
}

// runStep executes one step's Check or Apply (per opts) and always returns a
// Result — a panic inside a Step's function is recovered and reported as
// that step's own error, never propagated, so one badly-behaved step can't
// take down the whole roster.
func runStep(ctx context.Context, run exec.Runner, s Step, opts Options) (res Result) {
	res = Result{Name: s.Name, Destructive: s.Destructive}
	defer func() {
		if r := recover(); r != nil {
			res.Err = fmt.Errorf("panic: %v", r)
			slog.Error("Maintenance step panicked.", "step", s.Name, "panic", r)
		}
	}()

	if opts.CheckOnly {
		res.Output, res.Duration, res.Err = runPhase(ctx, run, s.Name, "check", s.Check)
		return res
	}

	if s.Destructive && !opts.Yes {
		res.Skipped = true
		res.SkipReason = "destructive step skipped: pass --yes to run it"
		slog.Info("Skipping destructive maintenance step.", "step", s.Name, "reason", res.SkipReason)
		return res
	}

	res.Output, res.Duration, res.Err = runPhase(ctx, run, s.Name, "apply", s.Apply)
	return res
}

// runPhase runs one step's Check or Apply function (named by phase, for
// logging), timing it and logging the outcome — the shared body behind
// runStep's CheckOnly and Apply branches, which otherwise differ only in
// which function they call and which phase name they log.
func runPhase(ctx context.Context, run exec.Runner, name, phase string, fn func(context.Context, exec.Runner) (string, error)) (output string, duration time.Duration, err error) {
	slog.Debug("Preparing to run maintenance step.", "step", name, "phase", phase)
	start := time.Now()
	output, err = fn(ctx, run)
	duration = time.Since(start)
	logStepOutcome(name, phase, Result{Name: name, Output: output, Err: err, Duration: duration})
	return output, duration, err
}

// logStepOutcome logs a step's Check/Apply result at the appropriate level.
func logStepOutcome(name, phase string, res Result) {
	if res.Err != nil {
		slog.Error("Failed to run maintenance step.", "step", name, "phase", phase, "error", res.Err, "duration", res.Duration.Round(time.Millisecond))
		return
	}
	slog.Info("Successfully ran maintenance step.", "step", name, "phase", phase, "duration", res.Duration.Round(time.Millisecond))
}

// selectSteps filters steps to only's names, preserving roster order. Empty
// only returns every step unchanged. A name in only that matches nothing in
// steps is silently absent from the result — callers that need to reject an
// unknown name outright should call ValidateOnly before Run.
func selectSteps(steps []Step, only []string) []Step {
	if len(only) == 0 {
		return steps
	}
	want := make(map[string]bool, len(only))
	for _, name := range only {
		want[name] = true
	}
	out := make([]Step, 0, len(steps))
	for _, s := range steps {
		if want[s.Name] {
			out = append(out, s)
		}
	}
	return out
}
