package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/pr"
	"github.com/cameronsjo/forgectl/internal/review"
)

func newReviewMarkCmd(reviewedPath string) *cobra.Command {
	return &cobra.Command{
		Use:   "mark <ref>",
		Short: "Mark a work item reviewed (dims it until it sees new activity)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			key, err := review.ParseWorkRef(args[0])
			if err != nil {
				return err
			}
			store := pr.LoadReviewed(reviewedPath)
			if err := store.MarkKey(key); err != nil {
				return fmt.Errorf("mark %s reviewed: %w", key, err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "marked %s reviewed\n", key)
			return nil
		},
	}
}

func newReviewUnmarkCmd(reviewedPath string) *cobra.Command {
	return &cobra.Command{
		Use:   "unmark <ref>",
		Short: "Clear a work item's reviewed mark (un-dims it)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			key, err := review.ParseWorkRef(args[0])
			if err != nil {
				return err
			}
			store := pr.LoadReviewed(reviewedPath)
			if err := store.UnmarkKey(key); err != nil {
				return fmt.Errorf("unmark %s: %w", key, err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "unmarked %s\n", key)
			return nil
		},
	}
}

func newReviewSyncCmd(src review.Source, reviewedPath string) *cobra.Command {
	return &cobra.Command{
		Use:   "sync",
		Short: "Prune reviewed marks for work items that are no longer open",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			items, notes, err := review.Aggregate(cmd.Context(), src)
			if err != nil {
				return err
			}
			for _, n := range notes {
				fmt.Fprintln(cmd.ErrOrStderr(), "note: "+n)
			}
			// A degraded query yields a PARTIAL open set — pruning against it
			// would drop reviewed marks for items that are actually still open.
			// Refuse to prune on partial data (a mark is only ever recreated by
			// re-reviewing, so a false prune costs real work). Truncation notes
			// count too: a truncated set is a partial set.
			if len(notes) > 0 {
				fmt.Fprintln(cmd.ErrOrStderr(), "refusing to sync: open set is partial (a query degraded or truncated); rerun when healthy")
				return nil
			}
			// An empty open set almost never means "everything closed at once" —
			// far likelier the queries returned nothing. Pruning against it would
			// wipe the WHOLE store, so skip: a stale mark is cheap, a wiped store
			// is not.
			if len(items) == 0 {
				fmt.Fprintln(cmd.ErrOrStderr(), "no open items found across your queries; skipping prune to avoid wiping the store")
				return nil
			}
			keys := make([]string, len(items))
			for i, it := range items {
				keys[i] = it.Key()
			}
			store := pr.LoadReviewed(reviewedPath)
			if err := store.SyncKeys(keys); err != nil {
				return fmt.Errorf("sync reviewed store: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "synced reviewed store against %d open items\n", len(keys))
			return nil
		},
	}
}
