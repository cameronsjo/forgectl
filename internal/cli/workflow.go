package cli

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/bless"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/step"
	"github.com/cameronsjo/forgectl/internal/workflow"
)

// verifierFactory builds the Verifier the run and verify paths consult. It is a
// package-level var purely as a TEST SEAM — tests swap it for a spy or a fake;
// it is never user-configurable (the trust it roots on is the compiled-in
// anchor). Production always returns the real user-presence verifier.
var verifierFactory = func() workflow.Verifier { return bless.NewVerifier() }

// workflowAliases maps each canonical workflow subcommand to its accepted
// aliases — migrated here from forgive.WorkflowAliases at conversion. The
// `flow` group shorthand is a GroupAlias on the manifest. Separate var for
// the same initialization-cycle reason as yAliases.
var workflowAliases = map[string][]string{
	"run": {"r"},
}

// workflowModule declares the workflow engine core module (ADR-0005): owns
// the [workflow] config section. It contributes no Steps itself — the engine
// owns the builtins directly; modules contribute through NewRegistry. A
// constructor rather than a var: its run command aggregates every module's
// Steps via allModules(), and a package-level var would close that loop into
// an initialization cycle (var → constructor → stepContributions →
// allModules → var); a function-only cycle is legal.
func workflowModule() module.Manifest {
	return module.Manifest{
		Name:         "workflow",
		Tier:         module.TierCore,
		ConfigKey:    "workflow",
		GroupAliases: []string{"flow"},
		SubAliases:   workflowAliases,
		New:          newWorkflowCmd,
	}
}

// stepContributions aggregates every module's data-plane step contribution
// for workflow.NewRegistry. Each module's registry is passed separately so
// NewRegistry can name cross-module collisions, never last-wins them.
func stepContributions(deps module.Deps) []step.Registry {
	var out []step.Registry
	for _, m := range allModules() {
		if m.Steps != nil {
			out = append(out, m.Steps(deps))
		}
	}
	return out
}

// newWorkflowCmd builds the `workflow` parent command (alias `flow`). Verbs
// are attached as subcommands: `run` executes a workflow file, `list` shows
// the resolvable names. Mirrors newLaunchCmd's parent/subcommand shape.
func newWorkflowCmd(deps module.Deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "workflow",
		Aliases: []string{"flow"},
		Short:   "Run declarative workflows composing forgectl's other verbs",
		Long: `workflow parses a TOML step list and executes it against the local
toolset (git, claude, tmux) — orchestration as data, not one-off scripts.

  forgectl workflow run <name>              run a workflow by name
  forgectl workflow run <name> --dry-run    print the resolved plan, run nothing
  forgectl workflow run <name> --resume     resume the last run from its first
                                            incomplete step
  forgectl workflow status <name>           show the last run's checkpoint state
  forgectl workflow list                    show resolvable workflow names

Workflow files live in <config-dir>/workflows/<name>.workflow.toml — the same
base as config.toml (macOS: ~/Library/Application Support/forgectl, Linux:
~/.config/forgectl) — or fall back to a shipped built-in of the same name.`,
	}
	cmd.AddCommand(
		newWorkflowRunCmd(deps),
		newWorkflowStatusCmd(),
		newWorkflowListCmd(),
		newWorkflowBlessCmd(deps),
		newWorkflowVerifyCmd(),
		newWorkflowTrustCmd(deps),
	)
	applyAliases(cmd, workflowAliases)
	return cmd
}

// newWorkflowRunCmd builds `forgectl workflow run <name> [--dry-run]
// [--param k=v]...`.
func newWorkflowRunCmd(deps module.Deps) *cobra.Command {
	var dryRun bool
	var resume bool
	var rawParams []string

	cmd := &cobra.Command{
		Use:   "run <name>",
		Short: "Run a workflow by name",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]

			if dryRun && resume {
				return fmt.Errorf("--dry-run and --resume are mutually exclusive: dry-run plans without running, resume continues a real run")
			}

			params, err := parseParams(rawParams)
			if err != nil {
				return err
			}

			// Read the workflow bytes exactly ONCE, verify that buffer, and
			// parse the SAME buffer — no re-read between check and use closes
			// TOCTOU by construction (ADR-0006). Built-ins are compiled into
			// the trust surface and dry-run executes nothing, so both skip the
			// blessing gate deliberately.
			src, err := workflow.Load(name)
			if err != nil {
				return err
			}
			if !src.Builtin && !dryRun {
				if err := verifierFactory().Verify(src.Path, src.Data); err != nil {
					return fmt.Errorf("workflow %q: %w", name, err)
				}
			}
			wf, err := workflow.Parse(src.Data)
			if err != nil {
				return err
			}

			// One merged registry serves BOTH BuildPlan and the Executor —
			// the plan-time deferral of a module verb's exports (launch's
			// ${review}) and run-time dispatch can't drift (ADR-0005).
			registry, err := workflow.NewRegistry(stepContributions(deps)...)
			if err != nil {
				return err
			}

			plan, err := workflow.BuildPlan(wf, params, registry)
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			if dryRun {
				printPlan(out, plan)
				return nil
			}

			// Serialize concurrent runs of the SAME workflow: the run-state
			// sidecar is a single file rewritten in full after each step, so two
			// overlapping runs would clobber each other's checkpoints (run B's
			// step-0 write landing after run A's step-2 regresses the file, so a
			// later --resume silently re-executes steps A already ran). An advisory
			// lock held for the whole run makes a second concurrent invocation fail
			// fast rather than interleave. dry-run took its early return above, so
			// it never contends. Acquired before any state read/write.
			lock, err := workflow.AcquireRunLock(name)
			if err != nil {
				return err
			}
			defer lock.Release()

			wctx := workflow.NewContext(nil)
			for k, v := range params {
				wctx.Set(k, v)
			}

			opts, err := resumeOptions(out, name, src.Data, plan, registry, resume)
			if err != nil {
				return err
			}
			if opts == nil {
				// A no-op resume of an already-complete run: nothing to execute.
				return nil
			}

			exe := workflow.NewExecutor(deps.Runner, registry, opts...)
			if err := exe.Run(cmd.Context(), plan, wctx); err != nil {
				return err
			}

			// A fully successful run needs no checkpoint — clear it so the next
			// run starts clean and `status` reflects reality.
			if err := workflow.ClearState(name); err != nil {
				return err
			}
			fmt.Fprintf(out, "workflow %q completed\n", plan.Name)
			return nil
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print the resolved plan without running any step")
	cmd.Flags().BoolVar(&resume, "resume", false, "resume the last run from its first incomplete step")
	cmd.Flags().StringArrayVar(&rawParams, "param", nil, "workflow param as key=value (repeatable)")
	return cmd
}

// resumeOptions decides how a run is executed and returns the Executor options
// that carry that decision. For a fresh run it returns a WithRecorder that
// checkpoints every step. For a resume it validates the saved state (present,
// same definition, no checkpointed-export dependency) and returns
// WithResumeFrom + a resume Recorder. A nil slice with a nil error is the
// deliberate "already complete — nothing to do" signal the caller returns on.
func resumeOptions(out io.Writer, name string, data []byte, plan workflow.Plan, registry step.Registry, resume bool) ([]workflow.Option, error) {
	now := time.Now()
	defHash := workflow.DefinitionHash(data)

	if !resume {
		recorder := workflow.NewStateRecorder(name, workflow.NewRunID(now), defHash, now)
		return []workflow.Option{workflow.WithRecorder(recorder)}, nil
	}

	prior, ok, err := workflow.LoadState(name)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("workflow %q has no saved run state to resume — run it without --resume first", name)
	}
	if prior.DefinitionHash != defHash {
		return nil, fmt.Errorf("workflow %q changed since its checkpointed run — run it fresh (without --resume); a resume never replays across an edited definition", name)
	}

	resumeFrom := workflow.ResumeFrom(prior, plan)
	if resumeFrom >= len(plan.Steps) {
		if err := workflow.ClearState(name); err != nil {
			return nil, err
		}
		fmt.Fprintf(out, "workflow %q is already complete — nothing to resume\n", name)
		return nil, nil
	}

	// Refuse rather than reconstruct a checkpointed step's ephemeral export from
	// the unsigned sidecar — that would let an attacker-writable state file feed
	// a blessing-guarded field. The user runs fresh instead (which is also the
	// only correct thing when the export was a torn-down sandbox).
	if exp, idx, missing := workflow.MissingResumeExport(plan, resumeFrom, registry); missing {
		return nil, fmt.Errorf("workflow %q cannot be resumed: step %d needs ${%s} from an earlier checkpointed step, and a step's outputs cannot be reconstructed on resume — run it fresh (without --resume)", name, idx+1, exp)
	}

	recorder := workflow.NewResumeRecorder(prior, resumeFrom, now)
	fmt.Fprintf(out, "resuming workflow %q from step %d of %d\n", name, resumeFrom+1, len(plan.Steps))
	return []workflow.Option{workflow.WithResumeFrom(resumeFrom), workflow.WithRecorder(recorder)}, nil
}

// newWorkflowListCmd builds `forgectl workflow list` — a stub that shows the
// embedded built-in workflow names. Listing user-directory workflows is a
// follow-on (the spike ships one built-in, clean-room-review).
func newWorkflowListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List resolvable workflow names",
		RunE: func(cmd *cobra.Command, _ []string) error {
			names, err := workflow.ListBuiltins()
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(names) == 0 {
				fmt.Fprintln(out, "no built-in workflows")
				return nil
			}
			sort.Strings(names)
			for _, n := range names {
				fmt.Fprintln(out, n)
			}
			return nil
		},
	}
}

// newWorkflowStatusCmd builds `forgectl workflow status <name>`: a read-only
// view of the last run's per-step checkpoint state (what `--resume` would skip).
// It touches no trust chain and prompts for nothing — it only reads the local
// run-state sidecar.
func newWorkflowStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status <name>",
		Short: "Show the last run's per-step checkpoint state",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			out := cmd.OutOrStdout()

			state, ok, err := workflow.LoadState(name)
			if err != nil {
				return err
			}
			if !ok {
				fmt.Fprintf(out, "%s: no saved run state (never run, or last run completed cleanly)\n", name)
				return nil
			}

			fmt.Fprintf(out, "%s — run %s\n", state.Workflow, state.RunID)
			fmt.Fprintf(out, "  started: %s\n", state.StartedAt)
			fmt.Fprintf(out, "  updated: %s\n", state.UpdatedAt)
			if len(state.Steps) == 0 {
				fmt.Fprintln(out, "  no steps checkpointed complete")
			} else {
				fmt.Fprintf(out, "  %d step(s) complete:\n", len(state.Steps))
				for _, s := range state.Steps {
					fmt.Fprintf(out, "    %d. %-10s done %s\n", s.Index+1, s.Uses, s.CompletedAt)
				}
			}

			// If the definition changed since this run, --resume will refuse it
			// (a resume never replays across an edited file) — say so up front.
			// The hash covers the WHOLE file, so any byte change invalidates every
			// checkpoint, not just the edited step.
			if src, err := workflow.Load(name); err == nil {
				if workflow.DefinitionHash(src.Data) != state.DefinitionHash {
					fmt.Fprintf(out, "  note: %s has changed since this run (any edit to the file invalidates every checkpoint) — resume will be refused; run it fresh\n", name)
				}
			}
			return nil
		},
	}
}

// parseParams turns repeatable --param key=value flags into a map. A
// malformed entry (no "=") is a usage error, not a silent skip.
func parseParams(raw []string) (map[string]string, error) {
	out := make(map[string]string, len(raw))
	for _, p := range raw {
		k, v, ok := strings.Cut(p, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("invalid --param %q: want key=value", p)
		}
		out[k] = v
	}
	return out, nil
}

// printPlan renders a resolved Plan for --dry-run: the step sequence a user
// reviews before trusting a workflow, with zero side effects.
func printPlan(out io.Writer, plan workflow.Plan) {
	fmt.Fprintf(out, "workflow %s@%s — %d step(s):\n", plan.Name, plan.Version, len(plan.Steps))
	for i, s := range plan.Steps {
		fmt.Fprintf(out, "  %d. %s\n", i+1, s.Uses)
		printField(out, "repo", s.Repo)
		printField(out, "ref", s.Ref)
		if len(s.Globs) > 0 {
			fmt.Fprintf(out, "     globs: %s\n", strings.Join(s.Globs, ", "))
		}
		printField(out, "skill", s.Skill)
		printField(out, "posture", s.Posture)
		printField(out, "mode", s.Mode)
		printField(out, "from", s.From)
		printField(out, "to", s.To)
		printField(out, "cmd", s.Cmd)
		if len(s.Args) > 0 {
			fmt.Fprintf(out, "     args: %s\n", strings.Join(s.Args, " "))
		}
	}
}

// printField writes one non-empty plan-step field as an indented line.
func printField(out io.Writer, name, value string) {
	if value == "" {
		return
	}
	fmt.Fprintf(out, "     %s: %s\n", name, value)
}
