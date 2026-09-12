package cli

import (
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/pr"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// newPrQueueCmd builds `forgectl pr queue` — the read-only view of what
// `forgectl pr drain` will pick up next.
func newPrQueueCmd(client *pr.Client) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "queue",
		Short: "List reviews waiting for the drainer, oldest first",
		Long: `queue lists every session record in the queued phase — deferred by
'pr <ref> --queue' or by 'pr pick' past the concurrency cap — sorted oldest
first: the order 'forgectl pr drain' claims them in.

  forgectl pr queue          list what is waiting
  forgectl pr queue --json   the same, as JSON

A queued record has no workspace and no tmux window yet; 'forgectl pr drain
--once' is what starts it.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			summaries, unreadable, err := client.List(cmd.Context())
			if err != nil {
				return err
			}
			if unreadable > 0 {
				_, _ = fmt.Fprintln(cmd.ErrOrStderr(), unreadableRecordsNote(unreadable))
			}
			queued := queuedOldestFirst(summaries)
			out := cmd.OutOrStdout()
			if asJSON {
				return writePrQueueJSON(out, queued)
			}
			if len(queued) == 0 {
				_, _ = fmt.Fprintln(out, "no queued reviews")
				return nil
			}
			for _, s := range queued {
				_, _ = fmt.Fprintf(out, "%s\t%s\t%s\n",
					s.Ref().String(), s.CreatedAt().Format(time.RFC3339), termsafe.QuotePathIfUnsafe(s.Path()))
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, `emit [{"ref":...,"created_at":...,"path":...}] to stdout`)
	return cmd
}

// queuedOldestFirst filters summaries to the queued phase, FIFO by
// createdAt — the same order drain.go's claim step claims them in, so this
// view and the drainer never disagree about who is next.
func queuedOldestFirst(summaries []pr.SessionSummary) []pr.SessionSummary {
	var queued []pr.SessionSummary
	for _, s := range summaries {
		if s.Phase() == pr.PhaseQueued {
			queued = append(queued, s)
		}
	}
	sort.Slice(queued, func(i, j int) bool {
		return queued[i].CreatedAt().Before(queued[j].CreatedAt())
	})
	return queued
}

// prQueueRowJSON is the --json wire shape for one `pr queue` row.
type prQueueRowJSON struct {
	Ref       string `json:"ref"`
	CreatedAt string `json:"created_at"`
	Path      string `json:"path"`
}

// writePrQueueJSON encodes the queued rows as a JSON array, [] rather than
// null when empty — matching `pr list --json`'s empty-array contract.
func writePrQueueJSON(out io.Writer, queued []pr.SessionSummary) error {
	rows := make([]prQueueRowJSON, 0, len(queued))
	for _, s := range queued {
		rows = append(rows, prQueueRowJSON{
			Ref:       s.Ref().String(),
			CreatedAt: s.CreatedAt().Format(time.RFC3339),
			Path:      s.Path(),
		})
	}
	enc := termsafe.JSONEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(rows)
}
