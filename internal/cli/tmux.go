package cli

import (
	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/theme"
	"github.com/cameronsjo/forgectl/internal/tmux"
)

// tmuxAliases maps each canonical tmux verb to its aliases — the single
// source of truth, migrated here from forgive.TmuxAliases at conversion.
// internal/cli builds cobra Aliases from it, and the argv normalizer
// (dispatch.go) builds its Resolver from it for the unknown-verb → TUI
// fallthrough. "-" is argv-only (skipped by applyAliases). Separate var for
// the same initialization-cycle reason as yAliases.
var tmuxAliases = map[string][]string{
	"ls":      {"l", "list", "sessions"},
	"pick":    {"p", "go", "n", "new"},
	"kill":    {"k", "rm", "delete", "x"},
	"rename":  {"mv", "rn"},
	"windows": {"w"},
	"tree":    {"t"},
	"last":    {"-"},
	"cheat":   {"keys"},
}

// tmuxModule declares the tmux core module (ADR-0005) — the daily
// session-wrangling verbs. The only module with ArgvTokens: pre-Cobra argv
// forgiveness stays tmux-scoped until the flagged forgiveness-for-all
// follow-on.
var tmuxModule = module.Manifest{
	Name:         "tmux",
	Tier:         module.TierCore,
	GroupAliases: []string{"tm"},
	ArgvTokens:   []string{"tm"},
	SubAliases:   tmuxAliases,
	New: func(deps module.Deps) *cobra.Command {
		return newTmuxCmd(tmux.New(deps.Runner), deps.Theme)
	},
}

// newTmuxCmd builds the `tmux` parent command. Verbs are attached in their own
// files (tmux_ls.go, …) so each milestone adds a slice without churn here.
func newTmuxCmd(client *tmux.Client, th theme.Theme) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "tmux",
		Aliases: []string{"tm"},
		Short:   "Wrangle tmux sessions, windows, and panes",
		// A stray subverb (a typo like `frobnicate`) must not fall through to
		// RunE below and silently open the menu — Args rejects it with
		// cobra's own unknown-command error before RunE ever runs
		// (forgectl#479; TestGroupParentsRefuseStrayTokens pins this for
		// every group parent).
		Args: cobra.NoArgs,
		// `forgectl tmux` with no verb opens the tmux menu (the same TUI as a
		// bare invoke — tmux is the only module today).
		RunE: func(cmd *cobra.Command, _ []string) error {
			noIcons, _ := cmd.Flags().GetBool("no-icons")
			return runAction(cmd.Context(), client, noIcons, th)
		},
	}
	cmd.AddCommand(
		newTmuxLsCmd(client),
		newTmuxPickCmd(client),
		newTmuxKillCmd(client, th),
		newTmuxRenameCmd(client),
		newTmuxWindowsCmd(client),
		newTmuxTreeCmd(client),
		newTmuxLastCmd(client),
		newTmuxCheatCmd(th),
	)
	applyAliases(cmd, tmuxAliases)
	return cmd
}
