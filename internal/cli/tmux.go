package cli

import (
	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/tmux"
)

// newTmuxCmd builds the `tmux` parent command. Verbs are attached in their own
// files (tmux_ls.go, …) so each milestone adds a slice without churn here.
func newTmuxCmd(client *tmux.Client) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "tmux",
		Aliases: []string{"tm"},
		Short:   "Wrangle tmux sessions, windows, and panes",
	}
	cmd.AddCommand(newTmuxLsCmd(client))
	return cmd
}
