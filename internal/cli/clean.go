package cli

import (
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/spf13/cobra"

	cleanpkg "github.com/cameronsjo/forgectl/internal/clean"
	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/redact"
	"github.com/cameronsjo/forgectl/internal/termsafe"
	"github.com/cameronsjo/forgectl/internal/theme"
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
	return newCleanCmdForClient(client, deps.Theme)
}

// newCleanCmdForClient builds the command over an already-constructed
// client — split out so tests can inject a fake-wired *clean.Client (mirrors
// newBranchCmdForClient) without going through newCleanCmd.
func newCleanCmdForClient(client *cleanpkg.Client, th theme.Theme) *cobra.Command {
	var (
		root      string
		typeFlag  string
		olderThan time.Duration
		apply     bool
		force     bool
		caches    bool
		docker    bool
		asJSON    bool
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
  forgectl clean --json                 the dep/build-dir dry-run report as
                                         JSON; refused with --apply, --caches
                                         or --docker

A project with a dirty (uncommitted) git tree is skipped unless --force —
a stray uncommitted file inside dist/ shouldn't be nuked silently. .git is
never a target, and a symlinked directory is never followed out of --root.

--caches and --docker are OPT-IN passes that run independently of the
dep/build-dir scan above and of each other — each gets its own dry-run
preview and confirmation prompt, and a failure in one pass never prevents
the others from running (their errors are combined and reported together,
not short-circuited). They're opt-in because clearing a warm cache or
pruning docker is slower and more disruptive than dep/build-dir reclaim,
which only removes trivially regenerable output.

Answering "No" at any one of these prompts cancels ONLY that pass — with
both --caches and --docker set, a "No" to the caches prompt still presents
the docker prompt afterward, rather than aborting the whole run. Ctrl-C
(or any prompt error) stops everything immediately; only an explicit "No"
has this narrower, per-pass scope.

--type only filters the dep/build-dir pass (its node|python|go|build
vocabulary doesn't describe a cache or a docker category) and cannot be
combined with --caches or --docker.

CAUTION: --caches' brew target runs plain "brew cleanup", which ALSO
removes old versions of every installed formula/cask from the Cellar —
not just Homebrew's download cache. A deliberately-retained older keg
(e.g. an unlinked version kept for a known-good rollback) would be removed
by this. The confirmation prompt names this explicitly; there is no brew
verb that clears only the cache.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if typeFlag != "" && (caches || docker) {
				// Lead with a word, not the flag token: fang capitalizes the
				// first letter of a styled error, and "--Type" is not a flag.
				return fmt.Errorf("cannot combine --type with --caches or --docker: --type only filters the dep/build-dir pass (the other passes scan a fixed target set, not the node|python|go|build vocabulary)")
			}
			if asJSON && (apply || caches || docker) {
				return fmt.Errorf("cannot combine --json with --apply, --caches or --docker: --json reports the dep/build-dir dry-run only")
			}
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
			}, caches, docker, asJSON, th)
		},
	}
	cmd.Flags().StringVar(&root, "root", "", "root directory to scan (default: ~/Projects, or [clean] default_root)")
	cmd.Flags().StringVar(&typeFlag, "type", "", "only consider one type: node|python|go|build (default: all; cannot combine with --caches/--docker)")
	cmd.Flags().DurationVar(&olderThan, "older-than", 0, "only consider targets older than this (e.g. 720h for 30 days)")
	cmd.Flags().BoolVar(&apply, "apply", false, "delete reclaimable targets, after a confirmation prompt")
	cmd.Flags().BoolVar(&force, "force", false, "also clean projects with a dirty/uncommitted git tree")
	cmd.Flags().BoolVar(&caches, "caches", false, "also reclaim detected package-manager caches (npm/pnpm/pip/go/brew) — opt-in, isolated per tool; brew ALSO clears old Cellar versions")
	cmd.Flags().BoolVar(&docker, "docker", false, "also prune docker (containers/images/volumes/build cache) — opt-in, isolated per category")
	cmd.Flags().BoolVar(&asJSON, "json", false, `emit {"root":...,"items":[{"path","kind","size_bytes","skipped","skip_reason"}],"total_reclaimable_bytes":...} to stdout (the total counts non-skipped items only); dry-run only, not valid with --apply/--caches/--docker`)
	return cmd
}

// runClean runs the dep/build-dir reclaim pass, then — only when requested
// — the --caches and --docker opt-in passes. All three ACTUALLY run
// independently: each pass's error is collected rather than returned
// immediately, so a failure (or a non-zero exit) in an earlier pass never
// prevents a later one from running — every error is combined and
// surfaced together at the end (errors.Join returns nil when errs is
// empty, so the all-success case is unaffected).
func runClean(cmd *cobra.Command, client *cleanpkg.Client, opts cleanpkg.CleanOptions, includeCaches, includeDocker, asJSON bool, th theme.Theme) error {
	var errs []error
	if err := runCleanDirs(cmd, client, opts, asJSON, th); err != nil {
		errs = append(errs, fmt.Errorf("dep/build-dir pass: %w", err))
	}
	if includeCaches {
		if err := runCleanCaches(cmd, client, opts.Apply, th); err != nil {
			errs = append(errs, fmt.Errorf("caches pass: %w", err))
		}
	}
	if includeDocker {
		if err := runCleanDocker(cmd, client, opts.Apply, th); err != nil {
			errs = append(errs, fmt.Errorf("docker pass: %w", err))
		}
	}
	return errors.Join(errs...)
}

// runCleanDirs is the original dep/build-dir reclaim pass: scans, prints
// the report, and — only with --apply, after a confirmation prompt —
// deletes everything reclaimable, then reports actual reclaimed bytes.
func runCleanDirs(cmd *cobra.Command, client *cleanpkg.Client, opts cleanpkg.CleanOptions, asJSON bool, th theme.Theme) error {
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

	if asJSON {
		return writeJSON(out, newCleanReportJSON(resolvedRoot, preview))
	}

	printCleanItems(out, preview.Items)

	if preview.TotalReclaimable == 0 {
		_, _ = fmt.Fprintln(out, "\nnothing to reclaim")
		return nil
	}
	_, _ = fmt.Fprintf(out, "\n%s reclaimable across %d target(s)\n", formatBytes(preview.TotalReclaimable), countReclaimable(preview.Items))

	if !opts.Apply {
		_, _ = fmt.Fprintln(out, "re-run with --apply to delete them")
		return nil
	}

	ok, err := confirmFn(th, fmt.Sprintf("Delete %s across %d target(s)?", formatBytes(preview.TotalReclaimable), countReclaimable(preview.Items)))
	if err != nil {
		return err
	}
	if !ok {
		return noteCancelled(out)
	}

	result, err := client.ApplyReport(ctx, resolvedRoot, report, opts)
	if err != nil {
		return err
	}

	_, _ = fmt.Fprintln(out)
	failed := 0
	for _, item := range result.Items {
		switch {
		case item.Err != nil:
			_, _ = fmt.Fprintf(out, "FAILED  %s: %s\n", termsafe.QuotePath(item.Path), safeText(termsafe.Error(item.Err).Error()))
			failed++
		case item.Skipped:
			// Already printed in the preview pass above; apply-phase output
			// only needs to report what actually happened to a delete
			// attempt.
		case item.Deleted:
			_, _ = fmt.Fprintf(out, "reclaimed %s (%s)\n", termsafe.QuotePath(item.Path), formatBytes(item.Size))
		}
	}
	_, _ = fmt.Fprintf(out, "\nreclaimed %s\n", formatBytes(result.TotalReclaimed))
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
func runCleanCaches(cmd *cobra.Command, client *cleanpkg.Client, apply bool, th theme.Theme) error {
	ctx := cmd.Context()
	out := cmd.OutOrStdout()

	items := client.ScanCaches(ctx, nil)
	_, _ = fmt.Fprintln(out, "\npackage-manager caches:")
	printCacheItems(out, items)

	total := totalCacheSize(items)
	detected := countCacheDetected(items)
	if total == 0 {
		_, _ = fmt.Fprintln(out, "nothing to reclaim")
		return nil
	}
	_, _ = fmt.Fprintf(out, "%s reclaimable across %d cache(s)\n", formatBytes(total), detected)

	if !apply {
		_, _ = fmt.Fprintln(out, "re-run with --apply to clear them")
		return nil
	}

	prompt := fmt.Sprintf("Clear %s across %d package-manager cache(s)?", formatBytes(total), detected)
	if hasCacheKind(items, cleanpkg.CacheBrew) {
		prompt += " (brew's cleanup ALSO removes old formula/cask versions from the Cellar, not just its download cache)"
	}
	if hasCacheKind(items, cleanpkg.CachePnpm) {
		prompt += " (the pnpm figure sizes its WHOLE content-addressable store; store prune only removes unreferenced packages, so actual reclaim is typically much less)"
	}
	ok, err := confirmFn(th, prompt)
	if err != nil {
		return err
	}
	if !ok {
		return noteCancelled(out)
	}

	result := client.PruneCaches(ctx, items)
	failed := 0
	var totalReclaimed int64
	for _, item := range result {
		switch {
		case item.Skipped:
			// Already printed in the preview pass above.
		case item.Err != nil:
			// termsafe.Error, not Categorical, on purpose: see cleanFailureText.
			_, _ = fmt.Fprintf(out, "FAILED  %s: %s\n", cacheDisplayName(item.Kind), cleanFailureText(item.Err))
			failed++
		case item.Applied:
			// Reclaimed is the ACTUAL measured delta (dirSize before vs
			// after the prune), never the pre-prune Size estimate.
			_, _ = fmt.Fprintf(out, "cleared %s (%s)\n", cacheDisplayName(item.Kind), formatBytes(item.Reclaimed))
			totalReclaimed += item.Reclaimed
		}
	}
	_, _ = fmt.Fprintf(out, "\nreclaimed %s\n", formatBytes(totalReclaimed))
	if failed > 0 {
		return fmt.Errorf("%d cache(s) failed to clear", failed)
	}
	return nil
}

// hasCacheKind reports whether items includes a detected (non-skipped)
// target of the given kind — the CLI's signal to append a kind-specific
// caveat to the confirmation prompt. Brew (old Cellar versions) and pnpm
// (whole-store preview vs. partial prune) are both disclosed this way;
// folded into one helper rather than a hasBrewTarget/hasPnpmTarget pair
// that differed only in the Kind literal (forgectl#165 review).
func hasCacheKind(items []cleanpkg.CacheItem, kind cleanpkg.CacheKind) bool {
	for _, item := range items {
		if !item.Skipped && item.Kind == kind {
			return true
		}
	}
	return false
}

// cacheDisplayName renders a human-facing label for kind — identical to the
// bare Kind for every probe except brew, whose underlying `brew cleanup`
// command ALSO removes old formula/cask versions from the Cellar, not just
// its download cache. Disclosed here so the scan item, the confirmation
// prompt (see hasCacheKind above), and clean.go's Long help never promise
// a narrower blast radius than the command actually has.
func cacheDisplayName(kind cleanpkg.CacheKind) string {
	switch kind {
	case cleanpkg.CacheBrew:
		return "brew (cache + old Cellar versions)"
	case cleanpkg.CachePnpm:
		return "pnpm (whole store — prune reclaims less, see below)"
	default:
		return string(kind)
	}
}

// runCleanDocker is the --docker opt-in pass: a dry-run preview of docker's
// own reported reclaimable size per category, then — only with apply,
// after its OWN confirmation prompt — each category's own prune command,
// isolated per category.
func runCleanDocker(cmd *cobra.Command, client *cleanpkg.Client, apply bool, th theme.Theme) error {
	ctx := cmd.Context()
	out := cmd.OutOrStdout()

	items := client.ScanDocker(ctx)
	_, _ = fmt.Fprintln(out, "\ndocker prune:")
	printDockerItems(out, items)

	if !anyDockerDetected(items) {
		_, _ = fmt.Fprintln(out, "docker unreachable; skipping")
		return nil
	}
	total := totalDockerReclaimable(items)
	unknown := anyDockerReclaimableUnknown(items)
	switch {
	case total == 0 && !unknown:
		// A GENUINE zero: every non-skipped category parsed cleanly to 0.
		_, _ = fmt.Fprintln(out, "nothing to reclaim")
		return nil
	case unknown && total == 0:
		// NOT a genuine zero — one or more categories' size string (shown
		// above) didn't parse, so the true total is unknown, not zero.
		// Saying "nothing to reclaim" here would contradict a nonzero-
		// looking line printed one line above it (forgectl#165 item 6).
		_, _ = fmt.Fprintln(out, "reclaimable size unknown for one or more categories (see above) — docker's own output didn't parse cleanly, so this is NOT reported as zero")
	case unknown:
		// total > 0 here, but ALSO unknown: at least one OTHER category's
		// size didn't parse, so total only reflects the categories that DID
		// — it's a lower bound, not the true amount. Without this case the
		// preview printed a flat total with no caveat while the prompt below
		// (which also keys off `unknown`) asked to prune "an unknown
		// amount" — a preview/prompt mismatch caught in review.
		_, _ = fmt.Fprintf(out, "at least %s reclaimable from docker (one or more categories' size could not be parsed — see above)\n", formatBytes(total))
	default:
		_, _ = fmt.Fprintf(out, "%s reclaimable from docker\n", formatBytes(total))
	}

	if !apply {
		_, _ = fmt.Fprintln(out, "re-run with --apply to prune")
		return nil
	}

	promptSize := formatBytes(total)
	if unknown {
		promptSize = "an unknown amount (one or more categories' size could not be parsed — see above)"
	}
	ok, err := confirmFn(th, fmt.Sprintf("Prune %s from docker (containers/images/volumes/build cache)?", promptSize))
	if err != nil {
		return err
	}
	if !ok {
		return noteCancelled(out)
	}

	result := client.PruneDocker(ctx, items)
	failed := 0
	var totalReclaimed int64
	anyUnknown := false
	for _, item := range result {
		switch {
		case item.Skipped:
			// Already printed in the preview pass above.
		case item.Err != nil:
			// termsafe.Error, not Categorical, on purpose: see cleanFailureText.
			_, _ = fmt.Fprintf(out, "FAILED  %s: %s\n", item.Kind, cleanFailureText(item.Err))
			failed++
		case item.Applied && item.ReclaimedKnown:
			// Reclaimed is parsed from docker's OWN prune-command output,
			// never the pre-prune Reclaimable estimate.
			_, _ = fmt.Fprintf(out, "pruned %s (%s)\n", item.Kind, formatBytes(item.Reclaimed))
			totalReclaimed += item.Reclaimed
		case item.Applied:
			// The prune succeeded, but its output had no parseable "Total"
			// summary line (a docker version difference, an empty prune) —
			// say so rather than silently reporting a fabricated zero.
			_, _ = fmt.Fprintf(out, "pruned %s (reclaimed size unknown — no parseable total in docker's output)\n", item.Kind)
			anyUnknown = true
		}
	}
	msg := fmt.Sprintf("\nreclaimed %s from docker", formatBytes(totalReclaimed))
	if anyUnknown {
		msg += " (at least — one or more categories' reclaimed size could not be parsed)"
	}
	_, _ = fmt.Fprintln(out, msg)
	if failed > 0 {
		return fmt.Errorf("%d docker categories failed to prune", failed)
	}
	return nil
}

// printCacheItems prints the --caches dry-run report: one line per probed
// tool, grouped by whether it was detected. Uses cacheDisplayName (not the
// bare Kind) so the Cellar-impact disclosure appears in the scan preview
// too, not only the confirmation prompt — no fixed-width padding, since
// brew's disclosed label is deliberately longer than the others.
func printCacheItems(out io.Writer, items []cleanpkg.CacheItem) {
	for _, item := range items {
		if item.Skipped {
			_, _ = fmt.Fprintf(out, "skip  %s — %s\n", cacheDisplayName(item.Kind), item.SkipReason)
			continue
		}
		_, _ = fmt.Fprintf(out, "%s %s — %s\n", cacheDisplayName(item.Kind), termsafe.QuotePath(item.Path), formatBytes(item.Size))
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
			_, _ = fmt.Fprintf(out, "skip  %-11s — %s\n", item.Kind, cleanDiagnostic(item.SkipReason))
			continue
		}
		size := formatBytes(item.Reclaimable)
		if !item.ReclaimableKnown {
			// ReclaimableKnown is the single source of truth for "did this
			// row's size actually parse" (see docker.go) — a formatted
			// Reclaimable of 0 here is a parse-failure placeholder, not a
			// measurement, so show docker's own raw string instead of a
			// "0 B" that would read as a genuine zero.
			if item.Reported != "" {
				size = item.Reported
			} else {
				size = "unknown"
			}
		}
		// Reported is decoded from `docker system df` JSON, and the daemon
		// can be remote, so it is escaped and capped like the skip reason
		// above. It never reaches --json, which is dep/build-dir only.
		_, _ = fmt.Fprintf(out, "%-11s %s\n", item.Kind, termsafe.SafeLineMax(size, cleanDiagnosticMaxRunes))
	}
}

// cleanDiagnosticMaxRunes caps a subprocess or docker-daemon diagnostic in a
// clean row. exec keeps up to a 64 KiB stderr tail, and each FAILED row or
// docker skip line would otherwise print all of it as one escaped line
// (forgectl#867). Every rendering below stays at or under it, elision and
// dropped-bytes note included.
//
// The cap keeps both ends, not just the head. exec keeps the stderr TAIL on
// purpose: brew, npm and docker print their fatal line ("Error: ...") LAST,
// after any warnings. A head-only cut would drop exactly that line, and a
// few escaped newlines are enough to push it past the cap. The head gets
// cleanDiagnosticHeadRunes because it only has to name the command; the tail
// gets whatever the head leaves.
const (
	cleanDiagnosticMaxRunes  = 512
	cleanDiagnosticHeadRunes = 128
)

// cleanElision joins the kept head and tail of a capped diagnostic.
const cleanElision = " … [truncated] … "

// cleanFailureText renders a prune/clear failure for a FAILED row.
//
// It uses termsafe.Error, NOT termsafe.Categorical, although Categorical's own
// doc names subprocess stderr as its use case. Two things make the tool's text
// safe to keep here: exec's CommandError already runs stderr through
// redact.Text, and that text is the only diagnostic the operator gets for why
// brew/npm/docker refused — a fixed category string would leave them nothing
// to act on. Escaping bounds what it can do to the terminal, and the cap
// bounds its length. Do not "fix" this back to Categorical.
//
// Text that fits is termsafe.Error's rendering unchanged. Longer text is cut
// from the error's RAW text and only then escaped, so the cut can never
// split an escape sequence or a quoted path. A *exec.CommandError is cut
// from its fields (cleanCommandFailure); anything else gets the plain
// head+tail cut.
//
// The raw text is read BEFORE termsafe.Error runs. An Error method that works
// once and then panics gives its one good call to the raw text, and
// termsafe.Error then withholds its own rendering behind a short stand-in,
// which fits. Read in the other order, the good call went to the escaped
// rendering and the cut had only escaped text to work on, where it could
// split an escape in half (forgectl#871). The reverse, an Error method that
// panics once and then works, leaves only the escaped rendering; when that
// fits it is kept, and when it does not, cleanTextUnavailable stands in for
// it rather than a cut of escaped text.
func cleanFailureText(err error) string {
	raw, ok := rawErrorText(err)
	full := termsafe.Error(err).Error()
	if utf8.RuneCountInString(full) <= cleanDiagnosticMaxRunes {
		return full
	}
	if !ok {
		// The raw call panicked but the later one rendered, and too long to
		// keep: only the escaped text exists, and cutting it could split an
		// escape, so a fixed stand-in takes its place (forgectl#891).
		return cleanTextUnavailable
	}
	if text, ok := cleanCommandFailure(err, raw); ok {
		return text
	}
	return cleanDiagnostic(raw)
}

// cleanTextUnavailable stands in for an overlong failure whose raw text
// could not be read, the one case where only escaped text is left to cut.
const cleanTextUnavailable = "error text withheld: its Error method panicked, then gave text too long to show"

// rawErrorText is err.Error(), or ok=false when that method panics — the
// case termsafe.Error withholds the text for.
func rawErrorText(err error) (text string, ok bool) {
	defer func() {
		if recover() != nil {
			text, ok = "", false
		}
	}()
	return err.Error(), true
}

// cleanCommandFailure caps the text of a failure that carries a
// *exec.CommandError, whose rendering is "<command>: [<dropped note>] <stderr>".
// It works from the struct, never by searching the text: the dropped-bytes
// note is rebuilt from StderrDropped, so a look-alike note inside stderr (or
// a remote daemon's reply) is only ever stderr, cut like the rest of it. The
// command, and any text a wrapper put before it, gets the head budget, the
// note is kept whole, and stderr gets the tail. ok is false when raw does not
// end in the CommandError's stderr, so the caller falls back to the plain cut.
func cleanCommandFailure(err error, raw string) (string, bool) {
	var ce *exec.CommandError
	if !errors.As(err, &ce) || ce == nil || ce.Stderr == "" {
		return "", false
	}
	stderr := redact.Text(ce.Stderr)
	if !strings.HasSuffix(raw, stderr) {
		return "", false
	}
	command := strings.TrimSuffix(raw, stderr)
	note := ""
	if ce.StderrDropped > 0 {
		note = "[stderr truncated, " + strconv.FormatInt(ce.StderrDropped, 10) + " earlier bytes dropped] "
		trimmed, found := strings.CutSuffix(command, note)
		if !found {
			return "", false
		}
		command = trimmed
	}

	elisionN := utf8.RuneCountInString(cleanElision)
	head := termsafe.SafeLine(command)
	if utf8.RuneCountInString(head) > cleanDiagnosticHeadRunes {
		head, _ = takeHead(escapedPieces(command), cleanDiagnosticHeadRunes-elisionN)
		head += cleanElision
	}
	budget := cleanDiagnosticMaxRunes - utf8.RuneCountInString(head) - utf8.RuneCountInString(note)
	tail := termsafe.SafeLine(stderr)
	if utf8.RuneCountInString(tail) > budget {
		tail, _ = takeTail(escapedPieces(stderr), budget-elisionN)
		tail = cleanElision + tail
	}
	return head + note + tail, true
}

// cleanDiagnostic escapes raw for a clean row and, when the escaped text is
// over cleanDiagnosticMaxRunes, keeps up to cleanDiagnosticHeadRunes runes of
// head and fills the rest of the cap with tail, around cleanElision. The
// budget counts escaped output runes, elision included, and the cut falls
// only between whole escapes. Nothing in raw is interpreted: a look-alike of
// exec's dropped-bytes note is cut like any other text.
func cleanDiagnostic(raw string) string {
	escaped := termsafe.SafeLine(raw)
	if utf8.RuneCountInString(escaped) <= cleanDiagnosticMaxRunes {
		return escaped
	}
	pieces := escapedPieces(raw)
	head, headN := takeHead(pieces, cleanDiagnosticHeadRunes)
	budget := cleanDiagnosticMaxRunes - utf8.RuneCountInString(head) - utf8.RuneCountInString(cleanElision)
	tail, _ := takeTail(pieces[headN:], budget)
	return head + cleanElision + tail
}

// escapedPieces is s split into one SafeLine rendering per rune, so a cut
// between pieces never splits an escape.
func escapedPieces(s string) []string {
	pieces := make([]string, 0, len(s))
	for _, r := range s {
		pieces = append(pieces, termsafe.SafeLine(string(r)))
	}
	return pieces
}

// takeHead joins the leading pieces that fit in budget runes and reports how
// many it took.
func takeHead(pieces []string, budget int) (string, int) {
	var b strings.Builder
	used, n := 0, 0
	for _, p := range pieces {
		w := utf8.RuneCountInString(p)
		if used+w > budget {
			break
		}
		b.WriteString(p)
		used += w
		n++
	}
	return b.String(), n
}

// takeTail joins the trailing pieces that fit in budget runes and reports
// how many it took.
func takeTail(pieces []string, budget int) (string, int) {
	used, i := 0, len(pieces)
	for i > 0 {
		w := utf8.RuneCountInString(pieces[i-1])
		if used+w > budget {
			break
		}
		used += w
		i--
	}
	return strings.Join(pieces[i:], ""), len(pieces) - i
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

// anyDockerReclaimableUnknown reports whether any non-skipped item's
// preview size failed to parse (ReclaimableKnown false) — the signal that a
// summed total of 0 is NOT a genuine "nothing to reclaim" and must be
// reported as unknown instead (forgectl#165 item 6).
func anyDockerReclaimableUnknown(items []cleanpkg.DockerItem) bool {
	for _, item := range items {
		if !item.Skipped && !item.ReclaimableKnown {
			return true
		}
	}
	return false
}

// printCleanItems prints the dry-run report: one line per matched target,
// grouped by whether it would be reclaimed or was skipped. A target's path is
// named by whatever directories sit under the scanned root, so it is quoted
// on its way to the terminal (forgectl#855); --json keeps it raw.
func printCleanItems(out io.Writer, items []cleanpkg.Item) {
	if len(items) == 0 {
		_, _ = fmt.Fprintln(out, "no reclaimable directories found")
		return
	}
	for _, item := range items {
		if item.Skipped {
			_, _ = fmt.Fprintf(out, "skip  %s — %s\n", termsafe.QuotePath(item.Path), item.SkipReason)
			continue
		}
		_, _ = fmt.Fprintf(out, "%-8s %s — %s\n", item.Kind, termsafe.QuotePath(item.Path), formatBytes(item.Size))
	}
}

// cleanItemJSON is one target in `clean --json` (additive-only, ADR-0008).
type cleanItemJSON struct {
	Path       string `json:"path"`
	Kind       string `json:"kind"`
	SizeBytes  int64  `json:"size_bytes"`
	Skipped    bool   `json:"skipped"`
	SkipReason string `json:"skip_reason"`
}

type cleanReportJSON struct {
	Root                  string          `json:"root"`
	Items                 []cleanItemJSON `json:"items"`
	TotalReclaimableBytes int64           `json:"total_reclaimable_bytes"`
}

func newCleanReportJSON(root string, r cleanpkg.Result) cleanReportJSON {
	items := make([]cleanItemJSON, 0, len(r.Items))
	for _, it := range r.Items {
		items = append(items, cleanItemJSON{
			Path: it.Path, Kind: string(it.Kind), SizeBytes: it.Size,
			Skipped: it.Skipped, SkipReason: it.SkipReason,
		})
	}
	return cleanReportJSON{Root: root, Items: items, TotalReclaimableBytes: r.TotalReclaimable}
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
