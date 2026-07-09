package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	branchpkg "github.com/cameronsjo/forgectl/internal/branch"
	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/forgive"
)

// newBranchCmd builds `forgectl branch` — mirrors newNetCmd/newDockerCmd in
// building its own exec.Runner rather than sharing another domain's client
// lifecycle. Ships flat (no `forgectl git` parent) — see internal/branch's
// package doc for why.
func newBranchCmd(cfg config.Config) *cobra.Command {
	client := branchpkg.New(exec.OSRunner{})
	return newBranchCmdForClient(client)
}

// newBranchCmdForClient builds the command over an already-constructed
// client — split out so tests can inject a fake-wired *branch.Client (mirrors
// newNetCmdForClient/newDockerCmdForClient) without going through newBranchCmd.
func newBranchCmdForClient(client *branchpkg.Client) *cobra.Command {
	var (
		local, remote, includeGone, apply bool
		remoteName                        string
	)

	cmd := &cobra.Command{
		Use:     "branch",
		Aliases: forgive.BranchAliases,
		Short:   "Prune stale/orphaned git branches (dry-run by default)",
		Long: `branch enumerates local and/or remote-tracking branches, classifies each as
safe-to-delete, blocked, or needs-attention against SERVER-SIDE PR truth — never
local "git branch --merged" alone, which misses squash-merged branches — and
prints the grouped report. Nothing is deleted without --apply, which is
gated by a confirmation prompt.

  forgectl branch                    dry-run report, local + remote branches
  forgectl branch --local            local branches only
  forgectl branch --remote           remote-tracking branches only
  forgectl branch --include-gone     also surface upstream-gone branches with
                                      no server-confirmed merge (needs-attention)
  forgectl branch --apply            delete everything classified safe-to-delete,
                                      after a confirmation prompt

A stacked/dependent branch's own retargeting is a manual step this command
never attempts — it only ever reports and deletes.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			useLocal, useRemote := local, remote
			if !useLocal && !useRemote {
				useLocal, useRemote = true, true
			}
			return runBranch(cmd, client, branchRunOptions{
				local:       useLocal,
				remote:      useRemote,
				remoteName:  remoteName,
				includeGone: includeGone,
				apply:       apply,
			})
		},
	}
	cmd.Flags().BoolVar(&local, "local", false, "consider local branches (default: both, if neither --local nor --remote is given)")
	cmd.Flags().BoolVar(&remote, "remote", false, "consider remote-tracking branches (default: both, if neither --local nor --remote is given)")
	cmd.Flags().StringVar(&remoteName, "remote-name", "origin", "remote to query/prune against")
	cmd.Flags().BoolVar(&includeGone, "include-gone", false, "also surface upstream-gone branches with no server-confirmed merge")
	cmd.Flags().BoolVar(&apply, "apply", false, "delete safe-to-delete branches, after a confirmation prompt")
	return cmd
}

// branchRunOptions bundles the resolved flag values RunE hands to runBranch.
type branchRunOptions struct {
	local, remote bool
	remoteName    string
	includeGone   bool
	apply         bool
}

// runBranch enumerates, prints the grouped report, and — only with --apply,
// after a confirmation prompt — prunes everything classified safe-to-delete.
func runBranch(cmd *cobra.Command, client *branchpkg.Client, opts branchRunOptions) error {
	ctx := cmd.Context()
	out := cmd.OutOrStdout()

	report, err := client.Enumerate(ctx, branchpkg.EnumerateOptions{
		Local:       opts.local,
		Remote:      opts.remote,
		RemoteName:  opts.remoteName,
		IncludeGone: opts.includeGone,
	})
	if err != nil {
		return err
	}

	printBranchGroup(out, "safe-to-delete", report.SafeToDelete)
	printBranchGroup(out, "blocked", report.Blocked)
	printBranchGroup(out, "needs-attention", report.NeedsAttention)

	if !opts.apply {
		if len(report.SafeToDelete) > 0 {
			fmt.Fprintf(out, "\n%d branch(es) safe to delete — re-run with --apply to delete them\n", len(report.SafeToDelete))
		}
		return nil
	}

	if len(report.SafeToDelete) == 0 {
		fmt.Fprintln(out, "\nnothing to prune")
		return nil
	}

	ok, err := confirm(fmt.Sprintf("Delete %d branch(es) classified safe-to-delete?", len(report.SafeToDelete)))
	if err != nil {
		return err
	}
	if !ok {
		fmt.Fprintln(out, "cancelled")
		return nil
	}

	results := client.Prune(ctx, report.SafeToDelete, branchpkg.PruneOptions{
		RemoteName: opts.remoteName,
		Local:      opts.local,
		Remote:     opts.remote,
	})

	fmt.Fprintln(out)
	for _, r := range results {
		switch {
		case r.Err != nil:
			fmt.Fprintf(out, "FAILED  %s: %v\n", r.Name, r.Err)
		case r.Skipped:
			fmt.Fprintf(out, "skipped %s: %s\n", r.Name, r.Reason)
		case r.Deleted:
			fmt.Fprintf(out, "deleted %s\n", r.Name)
		}
	}
	return nil
}

// printBranchGroup prints one report section, or nothing at all when empty —
// an empty "blocked (0):" header on every run would be noise.
func printBranchGroup(out io.Writer, label string, items []branchpkg.Classification) {
	if len(items) == 0 {
		return
	}
	fmt.Fprintf(out, "%s (%d):\n", label, len(items))
	for _, item := range items {
		fmt.Fprintf(out, "  %s — %s\n", item.Info.Name, item.Reason)
	}
}
