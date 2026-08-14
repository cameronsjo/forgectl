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
		agent            string
		dryRun           bool
		noVerify         bool
		operatorAuthored bool
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
  forgectl pr local --dry-run      resolve + print the plan, create nothing

The --agent codex reviewer is NOT confined the way the default is: its sandbox
scopes writes and network but not which commands run, so it can execute shell
and read your whole home directory. It therefore requires --operator-authored,
which asserts that you wrote the code under review. A local path does not imply
that — ` + "`gh pr checkout`" + ` puts someone else's commit in your own repo.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path := "."
			if len(args) == 1 {
				path = args[0]
			}
			ctx := cmd.Context()
			if !dryRun {
				if err := client.CheckDispatchCapability(ctx); err != nil {
					return err
				}
			}

			sess, err := client.PrepareLocal(ctx, path, pr.PrepareLocalOpts{
				Agent:      resolveAgent(agent),
				DryRun:     dryRun,
				Provenance: localProvenance(operatorAuthored),
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

			dispatch, err := client.Launch(ctx, sess, cfg)
			if err != nil {
				return err
			}
			if err := dispatchVerificationError(verifyReviewDispatches(ctx, client, []pr.Dispatch{dispatch}, noVerify)); err != nil {
				return err
			}
			fmt.Fprintf(out, "prepared local clean-room review of %s @ %s\n", sess.HeadRef, sess.HeadOid)
			fmt.Fprintf(out, "  workspace: %s\n", sess.Workspace)
			fmt.Fprintf(out, "  findings: %s\n", sess.FindingsDir)
			fmt.Fprintf(out, "  breadcrumb: %s\n", sess.Path)
			return nil
		},
	}
	cmd.Flags().StringVar(&agent, "agent", "", "review agent (env "+prAgentEnv+"; default: claude; codex requires --operator-authored)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "resolve and print the plan without creating anything")
	cmd.Flags().BoolVar(&noVerify, "no-verify", false, "skip the delayed post-dispatch window check")
	cmd.Flags().BoolVar(&operatorAuthored, "operator-authored", false,
		"assert that you wrote the code under review; required for --agent codex, "+
			"which permits the reviewer arbitrary shell and host-wide reads")
	return cmd
}

// localProvenance maps the --operator-authored flag onto the provenance axis.
//
// The mapping is deliberately one-way: the flag's presence asserts authorship,
// and its ABSENCE asserts nothing at all — unknown, not third-party. `pr local`
// genuinely does not know whose commit is in the tree (that is the whole of
// forgectl#232), and claiming otherwise would be inventing a fact. Both values
// refuse Codex; the distinction is what lets the refusal offer this flag to the
// one operator who can honestly use it.
func localProvenance(operatorAuthored bool) pr.ReviewProvenance {
	if operatorAuthored {
		return pr.ReviewProvenanceOperatorAuthored
	}
	return pr.ReviewProvenanceUnknown
}
