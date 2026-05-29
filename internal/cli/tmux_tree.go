package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/tmux"
)

// newTmuxTreeCmd prints the session → window → pane tree. Icons are on unless
// --no-icons or NO_COLOR is set (M5 promotes this to a shared, persistent
// preference across the TUI and the other read verbs).
func newTmuxTreeCmd(client *tmux.Client) *cobra.Command {
	var noIcons bool
	cmd := &cobra.Command{
		Use:   "tree",
		Short: "Show the session → window → pane tree",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			icons := !noIcons && os.Getenv("NO_COLOR") == ""
			out, err := client.Tree(cmd.Context(), icons)
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), out)
			return nil
		},
	}
	cmd.Flags().BoolVar(&noIcons, "no-icons", false, "use ASCII markers instead of Nerd Font glyphs")
	return cmd
}
