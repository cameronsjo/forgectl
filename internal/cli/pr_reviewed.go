package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/pr"
)

// newPrReviewedCmd builds `forgectl pr reviewed` — the parent for the
// reviewed-state store verbs (mark / unmark / sync).
func newPrReviewedCmd(client *pr.Client) *cobra.Command {
	// err discarded: "" makes read paths degrade to an empty store, and the
	// write verbs (mark/unmark/sync) fail loudly via persist()'s path=="" guard.
	reviewedPath, _ := config.PrReviewedPath()
	return newPrReviewedCmdForClient(client, reviewedPath)
}

// newPrReviewedCmdForClient is the test seam.
func newPrReviewedCmdForClient(client *pr.Client, reviewedPath string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "reviewed",
		Short: "Manage the local reviewed-state store that dims PRs you've looked at",
		Long: `reviewed manages the offline store that drives PR dimming. A mark stamps
the current time; a PR reads as reviewed until it sees new activity (a later
activity timestamp auto-un-dims it).

  forgectl pr reviewed mark owner/repo#42     mark a PR reviewed
  forgectl pr reviewed unmark owner/repo#42   clear a PR's mark
  forgectl pr reviewed sync                   prune marks for closed PRs`,
		Args: cobra.NoArgs,
	}
	cmd.AddCommand(
		newPrReviewedMarkCmd(client, reviewedPath),
		newPrReviewedUnmarkCmd(client, reviewedPath),
		newPrReviewedSyncCmd(client, reviewedPath),
	)
	return cmd
}

func newPrReviewedMarkCmd(client *pr.Client, reviewedPath string) *cobra.Command {
	return &cobra.Command{
		Use:   "mark <ref>",
		Short: "Mark a PR reviewed (dims it until the PR sees new activity)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref, err := client.ResolveRef(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			store := pr.LoadReviewed(reviewedPath)
			if err := store.Mark(ref); err != nil {
				return fmt.Errorf("mark %s reviewed: %w", ref.String(), err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "marked %s reviewed\n", ref.String())
			return nil
		},
	}
}

func newPrReviewedUnmarkCmd(client *pr.Client, reviewedPath string) *cobra.Command {
	return &cobra.Command{
		Use:   "unmark <ref>",
		Short: "Clear a PR's reviewed mark (un-dims it)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref, err := client.ResolveRef(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			store := pr.LoadReviewed(reviewedPath)
			if err := store.Unmark(ref); err != nil {
				return fmt.Errorf("unmark %s: %w", ref.String(), err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "unmarked %s\n", ref.String())
			return nil
		},
	}
}

func newPrReviewedSyncCmd(client *pr.Client, reviewedPath string) *cobra.Command {
	return &cobra.Command{
		Use:   "sync",
		Short: "Prune reviewed marks for PRs that are no longer open",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			prs, notes, err := client.PRs(cmd.Context())
			if err != nil {
				return err
			}
			for _, n := range notes {
				fmt.Fprintln(cmd.ErrOrStderr(), "note: "+n)
			}
			// A degraded query yields a PARTIAL open set — pruning against it
			// would drop reviewed marks for PRs that are actually still open.
			// Refuse to prune on partial data (a mark is only ever recreated by
			// re-reviewing, so a false prune costs real work).
			if len(notes) > 0 {
				fmt.Fprintln(cmd.ErrOrStderr(), "refusing to sync: open-PR set is partial (a query degraded); rerun when healthy")
				return nil
			}

			openRefs := make([]pr.Ref, len(prs))
			for i, p := range prs {
				openRefs[i] = p.Ref
			}
			// An empty open set almost never means "every marked PR closed at
			// once" — far likelier the queries simply returned nothing (you have
			// no open PRs right now). Pruning against it would wipe the WHOLE
			// store, so skip: a stale mark is cheap, a wiped store is not.
			if len(openRefs) == 0 {
				fmt.Fprintln(cmd.ErrOrStderr(), "no open PRs found across your queries; skipping prune to avoid wiping the store")
				return nil
			}
			store := pr.LoadReviewed(reviewedPath)
			if err := store.Sync(openRefs); err != nil {
				return fmt.Errorf("sync reviewed store: %w", err)
			}
			// "Open" here means open AND in one of your queries (authored /
			// assigned / review-requested) — a mark for a PR outside that union
			// is pruned as though closed.
			fmt.Fprintf(cmd.OutOrStdout(), "synced reviewed store against %d open PRs (authored/assigned/review-requested)\n", len(openRefs))
			return nil
		},
	}
}
