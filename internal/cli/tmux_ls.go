package cli

import (
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/termsafe"
	"github.com/cameronsjo/forgectl/internal/tmux"
)

// newTmuxLsCmd lists sessions as a plain aligned table. The colored,
// icon-aware rendering is the TUI's job (M5); this is the power-mode glance.
func newTmuxLsCmd(client *tmux.Client) *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "List tmux sessions",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			sessions, err := client.ListSessions(cmd.Context())
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(sessions) == 0 {
				fmt.Fprintln(out, "no tmux sessions")
				return nil
			}
			w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			for _, s := range sessions {
				marker := "○"
				if s.Attached {
					marker = "●"
				}
				unit := "windows"
				if s.Windows == 1 {
					unit = "window"
				}
				// Name and Path are tmux's, not forgectl's: whoever created the
				// session chose both, so each is untrusted text on its way to a
				// terminal. SafeLine leaves an ordinary name or path unchanged —
				// the table an operator reads every day is byte-identical — and
				// escapes in place anything that could repaint or reorder the
				// line. Deliberately not QuotePath: that wraps every value in
				// quotes, rewriting rows nobody asked it to touch.
				fmt.Fprintf(w, "%s\t%s\t%d %s\t%s\n",
					marker, termsafe.SafeLine(s.Name), s.Windows, unit, termsafe.SafeLine(s.Path))
			}
			return w.Flush()
		},
	}
}
