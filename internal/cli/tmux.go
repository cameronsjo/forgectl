package cli

import (
	"errors"

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

// errTmuxMenuNeedsTerminal is what bare `forgectl tmux` says off a terminal:
// the menu is a full-screen TUI, and its plain forms are the verbs below it.
// Without this check Bubble Tea writes alt-screen sequences into a pipe, or
// fails with a raw /dev/tty error when there is no terminal at all
// (forgectl#1100).
var errTmuxMenuNeedsTerminal = errors.New("the tmux menu needs a terminal on stdin and stdout; use tmux ls, tmux pick, or tmux tree for plain output")

// newTmuxCmd builds the `tmux` parent command. Verbs are attached in their own
// files (tmux_ls.go, …) so each milestone adds a slice without churn here.
func newTmuxCmd(deps module.Deps, client *tmux.Client) *cobra.Command {
	return newTmuxCmdWith(deps, client, tui.Run)
}

// newTmuxCmdWith is newTmuxCmd with the hub runner supplied, so the hand-off
// of a verb chosen in the hub can be tested without a terminal.
func newTmuxCmdWith(deps module.Deps, client *tmux.Client, run hubRunner) *cobra.Command {
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
			if !isInteractiveTTY() {
				return errTmuxMenuNeedsTerminal
			}
			noIcons, _ := cmd.Flags().GetBool("no-icons")
			opts := hubRunOptions(cmd.Context(), deps, cmd.Root(), client)
			opts.StartInTmux = true
			opts.NoIcons = noIcons
			opts.Theme = th
			// A verb chosen in the hub is deferred, not run here: this RunE
			// is inside fang, and the verb's own dispatch is a second fang
			// frame (forgectl#1000).
			return runActionWith(cmd.Context(), client, opts, run, func(argv []string) error {
				return deferHubVerb(cmd, th, argv)
			})
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
