package workflow

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/quarantine"
	"github.com/cameronsjo/forgectl/internal/sandbox"
)

// defaultStripGlobs is the clean-room control's built-in fallback strip-list,
// used when a `strip` step omits `globs` AND no config default is set
// (design doc: "omit globs → configured default set"). Sourced from
// quarantine.DefaultTargets (#20) — the same canonical instruction-file list
// quarantine's reversible Hide uses — so the two controls never drift.
var defaultStripGlobs = quarantine.DefaultTargets

// Executor runs a Plan's steps in order through one constructor-injected
// exec.Runner (mirrors tmux.New / projects.New), threading a shared Context
// so steps compose on each other's exports.
type Executor struct {
	run               exec.Runner
	registry          StepRegistry
	dryRun            bool
	defaultStripGlobs []string
}

// Option configures an Executor at construction.
type Option func(*Executor)

// WithDryRun sets dry-run mode: Run returns without invoking any StepRunner,
// so zero Runner calls are issued.
func WithDryRun(dryRun bool) Option {
	return func(e *Executor) { e.dryRun = dryRun }
}

// WithDefaultStripGlobs overrides the strip-list a `strip` step falls back to
// when its own `globs` field is empty — wired from config.WorkflowConfig by
// the CLI layer. An empty slice is ignored (the built-in default still
// applies).
func WithDefaultStripGlobs(globs []string) Option {
	return func(e *Executor) {
		if len(globs) > 0 {
			e.defaultStripGlobs = globs
		}
	}
}

// NewExecutor builds an Executor over the given Runner, registering the
// built-in step verbs. Mirrors tmux.New(run exec.Runner, opts...). The registry
// is built once, after options apply, so WithDefaultStripGlobs feeds the strip
// runner its configured strip-list without rebuilding the registry.
func NewExecutor(run exec.Runner, opts ...Option) *Executor {
	e := &Executor{run: run, defaultStripGlobs: defaultStripGlobs}
	for _, opt := range opts {
		opt(e)
	}
	e.registry = defaultRegistry(e.defaultStripGlobs)
	return e
}

// defaultRegistry returns the built-in step vocabulary. run/worktree/clone/
// strip/teardown are implemented for the spike; launch/collect are registered
// but return ErrNotYetWired (design doc spike scope). Each verb's runner and
// exports are declared together so they can't drift.
func defaultRegistry(stripGlobs []string) StepRegistry {
	return StepRegistry{
		"run":      {Runner: runStep},
		"worktree": {Runner: newSandboxStep(false), Exports: []string{"workspace"}},
		"clone":    {Runner: newSandboxStep(true), Exports: []string{"workspace"}},
		"strip":    {Runner: newStripStep(stripGlobs)},
		"teardown": {Runner: teardownStep},
		"launch":   {Runner: notYetWiredStep, Exports: []string{"review"}},
		"collect":  {Runner: notYetWiredStep},
	}
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

// notYetWiredStep backs the launch/collect registry entries for the spike.
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

// newStripStep builds the `strip` StepRunner, closing over the default
// strip-list to fall back to when a step omits `globs` (design doc: "omit
// globs → configured default set"). Globs are resolved ONLY inside
// ${workspace} — a path-escape guard rejects any glob containing ".." or an
// absolute path, per ADR-0003's "correctness-and-security requirement, spike
// or not".
func newStripStep(defaultGlobs []string) StepRunner {
	return func(_ context.Context, _ exec.Runner, wctx *Context, step PlanStep) error {
		workspace, ok := wctx.Get("workspace")
		if !ok || workspace == "" {
			slog.Warn("Strip step missing workspace context (requires worktree/clone step to run first).")
			return errors.New("strip step requires ${workspace} (run after a worktree/clone step)")
		}

		globs := step.Globs
		if len(globs) == 0 {
			globs = defaultGlobs
		}

		slog.Debug("Preparing to strip paths from workspace.", "workspace", workspace, "globCount", len(globs), "globs", globs)
		for _, g := range globs {
			if err := validateStripGlob(g); err != nil {
				slog.Warn("Invalid strip glob.", "glob", g, "error", err)
				return err
			}
			// Expand the pattern so a real glob (e.g. *.md) removes every match,
			// not only a file literally named "*.md". The strip-list is a
			// security control, so under-stripping would weaken the clean-room
			// defense. A literal entry that doesn't exist yields no matches — a
			// no-op, matching the pre-glob behavior.
			matches, err := filepath.Glob(filepath.Join(workspace, filepath.Clean(g)))
			if err != nil {
				slog.Warn("Bad strip pattern.", "glob", g, "error", err)
				return fmt.Errorf("strip pattern %q: %w", g, err)
			}
			for _, target := range matches {
				// A pattern with no ".." can still reach outside via a symlink
				// (e.g. a matched dir that links to /etc); re-check every match's
				// real path before deleting through it.
				if !sandbox.WithinWorkspace(workspace, target) {
					slog.Error("Strip match escapes workspace; refusing.", "glob", g, "target", target)
					return fmt.Errorf("strip match %q escapes workspace", target)
				}
				slog.Debug("Removing path.", "glob", g, "target", target)
				if err := os.RemoveAll(target); err != nil {
					slog.Error("Failed to remove path.", "glob", g, "target", target, "error", err)
					return fmt.Errorf("strip %s: %w", g, err)
				}
			}
		}
		slog.Debug("Successfully stripped paths from workspace.", "workspace", workspace, "globCount", len(globs))
		return nil
	}
}

// validateStripGlob rejects a glob that could escape ${workspace}: an
// absolute path, or any ".." path-traversal segment.
func validateStripGlob(g string) error {
	if g == "" {
		return errors.New("strip glob must not be empty")
	}
	if filepath.IsAbs(g) {
		return fmt.Errorf("strip glob %q must not be absolute", g)
	}
	// Normalize Windows separators so a "..\" segment is caught on any OS, then
	// reject any ".." path segment wherever it appears.
	normalized := strings.ReplaceAll(filepath.Clean(g), "\\", "/")
	for _, seg := range strings.Split(normalized, "/") {
		if seg == ".." {
			return fmt.Errorf("strip glob %q must not traverse outside the workspace", g)
		}
	}
	return nil
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
