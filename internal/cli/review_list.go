package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/pr"
	"github.com/cameronsjo/forgectl/internal/review"
)

// runReviewList is the bare `forgectl review` body: aggregate, filter, render.
func runReviewList(cmd *cobra.Command, src review.Source, reviewedPath string, asJSON bool, kind, repo string) error {
	switch kind {
	case "", string(review.KindIssue), string(review.KindPR):
	default:
		return fmt.Errorf("invalid --kind %q (want issue or pr)", kind)
	}

	items, notes, err := review.Aggregate(cmd.Context(), src)
	if err != nil {
		return err
	}
	// Per-query degradation notes are diagnostics → stderr, never stdout.
	for _, n := range notes {
		fmt.Fprintln(cmd.ErrOrStderr(), "note: "+n)
	}

	items = filterItems(items, kind, repo)
	// An unresolvable store path degrades reads to an empty store (house
	// pattern), but for THIS view that silently renders every item as
	// unreviewed — say so instead of misreporting.
	if reviewedPath == "" {
		fmt.Fprintln(cmd.ErrOrStderr(), "note: reviewed-store path unavailable; reviewed state not shown")
	}
	store := pr.LoadReviewed(reviewedPath)
	if asJSON {
		return emitReviewJSON(cmd.OutOrStdout(), items, store)
	}
	return renderReviewTable(cmd.OutOrStdout(), cmd.ErrOrStderr(), items, store)
}

// filterItems applies the --kind/--repo filters. An empty filter passes all.
func filterItems(items []review.Item, kind, repo string) []review.Item {
	if kind == "" && repo == "" {
		return items
	}
	out := make([]review.Item, 0, len(items))
	for _, it := range items {
		if kind != "" && string(it.Kind) != kind {
			continue
		}
		if repo != "" && it.Slug() != repo {
			continue
		}
		out = append(out, it)
	}
	return out
}

// reviewRowJSON is the --json wire shape for one inventory row, including the
// resolved reviewed verdict so a script needn't recompute it.
type reviewRowJSON struct {
	Key       string    `json:"key"`
	Kind      string    `json:"kind"`
	Repo      string    `json:"repo"`
	Number    int       `json:"number"`
	Title     string    `json:"title"`
	State     string    `json:"state"`
	IsDraft   bool      `json:"isDraft"`
	Labels    []string  `json:"labels"`
	UpdatedAt time.Time `json:"updatedAt"`
	URL       string    `json:"url"`
	Reviewed  bool      `json:"reviewed"`
}

// emitReviewJSON writes the rows as an indented JSON array to out. An empty
// result emits [] (never null), and each row's labels field is [] never null —
// matching the projects/prs --json contract.
func emitReviewJSON(out io.Writer, items []review.Item, store *pr.ReviewedStore) error {
	rows := make([]reviewRowJSON, 0, len(items))
	for _, it := range items {
		labels := it.Labels
		if labels == nil {
			labels = []string{}
		}
		rows = append(rows, reviewRowJSON{
			Key:       it.Key(),
			Kind:      string(it.Kind),
			Repo:      it.Slug(),
			Number:    it.Number,
			Title:     it.Title,
			State:     it.State,
			IsDraft:   it.IsDraft,
			Labels:    labels,
			UpdatedAt: it.UpdatedAt,
			URL:       it.URL,
			Reviewed:  store.IsReviewedKey(it.Key(), it.UpdatedAt),
		})
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(rows)
}

// renderReviewTable writes the KIND/REPO/#/TITLE/LABELS/STATE table to out and
// a one-line count summary to errOut. Dimmed (reviewed) rows are styled per
// whole line AFTER the tabwriter flush — plain text lays the columns out
// first, so ANSI escape bytes never enter the width measurement.
func renderReviewTable(out, errOut io.Writer, items []review.Item, store *pr.ReviewedStore) error {
	var buf bytes.Buffer
	tw := tabwriter.NewWriter(&buf, 0, 2, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "KIND\tREPO\t#\tTITLE\tLABELS\tSTATE"); err != nil {
		return err
	}
	for _, it := range items {
		if _, err := fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\t%s\n",
			it.Kind, it.Slug(), it.Number,
			sanitizeCell(it.Title),
			sanitizeCell(strings.Join(it.Labels, ",")),
			reviewStateLabel(it)); err != nil {
			return err
		}
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	// lines[0] is the header; lines[i+1] aligns to items[i].
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if _, err := fmt.Fprintln(out, lines[0]); err != nil {
		return err
	}
	reviewed := 0
	for i, it := range items {
		line := lines[i+1]
		if store.IsReviewedKey(it.Key(), it.UpdatedAt) {
			reviewed++
			line = prDimStyle.Render(line)
		}
		if _, err := fmt.Fprintln(out, line); err != nil {
			return err
		}
	}
	fmt.Fprintf(errOut, "%d open items (%d reviewed)\n", len(items), reviewed)
	return nil
}

// reviewStateLabel renders an item's display state: "draft" for a draft PR,
// else the lowercased tracker state ("open").
func reviewStateLabel(it review.Item) string {
	if it.IsDraft {
		return "draft"
	}
	return strings.ToLower(it.State)
}
