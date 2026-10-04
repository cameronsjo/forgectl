package cli

import (
	"context"
	"errors"
	"fmt"
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
	if err := led.Begin(worker.Row{Name: name, Branch: branch, StartedAt: s.now().UTC()}); err != nil {
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
	if err := led.Update(name, func(r *worker.Row) { r.Harness = built.Invocation.Harness }); err != nil {
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
	service := surface.NewService(adapter, surface.Policy{AllowPATHBinary: opts.AllowPATH}, "")

	launched, err := runWorkerSteps(ctx, led, opts.DisplayName, opts.Worktree, workerSteps{
		addWorktree: func(ctx context.Context) (worker.Worktree, error) {
			return worker.AddWorktree(ctx, deps.Runner, top, opts.DisplayName, opts.Worktree)
		},
		build: func(cwd string) (launch.BuiltInvocation, error) {
			req := surfaceInvocationRequest(deps, cwd, injected, unset, opts.Harness)
			req.Worker = true
			return launch.BuildInvocation(req)
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
		now: time.Now,
	})
	if err != nil {
		return err
	}

	out := cmd.OutOrStdout()
	if _, err := fmt.Fprintln(out, termsafe.SafeLine(launched.ref.String())); err != nil {
		return err
	}
	_, err = fmt.Fprintln(out, termsafe.QuotePath(launched.worktree))
	return err
}
