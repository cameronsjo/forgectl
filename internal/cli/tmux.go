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
	applyAliases(cmd)
	return cmd
}

// applyAliases sets each tmux subcommand's Cobra aliases from the forgive
// registry — the single source of truth. Tokens that aren't valid standalone
// Cobra command names (the "-" last-session shorthand) are skipped here; the
// argv normalizer in dispatch.go handles those before Cobra ever sees them.
func applyAliases(parent *cobra.Command) {
	for _, sub := range parent.Commands() {
		var valid []string
		for _, alias := range forgive.TmuxAliases[sub.Name()] {
			if alias == "-" || alias == sub.Name() {
				continue
			}
			valid = append(valid, alias)
		}
		if len(valid) > 0 {
			sub.Aliases = valid
		}
	}
}
