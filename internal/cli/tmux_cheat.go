package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/tui"
)

// newTmuxCheatCmd prints the tmux cheatsheet — terminology + the keybindings
// that matter — for a newcomer. The same content backs the TUI Cheatsheet
// screen.
func newTmuxCheatCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "cheat",
		Short: "tmux terminology + the keys that matter",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			noIcons, _ := cmd.Flags().GetBool("no-icons")
			fmt.Fprintln(cmd.OutOrStdout(), tui.Cheatsheet(noIcons))
			return nil
		},
	}
}
