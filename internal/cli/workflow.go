package cli

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/forgive"
	"github.com/cameronsjo/forgectl/internal/workflow"
)

// newWorkflowCmd builds the `workflow` parent command (alias `flow`). Verbs
// are attached as subcommands: `run` executes a workflow file, `list` shows
// the resolvable names. Mirrors newLaunchCmd's parent/subcommand shape.
func newWorkflowCmd(cfg config.Config) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "workflow",
		Aliases: []string{"flow"},
		Short:   "Run declarative workflows composing forgectl's other verbs",
		Long: `workflow parses a TOML step list and executes it against the local
toolset (git, claude, tmux) — orchestration as data, not one-off scripts.

  forgectl workflow run <name>              run a workflow by name
  forgectl workflow run <name> --dry-run    print the resolved plan, run nothing
  forgectl workflow list                    show resolvable workflow names

Workflow files live in <config-dir>/workflows/<name>.workflow.toml — the same
base as config.toml (macOS: ~/Library/Application Support/forgectl, Linux:
~/.config/forgectl) — or fall back to a shipped built-in of the same name.`,
	}
	cmd.AddCommand(
		newWorkflowRunCmd(cfg),
		newWorkflowListCmd(),
	)
	applyWorkflowAliases(cmd)
	return cmd
}

// applyWorkflowAliases sets each workflow subcommand's Cobra aliases from the
// forgive registry — mirrors applyLaunchAliases.
func applyWorkflowAliases(parent *cobra.Command) {
	for _, sub := range parent.Commands() {
		var valid []string
		for _, alias := range forgive.WorkflowAliases[sub.Name()] {
			if alias == sub.Name() {
				continue
			}
			valid = append(valid, alias)
		}
		if len(valid) > 0 {
			sub.Aliases = valid
		}
	}
}

// newWorkflowRunCmd builds `forgectl workflow run <name> [--dry-run]
// [--param k=v]...`.
func newWorkflowRunCmd(cfg config.Config) *cobra.Command {
	var dryRun bool
	var rawParams []string

	cmd := &cobra.Command{
		Use:   "run <name>",
		Short: "Run a workflow by name",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]

			params, err := parseParams(rawParams)
			if err != nil {
				return err
			}

			wf, err := workflow.Resolve(name)
			if err != nil {
				return err
			}

			verifier := workflow.AllowAllVerifier{}
			if err := verifier.Verify(name); err != nil {
				return fmt.Errorf("verify workflow %q: %w", name, err)
			}

			plan, err := workflow.BuildPlan(wf, params)
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			if dryRun {
				printPlan(out, plan)
				return nil
			}

			exe := workflow.NewExecutor(exec.OSRunner{},
				workflow.WithDefaultStripGlobs(cfg.Workflow.StripGlobs))
			wctx := workflow.NewContext(nil)
			for k, v := range params {
				wctx.Set(k, v)
			}
			if err := exe.Run(cmd.Context(), plan, wctx); err != nil {
				return err
			}
			fmt.Fprintf(out, "workflow %q completed\n", plan.Name)
			return nil
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print the resolved plan without running any step")
	cmd.Flags().StringArrayVar(&rawParams, "param", nil, "workflow param as key=value (repeatable)")
	return cmd
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
