package cli

import (
	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/tmux"
	"github.com/cameronsjo/forgectl/internal/tui"
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
		return newTmuxCmd(deps, tmux.New(deps.Runner))
	},
}

// newTmuxCmd builds the `tmux` parent command. Verbs are attached in their own
// files (tmux_ls.go, …) so each milestone adds a slice without churn here.
func newTmuxCmd(deps module.Deps, client *tmux.Client) *cobra.Command {
	th := deps.Theme
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
		// `forgectl tmux` with no verb opens the tmux jumper directly (the
		// hub row's behavior for tmux) — StartInTmux skips the hub screen,
		// but the hub is still one esc away, so it's built from cmd.Root()
		// (resolved at run time, once the whole tree exists) rather than
		// threaded through construction.
		RunE: func(cmd *cobra.Command, _ []string) error {
			noIcons, _ := cmd.Flags().GetBool("no-icons")
			opts := tui.RunOptions{
				Hub:         buildHub(cmd.Root(), configFilePresent()),
				StartInTmux: true,
				NoIcons:     noIcons,
				Theme:       th,
			}
			return runAction(cmd.Context(), deps, cmd.Root(), client, opts)
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
