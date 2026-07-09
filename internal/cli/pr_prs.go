package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/pr"
)

// prDimStyle dims a whole reviewed row. Convention is a muted foreground, not
// .Faint() — mirrors internal/tui's styleMuted and launch_which's dim style.
// It is applied to a FULL tabwriter line AFTER flush, never a per-cell string
// before measurement (ANSI bytes inside a cell break column alignment).
var prDimStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))

// newPrPrsCmd builds `forgectl pr prs` over a freshly constructed client
// (mirrors newNetCmd). The reviewed-store path is resolved here so the command
// dims rows a prior review touched.
func newPrPrsCmd(client *pr.Client) *cobra.Command {
	// err discarded: "" degrades to an empty store on read (LoadReviewed).
	reviewedPath, _ := config.PrReviewedPath()
	return newPrPrsCmdForClient(client, reviewedPath)
}

// newPrPrsCmdForClient builds the command over an already-constructed client
// and an explicit reviewed-store path — the test seam (mirrors
// newNetCmdForClient) so a fake-wired *pr.Client and a temp store can be
// injected without touching the cfg-based constructor.
func newPrPrsCmdForClient(client *pr.Client, reviewedPath string) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "prs",
		Short: "List open PRs across your repos (authored, assigned, review-requested)",
		Long: `prs lists every open pull request you authored, are assigned, or have been
asked to review — a union of three gh searches. Rows you've already marked
reviewed are dimmed; new activity on the PR auto-un-dims them.

  forgectl pr prs          human table (REPO / # / TITLE / STATE)
  forgectl pr prs --json   machine-readable output for scripting`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			prs, notes, err := client.PRs(cmd.Context())
			if err != nil {
				return err
			}
			// Per-query degradation notes are diagnostics → stderr, never stdout.
			for _, n := range notes {
				fmt.Fprintln(cmd.ErrOrStderr(), "note: "+n)
			}

			store := pr.LoadReviewed(reviewedPath)
			if asJSON {
				return emitPRsJSON(cmd.OutOrStdout(), prs, store)
			}
			return renderPRTable(cmd.OutOrStdout(), cmd.ErrOrStderr(), prs, store)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable JSON to stdout")
	return cmd
}

// prRowJSON is the --json wire shape for one PR row, including the resolved
// reviewed verdict so a script needn't recompute it.
type prRowJSON struct {
	Ref       string    `json:"ref"`
	Repo      string    `json:"repo"`
	Number    int       `json:"number"`
	Title     string    `json:"title"`
	State     string    `json:"state"`
	Author    string    `json:"author"`
	IsDraft   bool      `json:"isDraft"`
	UpdatedAt time.Time `json:"updatedAt"`
	URL       string    `json:"url"`
	Reviewed  bool      `json:"reviewed"`
}

// emitPRsJSON writes the PR rows as an indented JSON array to out. An empty
// result emits [] (never null), matching the projects --json contract.
func emitPRsJSON(out io.Writer, prs []pr.PR, store *pr.ReviewedStore) error {
	rows := make([]prRowJSON, 0, len(prs))
	for _, p := range prs {
		rows = append(rows, prRowJSON{
			Ref:       p.Ref.String(),
			Repo:      p.Ref.Slug(),
			Number:    p.Ref.Number,
			Title:     p.Title,
			State:     p.State,
			Author:    p.Author,
			IsDraft:   p.IsDraft,
			UpdatedAt: p.UpdatedAt,
			URL:       p.URL,
			Reviewed:  pr.Dimmed(p, store),
		})
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(rows)
}

// renderPRTable writes a grep-friendly REPO/#/TITLE/STATE table to out and a
// one-line count summary to errOut. Dimmed (reviewed) rows are styled per whole
// line AFTER the tabwriter flush — laying the columns out in plain text first
// so ANSI escape bytes never enter the width measurement.
func renderPRTable(out, errOut io.Writer, prs []pr.PR, store *pr.ReviewedStore) error {
	var buf bytes.Buffer
	tw := tabwriter.NewWriter(&buf, 0, 2, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "REPO\t#\tTITLE\tSTATE"); err != nil {
		return err
	}
	for _, p := range prs {
		if _, err := fmt.Fprintf(tw, "%s\t%d\t%s\t%s\n",
			p.Ref.Slug(), p.Ref.Number, sanitizeCell(p.Title), prStateLabel(p)); err != nil {
			return err
		}
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	// lines[0] is the header; lines[i+1] aligns to prs[i].
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if _, err := fmt.Fprintln(out, lines[0]); err != nil {
		return err
	}
	reviewed := 0
	for i, p := range prs {
		line := lines[i+1]
		if pr.Dimmed(p, store) {
			reviewed++
			line = prDimStyle.Render(line)
		}
		if _, err := fmt.Fprintln(out, line); err != nil {
			return err
		}
	}
	fmt.Fprintf(errOut, "%d open PRs (%d reviewed)\n", len(prs), reviewed)
	return nil
}

// sanitizeCell strips tabs and newlines from a hostile gh-supplied string
// (a PR title) so a crafted value can't inject tabwriter columns or extra
// physical lines — either would break column alignment and, worse, the
// post-flush per-row dim indexing that assumes one line per PR.
func sanitizeCell(s string) string {
	return strings.NewReplacer("\t", " ", "\n", " ", "\r", " ").Replace(s)
}

// prStateLabel renders a PR's display state: "draft" for a draft, else the
// lowercased gh state ("open").
func prStateLabel(p pr.PR) string {
	if p.IsDraft {
		return "draft"
	}
	return strings.ToLower(p.State)
}
