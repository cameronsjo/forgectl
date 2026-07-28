package cli

import (
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	cleanpkg "github.com/cameronsjo/forgectl/internal/clean"
	"github.com/cameronsjo/forgectl/internal/module"
)

// cleanGroupAliases is clean's shorthand surface ("cln" — "cl" is claimed by
// launch) — migrated here from forgive.CleanAliases at conversion. Flat
// command, plain slice; separate var for the same initialization-cycle
// reason as yAliases.
var cleanGroupAliases = []string{"cln"}

// cleanModule declares the dep/build-dir reclaim extension (ADR-0005): owns
// the [clean] config section.
var cleanModule = module.Manifest{
	Name:         "clean",
	Tier:         module.TierExtension,
	ConfigKey:    "clean",
	GroupAliases: cleanGroupAliases,
	New:          newCleanCmd,
}

// newCleanCmd builds `forgectl clean` over the registry Deps. Ships flat (no
// subverbs), same as branch — dep/build-dir reclaim is the always-on core
// (PR-1); package-manager caches and docker prune (PR-2, issue #4's
// follow-on) are the --caches/--docker opt-in passes.
func newCleanCmd(deps module.Deps) *cobra.Command {
	client := cleanpkg.New(deps.Runner, cleanpkg.WithCleanConfig(deps.Cfg.Clean))
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
		caches    bool
		docker    bool
	)

	cmd := &cobra.Command{
		Use:     "clean",
		Aliases: cleanGroupAliases,
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
  forgectl clean --caches --apply       also clear detected package-manager
                                         caches (npm/pnpm/pip/go/brew)
  forgectl clean --docker --apply       also prune docker (containers,
                                         images, volumes, build cache)

A project with a dirty (uncommitted) git tree is skipped unless --force —
a stray uncommitted file inside dist/ shouldn't be nuked silently. .git is
never a target, and a symlinked directory is never followed out of --root.

--caches and --docker are OPT-IN, isolated passes that run independently of
the dep/build-dir scan above and of each other: each gets its own dry-run
preview and confirmation prompt, and one tool/category failing to prune
never blocks the rest. They're opt-in because clearing a warm cache or
pruning docker is slower and more disruptive than dep/build-dir reclaim,
which only removes trivially regenerable output.`,
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
			}, caches, docker)
		},
	}
	cmd.Flags().StringVar(&root, "root", "", "root directory to scan (default: ~/Projects, or [clean] default_root)")
	cmd.Flags().StringVar(&typeFlag, "type", "", "only consider one type: node|python|go|build (default: all)")
	cmd.Flags().DurationVar(&olderThan, "older-than", 0, "only consider targets older than this (e.g. 720h for 30 days)")
	cmd.Flags().BoolVar(&apply, "apply", false, "delete reclaimable targets, after a confirmation prompt")
	cmd.Flags().BoolVar(&force, "force", false, "also clean projects with a dirty/uncommitted git tree")
	cmd.Flags().BoolVar(&caches, "caches", false, "also reclaim detected package-manager caches (npm/pnpm/pip/go/brew) — opt-in, isolated per tool")
	cmd.Flags().BoolVar(&docker, "docker", false, "also prune docker (containers/images/volumes/build cache) — opt-in, isolated per category")
	return cmd
}

// runClean runs the dep/build-dir reclaim pass, then — only when requested
// — the --caches and --docker opt-in passes, each fully isolated: they run
// regardless of what the dep/build-dir pass found or decided (nothing to
// reclaim, cancelled, whatever), and one failing never blocks the others.
func runClean(cmd *cobra.Command, client *cleanpkg.Client, opts cleanpkg.CleanOptions, includeCaches, includeDocker bool) error {
	if err := runCleanDirs(cmd, client, opts); err != nil {
		return err
	}
	if includeCaches {
		if err := runCleanCaches(cmd, client, opts.Apply); err != nil {
			return err
		}
	}
	if includeDocker {
		if err := runCleanDocker(cmd, client, opts.Apply); err != nil {
			return err
		}
	}
	return nil
}

// runCleanDirs is the original dep/build-dir reclaim pass: scans, prints
// the report, and — only with --apply, after a confirmation prompt —
// deletes everything reclaimable, then reports actual reclaimed bytes.
func runCleanDirs(cmd *cobra.Command, client *cleanpkg.Client, opts cleanpkg.CleanOptions) error {
	ctx := cmd.Context()
	out := cmd.OutOrStdout()

	// Scan exactly ONCE, up front, regardless of --apply — both the preview
	// classification below and (if confirmed) the apply pass reuse this
	// SAME Report. Re-scanning between preview and apply would let the
	// filesystem move in between (a target added, removed, or resized),
	// silently making the deleted set differ from what the user confirmed.
	resolvedRoot, report, err := client.ScanReport(opts)
	if err != nil {
		return err
	}

	previewOpts := opts
	previewOpts.Apply = false
	preview, err := client.ApplyReport(ctx, resolvedRoot, report, previewOpts)
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

	result, err := client.ApplyReport(ctx, resolvedRoot, report, opts)
	if err != nil {
		return err
	}

	fmt.Fprintln(out)
	failed := 0
	for _, item := range result.Items {
		switch {
		case item.Err != nil:
			fmt.Fprintf(out, "FAILED  %s: %v\n", item.Path, item.Err)
			failed++
		case item.Skipped:
			// Already printed in the preview pass above; apply-phase output
			// only needs to report what actually happened to a delete
			// attempt.
		case item.Deleted:
			fmt.Fprintf(out, "reclaimed %s (%s)\n", item.Path, formatBytes(item.Size))
		}
	}
	fmt.Fprintf(out, "\nreclaimed %s\n", formatBytes(result.TotalReclaimed))
	if failed > 0 {
		// A partial delete failure must not exit 0 — a scripted caller
		// reading only the exit code needs to see that not everything
		// reported above actually reclaimed. Each failure was already
		// printed individually above; this is the aggregate signal.
		return fmt.Errorf("%d target(s) failed to reclaim", failed)
	}
	return nil
}

// runCleanCaches is the --caches opt-in pass: a dry-run preview of every
// detected package-manager cache's size, then — only with apply, after its
// OWN confirmation prompt (mirroring runCleanDirs' gate exactly) — each
// tool's own prune command, isolated per tool.
func runCleanCaches(cmd *cobra.Command, client *cleanpkg.Client, apply bool) error {
	ctx := cmd.Context()
	out := cmd.OutOrStdout()

	items := client.ScanCaches(ctx, nil)
	fmt.Fprintln(out, "\npackage-manager caches:")
	printCacheItems(out, items)

	total := totalCacheSize(items)
	detected := countCacheDetected(items)
	if total == 0 {
		fmt.Fprintln(out, "nothing to reclaim")
		return nil
	}
	fmt.Fprintf(out, "%s reclaimable across %d cache(s)\n", formatBytes(total), detected)

	if !apply {
		fmt.Fprintln(out, "re-run with --apply to clear them")
		return nil
	}

	ok, err := confirm(fmt.Sprintf("Clear %s across %d package-manager cache(s)?", formatBytes(total), detected))
	if err != nil {
		return err
	}
	if !ok {
		fmt.Fprintln(out, "cancelled")
		return nil
	}

	result := client.PruneCaches(ctx, items)
	failed := 0
	for _, item := range result {
		switch {
		case item.Skipped:
			// Already printed in the preview pass above.
		case item.Err != nil:
			fmt.Fprintf(out, "FAILED  %s: %v\n", item.Kind, item.Err)
			failed++
		case item.Applied:
			fmt.Fprintf(out, "cleared %s (%s)\n", item.Kind, formatBytes(item.Size))
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d cache(s) failed to clear", failed)
	}
	return nil
}

// runCleanDocker is the --docker opt-in pass: a dry-run preview of docker's
// own reported reclaimable size per category, then — only with apply,
// after its OWN confirmation prompt — each category's own prune command,
// isolated per category.
func runCleanDocker(cmd *cobra.Command, client *cleanpkg.Client, apply bool) error {
	ctx := cmd.Context()
	out := cmd.OutOrStdout()

	items := client.ScanDocker(ctx)
	fmt.Fprintln(out, "\ndocker prune:")
	printDockerItems(out, items)

	if !anyDockerDetected(items) {
		fmt.Fprintln(out, "docker unreachable; skipping")
		return nil
	}
	total := totalDockerReclaimable(items)
	if total == 0 {
		fmt.Fprintln(out, "nothing to reclaim")
		return nil
	}
	fmt.Fprintf(out, "%s reclaimable from docker\n", formatBytes(total))

	if !apply {
		fmt.Fprintln(out, "re-run with --apply to prune")
		return nil
	}

	ok, err := confirm(fmt.Sprintf("Prune %s from docker (containers/images/volumes/build cache)?", formatBytes(total)))
	if err != nil {
		return err
	}
	if !ok {
		fmt.Fprintln(out, "cancelled")
		return nil
	}

	result := client.PruneDocker(ctx, items)
	failed := 0
	for _, item := range result {
		switch {
		case item.Skipped:
			// Already printed in the preview pass above.
		case item.Err != nil:
			fmt.Fprintf(out, "FAILED  %s: %v\n", item.Kind, item.Err)
			failed++
		case item.Applied:
			fmt.Fprintf(out, "pruned %s\n", item.Kind)
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d docker categories failed to prune", failed)
	}
	return nil
}

// printCacheItems prints the --caches dry-run report: one line per probed
// tool, grouped by whether it was detected.
func printCacheItems(out io.Writer, items []cleanpkg.CacheItem) {
	for _, item := range items {
		if item.Skipped {
			fmt.Fprintf(out, "skip  %-5s — %s\n", item.Kind, item.SkipReason)
			continue
		}
		fmt.Fprintf(out, "%-5s %s — %s\n", item.Kind, item.Path, formatBytes(item.Size))
	}
}

// totalCacheSize sums the size of every non-skipped (detected) cache item.
func totalCacheSize(items []cleanpkg.CacheItem) int64 {
	var total int64
	for _, item := range items {
		if !item.Skipped {
			total += item.Size
		}
	}
	return total
}

// countCacheDetected counts the non-skipped (detected) items in items.
func countCacheDetected(items []cleanpkg.CacheItem) int {
	n := 0
	for _, item := range items {
		if !item.Skipped {
			n++
		}
	}
	return n
}

// printDockerItems prints the --docker dry-run report: one line per
// category, grouped by whether docker reported it. Falls back to docker's
// own raw size string when the byte parse didn't succeed cleanly, so the
// user still sees something concrete rather than a silently wrong "0 B".
func printDockerItems(out io.Writer, items []cleanpkg.DockerItem) {
	for _, item := range items {
		if item.Skipped {
			fmt.Fprintf(out, "skip  %-11s — %s\n", item.Kind, item.SkipReason)
			continue
		}
		size := formatBytes(item.Reclaimable)
		if item.Reclaimable == 0 && item.Reported != "" {
			size = item.Reported
		}
		fmt.Fprintf(out, "%-11s %s\n", item.Kind, size)
	}
}

// totalDockerReclaimable sums the parsed reclaimable bytes across items.
func totalDockerReclaimable(items []cleanpkg.DockerItem) int64 {
	var total int64
	for _, item := range items {
		total += item.Reclaimable
	}
	return total
}

// anyDockerDetected reports whether ScanDocker reached docker at all (at
// least one category is non-skipped) — distinguishes "docker unreachable"
// from "docker reachable, nothing to reclaim".
func anyDockerDetected(items []cleanpkg.DockerItem) bool {
	for _, item := range items {
		if !item.Skipped {
			return true
		}
	}
	return false
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
