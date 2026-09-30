package cli

import (
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	branchpkg "github.com/cameronsjo/forgectl/internal/branch"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/termsafe"
	"github.com/cameronsjo/forgectl/internal/theme"
)

// branchGroupAliases is branch's shorthand surface ("br") — migrated here
// from forgive.BranchAliases at conversion. branch ships flat (no subverbs),
// so this is a plain slice on the command itself, not a SubAliases map. A
// separate var (not a manifest-routed read) because newBranchCmdForClient
// also sets it — see yAliases for the initialization-cycle reason.
var branchGroupAliases = []string{"br"}

// branchModule declares the branch-pruning extension (ADR-0005).
var branchModule = module.Manifest{
	Name:         "branch",
	Tier:         module.TierExtension,
	GroupAliases: branchGroupAliases,
	New:          newBranchCmd,
}

// newBranchCmd builds `forgectl branch` over the registry Deps. Ships flat
// (no `forgectl git` parent) — see internal/branch's package doc for why.
func newBranchCmd(deps module.Deps) *cobra.Command {
	client := branchpkg.New(deps.Runner)
	return newBranchCmdForClient(client, deps.Theme)
}

// newBranchCmdForClient builds the command over an already-constructed
// client — split out so tests can inject a fake-wired *branch.Client (mirrors
// newNetCmdForClient/newDockerCmdForClient) without going through newBranchCmd.
func newBranchCmdForClient(client *branchpkg.Client, th theme.Theme) *cobra.Command {
	var (
		local, remote, includeGone, apply, asJSON bool
		remoteName                                string
	)

	cmd := &cobra.Command{
		Use:     "branch",
		Aliases: branchGroupAliases,
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
  forgectl branch --json             the dry-run report as JSON; refused with
                                      --apply (the delete arm is interactive)

A stacked/dependent branch's own retargeting is a manual step this command
never attempts — it only ever reports and deletes.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if asJSON && apply {
				return errors.New("--json reports the dry-run classification; it cannot be combined with --apply (the delete arm is interactive and reports separately)")
			}
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
				asJSON:      asJSON,
			}, th)
		},
	}
	cmd.Flags().BoolVar(&local, "local", false, "consider local branches (default: both, if neither --local nor --remote is given)")
	cmd.Flags().BoolVar(&remote, "remote", false, "consider remote-tracking branches (default: both, if neither --local nor --remote is given)")
	cmd.Flags().StringVar(&remoteName, "remote-name", "origin", "remote to query/prune against")
	cmd.Flags().BoolVar(&includeGone, "include-gone", false, "also surface upstream-gone branches with no server-confirmed merge")
	cmd.Flags().BoolVar(&apply, "apply", false, "delete safe-to-delete branches, after a confirmation prompt")
	cmd.Flags().BoolVar(&asJSON, "json", false, `emit {"safe_to_delete":[...],"blocked":[...],"needs_attention":[...]} of {"name","reason","local","remote","upstream_gone"} to stdout; not valid with --apply`)
	return cmd
}

// branchRunOptions bundles the resolved flag values RunE hands to runBranch.
type branchRunOptions struct {
	local, remote bool
	remoteName    string
	includeGone   bool
	apply         bool
	asJSON        bool
}

// runBranch enumerates, prints the grouped report, and — only with --apply,
// after a confirmation prompt — prunes everything classified safe-to-delete.
func runBranch(cmd *cobra.Command, client *branchpkg.Client, opts branchRunOptions, th theme.Theme) error {
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

	if opts.asJSON {
		return writeJSON(out, newBranchReportJSON(report))
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

	ok, err := confirm(th, fmt.Sprintf("Delete %d branch(es) classified safe-to-delete?", len(report.SafeToDelete)))
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
	printPruneResults(out, results)
	return nil
}

// printPruneResults writes one line per prune outcome. r.Name can be a
// remote-derived refname, which git allows to carry bidi overrides and C1
// controls, and r.Err can carry a path or a subprocess cause, so every
// value goes through termsafe (#658).
func printPruneResults(out io.Writer, results []branchpkg.PruneResult) {
	for _, r := range results {
		name := safeTitle(r.Name)
		switch {
		case r.Err != nil:
			_, _ = fmt.Fprintf(out, "FAILED  %s: %s\n", name, safeText(termsafe.Error(r.Err).Error()))
		case r.Skipped:
			_, _ = fmt.Fprintf(out, "skipped %s: %s\n", name, safeText(r.Reason))
		case r.Deleted:
			_, _ = fmt.Fprintf(out, "deleted %s\n", name)
		}
	}
}

// branchJSON is one classified branch in `branch --json` (additive-only,
// ADR-0008).
type branchJSON struct {
	Name         string `json:"name"`
	Reason       string `json:"reason"`
	Local        bool   `json:"local"`
	Remote       bool   `json:"remote"`
	UpstreamGone bool   `json:"upstream_gone"`
}

// branchReportJSON is the `branch --json` shape; every group is an array,
// never null.
type branchReportJSON struct {
	SafeToDelete   []branchJSON `json:"safe_to_delete"`
	Blocked        []branchJSON `json:"blocked"`
	NeedsAttention []branchJSON `json:"needs_attention"`
}

func newBranchReportJSON(r branchpkg.Report) branchReportJSON {
	conv := func(items []branchpkg.Classification) []branchJSON {
		out := make([]branchJSON, 0, len(items))
		for _, c := range items {
			out = append(out, branchJSON{
				Name: c.Info.Name, Reason: c.Reason,
				Local: c.Info.LocalExists, Remote: c.Info.RemoteExists,
				UpstreamGone: c.Info.UpstreamGone,
			})
		}
		return out
	}
	return branchReportJSON{
		SafeToDelete:   conv(r.SafeToDelete),
		Blocked:        conv(r.Blocked),
		NeedsAttention: conv(r.NeedsAttention),
	}
}

// printBranchGroup prints one report section, or nothing at all when empty —
// an empty "blocked (0):" header on every run would be noise.
func printBranchGroup(out io.Writer, label string, items []branchpkg.Classification) {
	if len(items) == 0 {
		return
	}
	fmt.Fprintf(out, "%s (%d):\n", label, len(items))
	for _, item := range items {
		// Same rule as printPruneResults: the name can be remote-derived.
		_, _ = fmt.Fprintf(out, "  %s — %s\n", safeTitle(item.Info.Name), safeText(item.Reason))
	}
}
