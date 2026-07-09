package cli

import (
	"fmt"
	"io"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/pr"
)

// prSectionStyle titles a dashboard block. Bold accent (color 110) matches the
// launch-profile title style.
var prSectionStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("110"))

// newPrDashCmd builds `forgectl pr dash`.
func newPrDashCmd(client *pr.Client) *cobra.Command {
	// err discarded: "" degrades to an empty store on read (LoadReviewed).
	reviewedPath, _ := config.PrReviewedPath()
	return newPrDashCmdForClient(client, reviewedPath)
}

// newPrDashCmdForClient is the test seam (mirrors newNetCmdForClient).
func newPrDashCmdForClient(client *pr.Client, reviewedPath string) *cobra.Command {
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
			for _, n := range notes {
				fmt.Fprintln(cmd.ErrOrStderr(), "note: "+n)
			}

			store := pr.LoadReviewed(reviewedPath)
			out := cmd.OutOrStdout()
			errOut := cmd.ErrOrStderr()

			fmt.Fprintln(out, prSectionStyle.Render("active reviews"))
			renderSessions(out, dash.ActiveReviews)
			fmt.Fprintln(out)

			fmt.Fprintln(out, prSectionStyle.Render("awaiting your review"))
			if err := renderPRTable(out, errOut, dash.AwaitingYou, store); err != nil {
				return err
			}
			fmt.Fprintln(out)

			fmt.Fprintln(out, prSectionStyle.Render("your open PRs"))
			return renderPRTable(out, errOut, dash.YourOpen, store)
		},
	}
}

// renderSessions prints the local active-review breadcrumbs, or "(none)".
func renderSessions(out io.Writer, sessions []pr.Session) {
	if len(sessions) == 0 {
		fmt.Fprintln(out, "  (none)")
		return
	}
	for _, s := range sessions {
		age := time.Since(s.CreatedAt).Round(time.Second)
		fmt.Fprintf(out, "  %s  (%s ago)  %s\n", s.Ref.String(), age, s.Path)
	}
}
