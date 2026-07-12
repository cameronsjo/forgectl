package workflow

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/sandbox"
)

// Executor runs a Plan's steps in order through one constructor-injected
// exec.Runner (mirrors tmux.New / projects.New), threading a shared Context
// so steps compose on each other's exports.
type Executor struct {
	run      exec.Runner
	registry StepRegistry
	dryRun   bool
}

// Option configures an Executor at construction.
type Option func(*Executor)

// WithDryRun sets dry-run mode: Run returns without invoking any StepRunner,
// so zero Runner calls are issued.
func WithDryRun(dryRun bool) Option {
	return func(e *Executor) { e.dryRun = dryRun }
}

// NewExecutor builds an Executor over the given Runner and step registry.
// The registry is the MERGED vocabulary from NewRegistry — the same one
// BuildPlan reads exports from, so plan-time deferral and run-time dispatch
// can never drift (ADR-0005).
func NewExecutor(run exec.Runner, registry StepRegistry, opts ...Option) *Executor {
	e := &Executor{run: run, registry: registry}
	for _, opt := range opts {
		opt(e)
	}
	return e
}

// builtinRegistry returns the engine-owned step vocabulary: the generic
// escape hatch (run), the sandbox-backed verbs (worktree/clone/teardown),
// and the collect stub (spike scope). strip belongs to the quarantine module
// and launch to the launch module — contributed through NewRegistry, not
// listed here (ADR-0005's verb redistribution).
//
// GuardedFields (step.Def) is declared here, at the one place each verb is
// defined, so the bless-time injection guard's model of danger can never drift
// from the runner that actually executes the verb. `run` is the arbitrary-exec
// escape hatch: its cmd and args choose what runs, so both are guarded.
// worktree/clone take repo/ref, which merely NAME data — parameterizing them
// (`--param repo=owner/x`) is the feature, and the sandbox contains what they
// fetch. teardown reads only ${workspace} from the Context. collect's from/to
// are likewise data paths under the same rule (it is an unwired stub; when it
// is implemented, re-judge whether `to` is a write sink worth guarding).
func builtinRegistry() StepRegistry {
	return StepRegistry{
		"run":      {Runner: runStep, GuardedFields: []string{"Cmd", "Args"}},
		"worktree": {Runner: newSandboxStep(false), Exports: []string{"workspace"}},
		"clone":    {Runner: newSandboxStep(true), Exports: []string{"workspace"}},
		"teardown": {Runner: teardownStep},
		"collect":  {Runner: notYetWiredStep},
	}
}

// NewRegistry merges the engine built-ins with module step contributions
// into the one registry BuildPlan AND NewExecutor consume. Any collision —
// a module shadowing a builtin, or two modules claiming the same verb — is
// an error, never a silent last-wins (each contribution is passed
// separately so cross-module collisions are visible here).
func NewRegistry(contributions ...StepRegistry) (StepRegistry, error) {
	merged := builtinRegistry()
	for _, contributed := range contributions {
		for name, def := range contributed {
			if _, exists := merged[name]; exists {
				return nil, fmt.Errorf("step verb %q registered twice (module collides with a builtin or another module)", name)
			}
			merged[name] = def
		}
	}
	return merged, nil
}

// Run executes plan's steps in order. In dry-run mode it returns immediately
// after receiving plan — no StepRunner is invoked, so the caller (or a test
// asserting on a FakeRunner) sees zero Runner calls.
func (e *Executor) Run(ctx context.Context, plan Plan, wctx *Context) error {
	slog.Debug("Preparing to execute workflow plan.", "workflowName", plan.Name, "stepCount", len(plan.Steps))
	if e.dryRun {
		slog.Debug("Dry-run mode: skipping execution")
		return nil
	}
	for i, step := range plan.Steps {
		def, ok := e.registry[step.Uses]
		if !ok {
			slog.Error("Unknown step verb.", "stepIndex", i, "stepUse", step.Uses)
			return fmt.Errorf("step %d: unknown step verb %q", i, step.Uses)
		}
		// Re-interpolate the step's fields against the live Context: exports
		// earlier steps produced (${workspace}, ${review}) resolve here, where
		// nothing is deferred — so a forward reference (consuming an export
		// before its step has run) is a hard error, never a literal "${...}"
		// handed to a command.
		resolved, err := interpolatePlanStep(wctx, step)
		if err != nil {
			slog.Error("Failed to resolve step exports.", "stepIndex", i, "stepUse", step.Uses, "error", err)
			cleanupSandbox(wctx)
			return fmt.Errorf("step %d (%s): %w", i, step.Uses, err)
		}
		slog.Debug("Executing step.", "stepIndex", i, "stepUse", step.Uses)
		if err := def.Runner(ctx, e.run, wctx, resolved); err != nil {
			slog.Error("Step execution failed.", "stepIndex", i, "stepUse", step.Uses, "error", err)
			// A mid-workflow failure skips the explicit teardown step, so best-
			// effort remove the sandbox here to avoid leaking a temp checkout.
			cleanupSandbox(wctx)
			return fmt.Errorf("step %d (%s): %w", i, step.Uses, err)
		}
		slog.Debug("Step completed.", "stepIndex", i, "stepUse", step.Uses)
	}
	slog.Info("Successfully executed workflow plan.", "workflowName", plan.Name, "stepCount", len(plan.Steps))
	return nil
}

// cleanupSandbox best-effort removes the ${workspace} temp dir a worktree/clone
// step created, called when a step fails before the workflow's own teardown
// step runs. ${workspace} is always a forgectl os.MkdirTemp dir, so removing it
// is safe. A stale git-worktree registration in the source repo is left for a
// future `git worktree prune` (the full teardown lands with the clean-room path).
func cleanupSandbox(wctx *Context) {
	ws, ok := wctx.Get("workspace")
	if !ok || ws == "" {
		return
	}
	if err := sandbox.Teardown(context.Background(), nil, ws); err != nil {
		slog.Warn("Failed to clean up sandbox after error.", "workspace", ws, "error", err)
		return
	}
	slog.Debug("Cleaned up sandbox after error.", "workspace", ws)
}

// notYetWiredStep backs the collect registry entry for the spike (launch's
// stub lives with its module: internal/launch/steps.go).
func notYetWiredStep(context.Context, exec.Runner, *Context, PlanStep) error {
	return ErrNotYetWired
}

// runStep is the arbitrary-command escape hatch: it shells out to step.Cmd
// with step.Args via the injected Runner.
func runStep(ctx context.Context, run exec.Runner, _ *Context, step PlanStep) error {
	if step.Cmd == "" {
		slog.Warn("Run step missing required cmd field.")
		return errors.New("run step requires cmd")
	}
	slog.Debug("Running command.", "cmd", step.Cmd, "args", step.Args)
	_, err := run.Run(ctx, step.Cmd, step.Args...)
	return err
}

// newSandboxStep builds the worktree/clone StepRunner: sandbox a repo into a
// fresh os.MkdirTemp dir and export ${workspace} (ADR-0003). The two verbs
// share the runner but differ deliberately: `worktree` (alwaysClone=false)
// uses a cheap `git worktree add` for a local repo — shared object store, a
// .git file pointing back at the source checkout — and falls back to `git
// clone` for a remote; an explicit `clone` (alwaysClone=true) ALWAYS clones,
// even for a local path, so an author's full-isolation request is honored
// rather than silently downgraded to a linked worktree.
func newSandboxStep(alwaysClone bool) StepRunner {
	return func(ctx context.Context, run exec.Runner, wctx *Context, step PlanStep) error {
		dir, err := sandbox.Sandbox(ctx, run, step.Repo, step.Ref, alwaysClone)
		if err != nil {
			return err
		}
		wctx.Set("workspace", dir)
		return nil
	}
}

// teardownStep removes the sandbox dir. It is idempotent: a missing
// ${workspace} value or an already-removed dir is not an error, so teardown
// is safe to run after a partial failure (ADR-0003).
func teardownStep(ctx context.Context, run exec.Runner, wctx *Context, _ PlanStep) error {
	workspace, ok := wctx.Get("workspace")
	if !ok || workspace == "" {
		slog.Debug("Teardown: no workspace context, nothing to remove.")
		return nil
	}
	return sandbox.Teardown(ctx, run, workspace)
}
