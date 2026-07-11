package cli

import (
	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/review"
)

// defaultReviewOwner is the --owner scope applied when the [review] section is
// absent or empty.
const defaultReviewOwner = "cameronsjo"

// newReviewCmd builds `forgectl review` — the cross-project work-inventory
// group. It builds its own source over a fresh runner (mirrors newPrCmd's
// self-contained client lifecycle).
func newReviewCmd(cfg config.Config) *cobra.Command {
	src := review.NewGitHub(exec.OSRunner{}, resolveReviewOwners(cfg))
	// err discarded: "" degrades to an empty store on read (LoadReviewed), and
	// the write verbs fail loudly via persist()'s path=="" guard.
	reviewedPath, _ := config.ReviewReviewedPath()
	return newReviewCmdForSource(src, reviewedPath)
}

// resolveReviewOwners applies the [review] owners config, falling back to the
// built-in default owner when the section is absent or empty. Split out of
// newReviewCmd so the one piece of wiring logic the test seam bypasses is
// itself unit-testable.
func resolveReviewOwners(cfg config.Config) []string {
	if len(cfg.Review.Owners) > 0 {
		return cfg.Review.Owners
	}
	return []string{defaultReviewOwner}
}

// newReviewCmdForSource builds the command tree over an explicit source and
// reviewed-store path — the test seam (mirrors newPrPrsCmdForClient).
func newReviewCmdForSource(src review.Source, reviewedPath string) *cobra.Command {
	var (
		asJSON bool
		kind   string
		repo   string
	)
	cmd := &cobra.Command{
		// The Use line's [--flag …] placeholders are load-bearing, not just help
		// text: the pre-Cobra menu router (shouldLaunchTUI → parentTakesArg)
		// reads them to learn this parent accepts tokens that are not subverbs.
		// Without them, a flag VALUE (`review --repo owner/name`) is mistaken
		// for an unknown subverb and routed to the TUI menu — a silent exit 1
		// in any non-TTY invocation.
		Use:   "review [--kind issue|pr] [--repo <owner/name>]",
		Short: "Cross-project work inventory: open issues and PRs across your repos",
		Long: `review lists every open issue and pull request across the configured
owners ([review] owners in config.toml; default cameronsjo) — the whole work
inventory, rendered live from gh. Nothing is copied or synced; the only local
state is the reviewed-marks file, and new activity on an item auto-un-dims it.

  forgectl review                       unified table (reviewed rows dimmed)
  forgectl review --json                machine-readable output
  forgectl review --kind issue          issues only (or: pr)
  forgectl review --repo owner/name     one repo only
  forgectl review mark owner/repo#42    mark an item reviewed
  forgectl review unmark owner/repo#42  clear an item's mark
  forgectl review sync                  prune marks for closed items`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runReviewList(cmd, src, reviewedPath, asJSON, kind, repo)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable JSON to stdout")
	cmd.Flags().StringVar(&kind, "kind", "", "filter by kind: issue or pr")
	cmd.Flags().StringVar(&repo, "repo", "", "filter to one owner/name repo")

	cmd.AddCommand(
		newReviewMarkCmd(reviewedPath),
		newReviewUnmarkCmd(reviewedPath),
		newReviewSyncCmd(src, reviewedPath),
	)
	return cmd
}
