package cli

import (
	"log/slog"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/review"
)

// defaultReviewOwner is the --owner scope applied when the [review] section is
// absent or empty.
const defaultReviewOwner = "cameronsjo"

// reviewModule declares the cross-project work-inventory extension
// (ADR-0005): owns the [review] config section, no alias surface.
var reviewModule = module.Manifest{
	Name:      "review",
	Tier:      module.TierExtension,
	ConfigKey: "review",
	New:       newReviewCmd,
}

// newReviewCmd builds `forgectl review` over the registry Deps — the gh
// source comes from deps.Runner (mirrors the other module constructors), and
// the Gitea source (Phase C) is appended only when [review.gitea] is enabled
// and configured with a host.
func newReviewCmd(deps module.Deps) *cobra.Command {
	srcs := []review.Source{review.NewGitHub(deps.Runner, resolveReviewOwners(deps.Cfg))}
	if src, ok := resolveGiteaSource(deps); ok {
		srcs = append(srcs, src)
	}
	// err discarded: "" degrades to an empty store on read (LoadReviewed), and
	// the write verbs fail loudly via persist()'s path=="" guard.
	reviewedPath, _ := config.ReviewReviewedPath()
	return newReviewCmdForSources(srcs, reviewedPath)
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

// resolveGiteaSource builds the Gitea review source from [review.gitea] when
// it is enabled and a host is configured; ok is false otherwise, and the
// caller omits the source entirely rather than constructing one doomed to
// error at Items() time. A malformed host (caught by review.NewGitea) also
// degrades to omission — logged loudly, since a config typo should not take
// down the whole `review` command.
func resolveGiteaSource(deps module.Deps) (src review.Source, ok bool) {
	gc := deps.Cfg.Review.Gitea
	if !gc.Enabled || gc.Host == "" {
		return nil, false
	}
	g, err := review.NewGitea(deps.Runner, gc.Host, gc.Login, gc.Owners)
	if err != nil {
		slog.Warn("Gitea review source misconfigured; omitting from review.", "host", gc.Host, "error", err)
		return nil, false
	}
	return g, true
}

// extraHosts collects the non-github hosts contributed by srcs — any source
// that exposes a Host() string (Gitea does) — for the allowlist mark/unmark
// pass to review.ParseWorkRefForHosts, so a host-qualified ref is only
// accepted for a host actually wired up as a review source.
func extraHosts(srcs []review.Source) []string {
	var hosts []string
	for _, src := range srcs {
		if h, ok := src.(interface{ Host() string }); ok {
			hosts = append(hosts, h.Host())
		}
	}
	return hosts
}

// newReviewCmdForSources builds the command tree over explicit sources and a
// reviewed-store path — the test seam (mirrors newPrPrsCmdForClient).
func newReviewCmdForSources(srcs []review.Source, reviewedPath string) *cobra.Command {
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

An optional second source, a self-hosted Gitea instance, joins the inventory
when [review.gitea] sets enabled = true and a host (enumerated over the tea
CLI). Its items use host-qualified refs: "host/owner/repo#N" or a Gitea
issue/pull URL, alongside the plain "owner/repo#N" github.com form.

  forgectl review                       unified table (reviewed rows dimmed)
  forgectl review --json                machine-readable output
  forgectl review --kind issue          issues only (or: pr)
  forgectl review --repo owner/name     one repo only
  forgectl review mark owner/repo#42    mark an item reviewed
  forgectl review unmark owner/repo#42  clear an item's mark
  forgectl review sync                  prune marks for closed items`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runReviewList(cmd, srcs, reviewedPath, asJSON, kind, repo)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable JSON to stdout")
	cmd.Flags().StringVar(&kind, "kind", "", "filter by kind: issue or pr")
	cmd.Flags().StringVar(&repo, "repo", "", "filter to one owner/name repo")

	hosts := extraHosts(srcs)
	cmd.AddCommand(
		newReviewMarkCmd(reviewedPath, hosts),
		newReviewUnmarkCmd(reviewedPath, hosts),
		newReviewSyncCmd(srcs, reviewedPath),
	)
	return cmd
}
