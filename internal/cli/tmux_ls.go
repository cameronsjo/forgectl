package cli

import (
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/tmux"
)

// newTmuxLsCmd lists sessions as a plain aligned table. The colored,
// icon-aware rendering is the TUI's job (M5); this is the power-mode glance.
func newTmuxLsCmd(client *tmux.Client) *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "List tmux sessions",
		Args:    cobra.NoArgs,
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
				fmt.Fprintf(w, "%s\t%s\t%d %s\t%s\n", marker, s.Name, s.Windows, unit, s.Path)
			}
			return w.Flush()
		},
	}
}
