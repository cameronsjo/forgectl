package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/pr"
)

// newPrLocalCmd builds `forgectl pr local` — the offline sibling of `pr
// <ref>`: it reviews the LOCAL committed changes in a repo with no GitHub
// round-trip at all. There is no --headless flag: a local session has no
// PostReview path to gate, so it would be a no-op.
func newPrLocalCmd(client *pr.Client, cfg config.Config) *cobra.Command {
	var (
		agent  string
		dryRun bool
	)

	cmd := &cobra.Command{
		Use:   "local [path]",
		Short: "Offline clean-room review of local committed changes",
		Long: `local sets up the same isolated, deny-by-default clean room as ` + "`pr <ref>`" + `,
but entirely offline: it sandboxes the local HEAD of path (default: the
current directory) into a throwaway worktree, quarantines any AI-instruction
files, writes a read-only agent allowlist that denies every network CLI, and
dispatches a review agent into a tmux window. There is no GitHub round-trip:
the review reads only committed changes, and findings are written to a
writable escape-hatch directory rather than posted anywhere.

  forgectl pr local                jump into a review of the cwd's HEAD
  forgectl pr local ../other-repo  review a different local repo
  forgectl pr local --dry-run      resolve + print the plan, create nothing`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path := "."
			if len(args) == 1 {
				path = args[0]
			}
			ctx := cmd.Context()

			sess, err := client.PrepareLocal(ctx, path, pr.PrepareLocalOpts{
				Agent:  resolveAgent(agent),
				DryRun: dryRun,
			})
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			if dryRun {
				displayAgent := agentDisplayLabel(sess.Agent)
				fmt.Fprintf(out, "plan: local review %s @ %s\n", sess.HeadRef, sess.HeadOid)
				fmt.Fprintf(out, "  agent: %s\n", displayAgent)
				fmt.Fprintln(out, "  worktree -> quarantine -> launch agent (local profile: read-only git, no network CLI, one writable findings dir)")
				fmt.Fprintln(out, "  (dry-run: no workspace, window, or breadcrumb created)")
				return nil
			}

			if err := client.Launch(ctx, sess, cfg); err != nil {
				return err
			}
			fmt.Fprintf(out, "prepared local clean-room review of %s @ %s\n", sess.HeadRef, sess.HeadOid)
			fmt.Fprintf(out, "  workspace: %s\n", sess.Workspace)
			fmt.Fprintf(out, "  findings: %s\n", sess.FindingsDir)
			fmt.Fprintf(out, "  breadcrumb: %s\n", sess.Path)
			return nil
		},
	}
	cmd.Flags().StringVar(&agent, "agent", "", "review agent (env "+prAgentEnv+"; default: claude)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "resolve and print the plan without creating anything")
	return cmd
}
