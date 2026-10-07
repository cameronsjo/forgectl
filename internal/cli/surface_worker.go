package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/launch"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/projects"
	"github.com/cameronsjo/forgectl/internal/surface"
	"github.com/cameronsjo/forgectl/internal/surface/backend"
	"github.com/cameronsjo/forgectl/internal/surface/worker"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// A worker launch (`surface launch --worktree <branch>`) starts one agent for
// a coordinator: its own git worktree, its own herdr workspace, and a ledger
// row that says what was created at each step.

// maxLedgerFailureLen bounds the error text kept in a failed row.
const maxLedgerFailureLen = 240

// workerLedger is the slice of *worker.Ledger a launch writes.
type workerLedger interface {
	Begin(worker.Row) error
	Update(name string, fn func(*worker.Row)) error
}

// workerSteps are the three things a worker launch does, as functions so the
// ledger bookkeeping between them can be tested without git or herdr.
type workerSteps struct {
	addWorktree func(ctx context.Context) (worker.Worktree, error)
	build       func(cwd string) (launch.BuiltInvocation, error)
	launch      func(ctx context.Context, inv launch.Invocation) (backend.Ref, error)
	now         func() time.Time
	// brief is the first brief's ledger record, or nil for a launch with
	// none. It is written with the pending row, before anything starts.
	brief *worker.Brief
}

// workerLaunched is what a successful worker launch reports.
type workerLaunched struct {
	ref      backend.Ref
	worktree string
}

// runWorkerSteps writes a pending row before anything is created, then moves
// the row forward after each step. A failure at any step leaves the row in
// StageFailed with whatever the earlier steps recorded, so the next
// coordinator can find a worktree or workspace a dead launch left behind.
func runWorkerSteps(ctx context.Context, led workerLedger, name, branch string, s workerSteps) (workerLaunched, error) {
	if err := led.Begin(worker.Row{Name: name, Branch: branch, StartedAt: s.now().UTC(), Brief: s.brief}); err != nil {
		return workerLaunched{}, err
	}
	fail := func(err error) (workerLaunched, error) {
		var recovery string
		var launchErr *surface.LaunchError
		if errors.As(err, &launchErr) {
			recovery = launchErr.Recovery
		}
		if uerr := led.Update(name, func(r *worker.Row) {
			r.Stage = worker.StageFailed
			r.Failure = termsafe.SafeLineMax(err.Error(), maxLedgerFailureLen)
			r.Recovery = recovery
		}); uerr != nil {
			return workerLaunched{}, errors.Join(err, fmt.Errorf("record the failure in the worker ledger: %w", uerr))
		}
		return workerLaunched{}, err
	}

	wt, err := s.addWorktree(ctx)
	if err != nil {
		return fail(err)
	}
	if err := led.Update(name, func(r *worker.Row) {
		r.Stage = worker.StageWorktree
		r.Worktree = wt.Path
		r.Base = wt.Base
	}); err != nil {
		return fail(err)
	}

	built, err := s.build(wt.Path)
	if err != nil {
		return fail(err)
	}
	// A worker must be built as one: that is what applies the posture floor,
	// the isolation argv and the environment allowlist. A build step that
	// skipped it would launch a worker with the launcher's posture and env.
	if !built.Worker {
		return fail(errors.New("forgectl: the worker invocation was not built as a worker launch"))
	}
	if err := led.Update(name, func(r *worker.Row) {
		r.Harness = built.Invocation.Harness
		if built.SessionID != "" {
			r.SessionID = built.SessionID
			r.Transcript = worker.TranscriptPath(built.Invocation.Env, built.Invocation.CWD, built.SessionID)
		}
	}); err != nil {
		return fail(err)
	}

	ref, err := s.launch(ctx, built.Invocation)
	if err != nil {
		return fail(err)
	}
	// From here the worker is running, so a failure is bookkeeping only and
	// the live ref goes back with it; recording StageFailed would orphan the
	// workspace.
	encoded, err := ref.MarshalJSON()
	if err != nil {
		return workerLaunched{ref: ref, worktree: wt.Path},
			fmt.Errorf("the worker started but its surface reference could not be recorded: %w", err)
	}
	if err := led.Update(name, func(r *worker.Row) {
		r.Stage = worker.StageLaunched
		r.Ref = encoded
	}); err != nil {
		// The worker is running; only the bookkeeping failed. Say so rather
		// than report a launch failure the operator would retry.
		return workerLaunched{ref: ref, worktree: wt.Path},
			fmt.Errorf("the worker started but its ledger row could not be completed: %w", err)
	}
	return workerLaunched{ref: ref, worktree: wt.Path}, nil
}

// sessionNamer is implemented by the herdr adapter.
type sessionNamer interface{ Session() string }

// runWorkerLaunch wires the real steps: herdr only, a validated name, the repo
// top as the ledger key, and the worker posture floor on the invocation.
func runWorkerLaunch(cmd *cobra.Command, deps module.Deps, opts surfaceLaunchOptions) error {
	if opts.Backend != "herdr" {
		return WithExitCode(errors.New("--worktree needs --surface herdr; workers run in herdr only"), 2)
	}
	if err := worker.ValidName(opts.DisplayName); err != nil {
		return WithExitCode(fmt.Errorf("--name: %w", err), 2)
	}

	adapter, err := newHerdrAdapter(cmd.ErrOrStderr())
	if err != nil {
		return WithExitCode(err, 2)
	}
	namer, ok := adapter.(sessionNamer)
	if !ok {
		return errors.New("forgectl: the herdr adapter does not report its session")
	}

	ctx := cmd.Context()
	target, err := projects.New(deps.Runner).ResolveTarget(opts.Target)
	if err != nil {
		return WithExitCode(err, 2)
	}
	top, err := worker.RepoTop(ctx, deps.Runner, target)
	if err != nil {
		return WithExitCode(err, 2)
	}
	led, err := worker.Open(top, namer.Session())
	if err != nil {
		return err
	}

	self, err := surface.SelfPath()
	if err != nil {
		return err
	}
	injected, unset, err := injectedLaunchEnv(deps.Cfg)
	if err != nil {
		return WithExitCode(termsafe.Error(err), 2)
	}
	prompt, brief, err := launchBrief(opts.Brief, time.Now)
	if err != nil {
		return WithExitCode(err, 2)
	}
	if opts.DryRun {
		return planWorkerLaunch(cmd, deps, opts, top, led, workerPlanInputs{injected: injected, unset: unset, prompt: prompt, hasBrief: brief != nil, self: self})
	}
	service := surface.NewService(adapter, surface.Policy{AllowPATHBinary: opts.AllowPATH}, "")

	launched, err := runWorkerSteps(ctx, led, opts.DisplayName, opts.Worktree, workerSteps{
		addWorktree: func(ctx context.Context) (worker.Worktree, error) {
			return worker.AddWorktree(ctx, deps.Runner, top, opts.DisplayName, opts.Worktree, func() (string, error) {
				return workerBase(ctx, deps.Runner, top, cmd.ErrOrStderr())
			})
		},
		build: func(cwd string) (launch.BuiltInvocation, error) {
			return buildWorkerInvocation(surfaceInvocationRequest(deps.Cfg.Launch, cwd, injected, unset, opts.Harness), prompt, worker.NewSessionID, cmd.ErrOrStderr())
		},
		launch: func(ctx context.Context, inv launch.Invocation) (backend.Ref, error) {
			inv.Env = markHerdrPane(inv.Env)
			req := surface.NewLaunchRequest(opts.DisplayName, inv)
			req.Self = self
			result, err := service.Launch(ctx, req)
			if err != nil {
				return backend.Ref{}, err
			}
			return result.Ref(), nil
		},
		now:   time.Now,
		brief: brief,
	})
	if err != nil {
		return err
	}

	out := cmd.OutOrStdout()
	if _, err := fmt.Fprintln(out, safeTitle(launched.ref.String())); err != nil {
		return err
	}
	_, err = fmt.Fprintln(out, safePath(launched.worktree))
	return err
}

// buildWorkerInvocation marks req as a worker launch, which is what turns on
// the worker floor, the claude isolation argv and the environment allowlist
// (launch.BuildInvocation), and gives a claude worker its session id. The
// harness is known only once the profile resolves, so a claude worker is
// built a second time with the id.
func buildWorkerInvocation(req launch.InvocationRequest, prompt string, newID func() (string, error), warn io.Writer) (launch.BuiltInvocation, error) {
	req.Worker = true
	req.Prompt = prompt
	built, err := launch.BuildInvocation(req)
	if err == nil && built.Invocation.Harness == "claude" {
		if req.SessionID, err = newID(); err != nil {
			return launch.BuiltInvocation{}, err
		}
		built, err = launch.BuildInvocation(req)
	}
	if err != nil {
		return built, err
	}
	for _, n := range built.Notes {
		if _, werr := fmt.Fprintln(warn, "forgectl: note: "+termsafe.SafeLineMax(n, 300)); werr != nil {
			return launch.BuiltInvocation{}, werr
		}
	}
	return built, nil
}

// launchBrief turns --brief into the prompt argument and its ledger record:
// the checked text with the report instruction under a fresh marker. An empty
// flag is no brief.
func launchBrief(arg string, now func() time.Time) (string, *worker.Brief, error) {
	if arg == "" {
		return "", nil, nil
	}
	text, err := readBriefArg(arg)
	if err != nil {
		return "", nil, err
	}
	if err := worker.CheckBrief(text, worker.ViaLaunch); err != nil {
		return "", nil, err
	}
	marker, err := worker.NewMarker()
	if err != nil {
		return "", nil, err
	}
	b := &worker.Brief{Marker: marker, Count: 1, SentAt: now().UTC(), Via: worker.ViaLaunch}
	return worker.Compose(text, marker, worker.ViaLaunch), b, nil
}

// workerPlanInputs are the pieces runWorkerLaunch has already built when it
// reaches a --dry-run.
type workerPlanInputs struct {
	injected map[string]string
	unset    []string
	prompt   string
	hasBrief bool
	// self is this forgectl executable, which the harness binary must not be.
	self string
}

// planWorkerLaunch is runWorkerLaunch's --dry-run: the checks the launch runs
// before its first write, then a preview. It does not open the ledger for
// writing, create the worktree root, run git worktree add, or start a surface.
// A name already in the ledger, a branch or path the launch would refuse, and
// a harness profile that does not build as a worker all fail here as they
// fail there.
func planWorkerLaunch(cmd *cobra.Command, deps module.Deps, opts surfaceLaunchOptions, top string, led *worker.Ledger, in workerPlanInputs) error {
	taken, err := led.NameTaken(opts.DisplayName)
	if err != nil {
		return WithExitCode(err, 2)
	}
	if taken {
		// The launch refuses it in Begin, with this error and no exit code.
		return fmt.Errorf("%w (--name %q); close it first or pick another name", worker.ErrNameTaken, opts.DisplayName)
	}
	plan, err := worker.PlanWorktree(cmd.Context(), deps.Runner, top, opts.DisplayName, opts.Worktree)
	if err != nil {
		return err
	}
	built, err := buildWorkerInvocation(surfaceInvocationRequest(deps.Cfg.Launch, plan.Path, in.injected, in.unset, opts.Harness), in.prompt, worker.NewSessionID, cmd.ErrOrStderr())
	if err != nil {
		return err
	}
	if !built.Worker {
		return errors.New("forgectl: the worker invocation was not built as a worker launch")
	}
	// The service refuses an unaccepted binary when it launches; ask the same
	// policy here so the preview refuses too.
	if err := (surface.Policy{AllowPATHBinary: opts.AllowPATH}).AcceptBinary(built.Invocation.Binary, in.self); err != nil {
		return err
	}
	return renderLaunchPlan(cmd.OutOrStdout(), launchPlan{
		DryRun:  true,
		Surface: opts.Backend,
		Name:    opts.DisplayName,
		Target:  top,
		Harness: built.Invocation.Harness,
		Worker: &workerLaunchPlan{
			Repo:       top,
			Worktree:   plan.Path,
			Branch:     plan.Branch,
			BranchFrom: plan.BranchFrom,
			LedgerRow:  ledgerRowWouldCreate,
			Brief:      in.hasBrief,
		},
	}, opts.JSON)
}
