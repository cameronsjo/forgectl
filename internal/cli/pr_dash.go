package cli

import (
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/pr"
	"github.com/cameronsjo/forgectl/internal/termsafe"
	"github.com/cameronsjo/forgectl/internal/theme"
)

// newPrDashCmd builds `forgectl pr dash`.
func newPrDashCmd(client *pr.Client, th theme.Theme) *cobra.Command {
	// err discarded: "" degrades to an empty store on read (LoadReviewed).
	reviewedPath, _ := config.PrReviewedPath()
	return newPrDashCmdForClient(client, reviewedPath, th)
}

// newPrDashCmdForClient is the test seam (mirrors newNetCmdForClient).
func newPrDashCmdForClient(client *pr.Client, reviewedPath string, th theme.Theme) *cobra.Command {
	return &cobra.Command{
		Use:   "dash",
		Short: "Dashboard: active reviews, PRs awaiting you, and your open PRs",
		Long: `dash shows three sections: the clean-room reviews you have in flight
locally, the open PRs whose review is requested of you, and your own open PRs.
Rows you've marked reviewed are dimmed (new activity auto-un-dims them).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dash, notes, err := client.Dash(cmd.Context())
			if err != nil {
				return err
			}
			renderDegradationNotes(cmd, notes)

			store := pr.LoadReviewed(reviewedPath)
			out := th.Writer(cmd.OutOrStdout(), os.Environ())
			errOut := cmd.ErrOrStderr()
			styles := th.Styles()

			_, _ = fmt.Fprintln(out, styles.Accent.Render("active reviews"))
			renderSessions(out, dash.ActiveReviews)
			_, _ = fmt.Fprintln(out)

			_, _ = fmt.Fprintln(out, styles.Accent.Render("awaiting your review"))
			if err := renderPRTable(out, errOut, dash.AwaitingYou, store, styles.Muted); err != nil {
				return err
			}
			_, _ = fmt.Fprintln(out)

			_, _ = fmt.Fprintln(out, styles.Accent.Render("your open PRs"))
			return renderPRTable(out, errOut, dash.YourOpen, store, styles.Muted)
		},
	}
}

// renderSessions prints the local active-review breadcrumbs, or "(none)".
// A record whose workspace has been deleted is shown with a trailing marker
// rather than hidden — the dashboard is where a user notices the leftover, and
// the row carries the breadcrumb path teardown takes.
//
// Only a LIVE row goes unmarked. Marking is a three-way decision on the same
// fail-closed enum `pr list` reads, not a missing/not-missing flag: an
// unclassified summary marked as nothing renders identically to a healthy
// review, which is exactly the claim it cannot make.
func renderSessions(out io.Writer, summaries []pr.SessionSummary) {
	if len(summaries) == 0 {
		_, _ = fmt.Fprintln(out, "  (none)")
		return
	}
	for _, s := range summaries {
		age := time.Since(s.CreatedAt()).Round(time.Second)
		suffix := ""
		switch {
		case s.IsWorkspaceNone():
			// queued or preparing: no workspace by design. The phase note below
			// carries it; "workspace missing" is the word for damage.
		case s.IsWorkspaceMissing():
			suffix = "  (" + workspaceMissingStatus + ")"
		case s.IsWorkspaceLive():
		default:
			suffix = "  (" + workspaceUnclassifiedStatus + ")"
		}
		// The path is a FILENAME chosen on disk, so it is the one field here
		// that can carry ANSI or bidi controls; Ref is charset-constrained by
		// ParseRef. Quote it, as every other human sink in the CLI does.
		_, _ = fmt.Fprintf(out, "  %s  (%s ago)  %s%s%s\n",
			s.Ref().String(), age, termsafe.QuotePath(s.Path()), suffix, phaseNote(s))
	}
}

// phaseNote annotates a dash row with what the record SAYS about itself.
// Active is the unmarked baseline and a legacy record has no phase to
// report, so those two are the only silent cases: anything else — including
// a phase a later build adds — renders by name rather than reading as a
// healthy review.
func phaseNote(s pr.SessionSummary) string {
	switch s.Phase() {
	case "", pr.PhaseActive:
		return ""
	case pr.PhaseNeedsRepair:
		reason := safeTerm(s.RepairReason())
		if reason == "" {
			reason = "no reason recorded"
		}
		return "  [needs-repair: " + reason + "]"
	default:
		return "  [" + string(s.Phase()) + "]"
	}
}
