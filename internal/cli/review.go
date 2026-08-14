package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/review"
)

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
// and configured with a host. A configured-but-invalid [review.gitea]
// section fails the WHOLE command tree (see newReviewConfigErrorCmd) rather
// than silently narrowing to GitHub-only — module.Manifest.New has no error
// return, so this is the seam that surfaces the config error to the user.
func newReviewCmd(deps module.Deps) *cobra.Command {
	srcs := []review.Source{review.NewGitHub(deps.Runner, deps.Cfg.Review.Owners)}
	giteaSrc, ok, err := resolveGiteaSource(deps)
	if err != nil {
		return newReviewConfigErrorCmd(err)
	}
	if ok {
		srcs = append(srcs, giteaSrc)
	}
	// err discarded: "" degrades to an empty store on read (LoadReviewed), and
	// the write verbs fail loudly via persist()'s path=="" guard.
	reviewedPath, _ := config.ReviewReviewedPath()
	return newReviewCmdForSources(srcs, reviewedPath)
}

// newReviewConfigErrorCmd builds a `review` command tree whose every leaf —
// the bare command, mark, unmark, sync — fails immediately with err, before
// touching the reviewed store or shelling out. Used when [review.gitea] is
// configured (non-zero) but invalid: silently omitting the source (the prior
// behavior) let a host typo pass unnoticed, and — worse — let review sync's
// host-scoped prune leave every git.sjo.lol mark stranded with no active
// source to protect it. A config error must be loud, not a warn-and-omit.
func newReviewConfigErrorCmd(err error) *cobra.Command {
	fail := func(*cobra.Command, []string) error {
		return fmt.Errorf("review: invalid [review.gitea] config: %w", err)
	}
	cmd := &cobra.Command{
		Use:   "review [--kind issue|pr] [--repo <owner/name>]",
		Short: "Cross-project work inventory: open issues and PRs across your repos",
		Args:  cobra.NoArgs,
		RunE:  fail,
	}
	cmd.AddCommand(
		&cobra.Command{Use: "mark <ref>", Args: cobra.ExactArgs(1), RunE: fail},
		&cobra.Command{Use: "unmark <ref>", Args: cobra.ExactArgs(1), RunE: fail},
		&cobra.Command{Use: "sync", Args: cobra.NoArgs, RunE: fail},
	)
	return cmd
}

// resolveGiteaSource builds the Gitea review source from [review.gitea].
// Three outcomes:
//   - disabled (Enabled == false, whatever else is set) → (nil, false, nil):
//     the caller silently omits the source — this covers both a genuinely
//     absent/empty section and a deliberately-off one, matching the pre-Gitea
//     behavior exactly.
//   - Enabled == true but Host is empty, or Host fails review.NewGitea's
//     charset validation → (nil, false, err): an ERROR, not a warn-and-omit.
//     A config typo must not silently narrow the review inventory — and,
//     because review sync's prune is host-scoped, a silently-omitted host
//     would leave every mark for that host permanently stranded (never
//     eligible for pruning, but never re-verified as open either) with no
//     visible signal that anything was wrong.
//   - Enabled == true, Host valid → (src, true, nil).
func resolveGiteaSource(deps module.Deps) (src review.Source, ok bool, err error) {
	gc := deps.Cfg.Review.Gitea
	if !gc.Enabled {
		return nil, false, nil
	}
	if gc.Host == "" {
		return nil, false, fmt.Errorf("[review.gitea] enabled = true but host is empty")
	}
	g, err := review.NewGitea(deps.Runner, gc.Host, gc.Login, gc.Owners)
	if err != nil {
		return nil, false, err
	}
	return g, true, nil
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

// activeHosts is extraHosts plus github.com — github.com is always active
// because newReviewCmd always includes a GitHub source unconditionally.
// review sync uses this as the host-scoping allowlist for SyncKeysScoped: a
// host outside this set has no active source THIS run, so its marks are
// never eligible for pruning (see newReviewSyncCmd).
func activeHosts(srcs []review.Source) []string {
	return append([]string{review.GitHubHost}, extraHosts(srcs)...)
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
owners — the whole work inventory, rendered live from gh. Nothing is copied or
synced; the only local state is the reviewed-marks file, and new activity on an
item auto-un-dims it.

The owners come from [review] owners in config.toml. Leave that list unset (or
empty) and review enumerates whoever gh is authenticated as on GitHub.com; the
list is independent of [projects] owners and neither inherits from the other.
Every GitHub query is pinned to github.com — GitHub Enterprise is not
supported, and an ambient GH_HOST is overridden rather than queried.

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
