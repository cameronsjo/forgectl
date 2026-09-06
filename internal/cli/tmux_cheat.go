package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/theme"
	"github.com/cameronsjo/forgectl/internal/tui"
)

// newTmuxCheatCmd prints the tmux cheatsheet — terminology + the keybindings
// that matter — for a newcomer. The same content backs the TUI Cheatsheet
// screen.
func newTmuxCheatCmd(th theme.Theme) *cobra.Command {
	return &cobra.Command{
		Use:   "cheat",
		Short: "tmux terminology + the keys that matter",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			noIcons, _ := cmd.Flags().GetBool("no-icons")
			out := th.Writer(cmd.OutOrStdout(), os.Environ())
			_, _ = fmt.Fprintln(out, tui.Cheatsheet(noIcons, th.Styles()))
			return nil
		},
	}
}
