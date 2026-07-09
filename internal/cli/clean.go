package cli

import (
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	cleanpkg "github.com/cameronsjo/forgectl/internal/clean"
	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/forgive"
)

// newCleanCmd builds `forgectl clean` — mirrors newBranchCmd/newDockerCmd in
// building its own exec.Runner rather than sharing another domain's client
// lifecycle. Ships flat (no subverbs), same as branch — PR-1 is dep/build-dir
// reclaim only; package-manager caches and docker prune are issue #4's
// follow-on PR.
func newCleanCmd(cfg config.Config) *cobra.Command {
	client := cleanpkg.New(exec.OSRunner{}, cleanpkg.WithCleanConfig(cfg.Clean))
	return newCleanCmdForClient(client)
}

// newCleanCmdForClient builds the command over an already-constructed
// client — split out so tests can inject a fake-wired *clean.Client (mirrors
// newBranchCmdForClient) without going through newCleanCmd.
func newCleanCmdForClient(client *cleanpkg.Client) *cobra.Command {
	var (
		root      string
		typeFlag  string
		olderThan time.Duration
		apply     bool
		force     bool
	)

	cmd := &cobra.Command{
		Use:     "clean",
		Aliases: forgive.CleanAliases,
		Short:   "Reclaim dep/build directories under a project root (dry-run by default)",
		Long: `clean scans --root (default ~/Projects) for reclaimable dependency and
build-output directories — node_modules, .venv/venv, __pycache__, target,
dist, .next, build, vendor, .svelte-kit — and reports each one's size plus a
total. Nothing is deleted without --apply, which is gated by a confirmation
prompt.

  forgectl clean                        dry-run report against ~/Projects
  forgectl clean --root ~/work          dry-run report against a different root
  forgectl clean --type node            only node_modules/.next/.svelte-kit
  forgectl clean --older-than 720h      only targets older than 30 days
  forgectl clean --apply                delete everything reclaimable, after
                                         a confirmation prompt
  forgectl clean --apply --force        also clean projects with an
                                         uncommitted/dirty git tree

A project with a dirty (uncommitted) git tree is skipped unless --force —
a stray uncommitted file inside dist/ shouldn't be nuked silently. .git is
never a target, and a symlinked directory is never followed out of --root.

Package-manager caches (npm/pip/go build cache/brew) and docker prune are
out of scope for this command — see issue #4's follow-on PR.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var types []cleanpkg.Kind
			if typeFlag != "" {
				k, err := cleanpkg.ParseKind(typeFlag)
				if err != nil {
					return err
				}
				types = []cleanpkg.Kind{k}
			}
			return runClean(cmd, client, cleanpkg.CleanOptions{
				Root:      root,
				Types:     types,
				OlderThan: olderThan,
				Apply:     apply,
				Force:     force,
			})
		},
	}
	cmd.Flags().StringVar(&root, "root", "", "root directory to scan (default: ~/Projects, or [clean] default_root)")
	cmd.Flags().StringVar(&typeFlag, "type", "", "only consider one type: node|python|go|build (default: all)")
	cmd.Flags().DurationVar(&olderThan, "older-than", 0, "only consider targets older than this (e.g. 720h for 30 days)")
	cmd.Flags().BoolVar(&apply, "apply", false, "delete reclaimable targets, after a confirmation prompt")
	cmd.Flags().BoolVar(&force, "force", false, "also clean projects with a dirty/uncommitted git tree")
	return cmd
}

// runClean scans, prints the report, and — only with --apply, after a
// confirmation prompt — deletes everything reclaimable, then reports actual
// reclaimed bytes.
func runClean(cmd *cobra.Command, client *cleanpkg.Client, opts cleanpkg.CleanOptions) error {
	ctx := cmd.Context()
	out := cmd.OutOrStdout()

	// Dry-run scan/classify first regardless of --apply — the confirmation
	// prompt below needs a real total to show, and this is also how a plain
	// `forgectl clean` (no --apply) gets its report.
	previewOpts := opts
	previewOpts.Apply = false
	preview, err := client.Clean(ctx, previewOpts)
	if err != nil {
		return err
	}

	printCleanItems(out, preview.Items)

	if preview.TotalReclaimable == 0 {
		fmt.Fprintln(out, "\nnothing to reclaim")
		return nil
	}
	fmt.Fprintf(out, "\n%s reclaimable across %d target(s)\n", formatBytes(preview.TotalReclaimable), countReclaimable(preview.Items))

	if !opts.Apply {
		fmt.Fprintln(out, "re-run with --apply to delete them")
		return nil
	}

	ok, err := confirm(fmt.Sprintf("Delete %s across %d target(s)?", formatBytes(preview.TotalReclaimable), countReclaimable(preview.Items)))
	if err != nil {
		return err
	}
	if !ok {
		fmt.Fprintln(out, "cancelled")
		return nil
	}

	result, err := client.Clean(ctx, opts)
	if err != nil {
		return err
	}

	fmt.Fprintln(out)
	for _, item := range result.Items {
		switch {
		case item.Err != nil:
			fmt.Fprintf(out, "FAILED  %s: %v\n", item.Path, item.Err)
		case item.Skipped:
			// Already printed in the preview pass above; apply-phase output
			// only needs to report what actually happened to a delete
			// attempt.
		case item.Deleted:
			fmt.Fprintf(out, "reclaimed %s (%s)\n", item.Path, formatBytes(item.Size))
		}
	}
	fmt.Fprintf(out, "\nreclaimed %s\n", formatBytes(result.TotalReclaimed))
	return nil
}

// printCleanItems prints the dry-run report: one line per matched target,
// grouped by whether it would be reclaimed or was skipped.
func printCleanItems(out io.Writer, items []cleanpkg.Item) {
	if len(items) == 0 {
		fmt.Fprintln(out, "no reclaimable directories found")
		return
	}
	for _, item := range items {
		if item.Skipped {
			fmt.Fprintf(out, "skip  %s — %s\n", item.Path, item.SkipReason)
			continue
		}
		fmt.Fprintf(out, "%-8s %s — %s\n", item.Kind, item.Path, formatBytes(item.Size))
	}
}

// countReclaimable counts the non-skipped items in items.
func countReclaimable(items []cleanpkg.Item) int {
	n := 0
	for _, item := range items {
		if !item.Skipped {
			n++
		}
	}
	return n
}

// formatBytes renders n as a human-readable size (KB/MB/GB/TB, base 1024) —
// a small local helper rather than pulling in a formatting dependency for
// one function.
func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
