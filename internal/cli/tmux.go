package cli

import (
	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/forgive"
	"github.com/cameronsjo/forgectl/internal/tmux"
)

// newTmuxCmd builds the `tmux` parent command. Verbs are attached in their own
// files (tmux_ls.go, …) so each milestone adds a slice without churn here.
func newTmuxCmd(client *tmux.Client) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "tmux",
		Aliases: []string{"tm"},
		Short:   "Wrangle tmux sessions, windows, and panes",
		// `forgectl tmux` with no verb opens the tmux menu (the same TUI as a
		// bare invoke — tmux is the only module today).
		RunE: func(cmd *cobra.Command, _ []string) error {
			noIcons, _ := cmd.Flags().GetBool("no-icons")
			return runAction(cmd.Context(), client, noIcons)
		},
	}
	cmd.AddCommand(
		newTmuxLsCmd(client),
		newTmuxPickCmd(client),
		newTmuxKillCmd(client),
		newTmuxRenameCmd(client),
		newTmuxWindowsCmd(client),
		newTmuxTreeCmd(client),
		newTmuxLastCmd(client),
		newTmuxCheatCmd(),
	)
	applyAliases(cmd, forgive.TmuxAliases)
	return cmd
}
