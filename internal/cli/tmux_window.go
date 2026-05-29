package cli

import (
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/tmux"
)

// newTmuxWindowsCmd lists every window across all sessions, with its jump
// target. The TUI turns these into a one-keystroke cross-session jump (M5).
func newTmuxWindowsCmd(client *tmux.Client) *cobra.Command {
	return &cobra.Command{
		Use:   "windows",
		Short: "List windows across all sessions",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			windows, err := client.ListWindows(cmd.Context())
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(windows) == 0 {
				fmt.Fprintln(out, "no windows")
				return nil
			}
			w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			for _, win := range windows {
				marker := " "
				if win.Active {
					marker = "*"
				}
				unit := "panes"
				if win.Panes == 1 {
					unit = "pane"
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%d %s\n", marker, win.Target, win.Name, win.Panes, unit)
			}
			return w.Flush()
		},
	}
}
