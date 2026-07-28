package cli

import (
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"

	ghosttypkg "github.com/cameronsjo/forgectl/internal/ghostty"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/tui"
)

// ghosttyModule declares the ghostty extension (ADR-0005): read-only theme
// and keybind reporting over Ghostty's only control surface, the
// `ghostty +<action>` CLI (forgectl#7 — Ghostty exposes no IPC/socket and no
// AppleScript). ConfigKey is deliberately empty: `--set` (the one write
// path) is out of scope for this pass, so there's nothing yet to persist —
// and this stays out of the `forgectl config` fold-in the issue floated.
var ghosttyModule = module.Manifest{
	Name:      "ghostty",
	Tier:      module.TierExtension,
	ConfigKey: "",
	New: func(deps module.Deps) *cobra.Command {
		return newGhosttyCmd(ghosttypkg.New(deps.Runner))
	},
}

// newGhosttyCmd builds the `ghostty` parent command over an already-
// constructed client — split out so tests can inject a fake-wired
// *ghostty.Client (mirrors newNetCmdForClient) without going through the
// module.Deps wiring.
func newGhosttyCmd(client *ghosttypkg.Client) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ghostty",
		Short: "Ghostty theme + keybind reporting",
		Long: `ghostty reports on the terminal Cameron lives in — themes and keybinds —
parsed live from the ghostty CLI rather than hard-coded (forgectl#7).

  forgectl ghostty themes        custom themes, active one marked
  forgectl ghostty themes --all  also list the themes bundled with ghostty
  forgectl ghostty cheat         keybind cheatsheet, parsed from +list-keybinds

Read-only: writing a theme choice back to the config (--set) is deferred.`,
	}
	cmd.AddCommand(
		newGhosttyThemesCmd(client),
		newGhosttyCheatCmd(client),
	)
	return cmd
}

// newGhosttyThemesCmd builds `ghostty themes`. Custom themes list first;
// --all appends the built-ins bundled with ghostty. The active theme (from
// +show-config) is marked with a leading "*", matching tmux ls's own
// attached-session marker.
func newGhosttyThemesCmd(client *ghosttypkg.Client) *cobra.Command {
	var all bool

	cmd := &cobra.Command{
		Use:   "themes",
		Short: "List ghostty themes — custom first, active one marked",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			themes, err := client.Themes(cmd.Context())
			if err != nil {
				return err
			}

			var custom, builtin []ghosttypkg.Theme
			for _, th := range themes {
				if th.Custom {
					custom = append(custom, th)
				} else {
					builtin = append(builtin, th)
				}
			}
			rows := custom
			if all {
				rows = append(rows, builtin...)
			}

			out := cmd.OutOrStdout()
			if len(rows) == 0 {
				fmt.Fprintln(out, "no ghostty themes found")
				return nil
			}
			w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			for _, th := range rows {
				marker := " "
				if th.Active {
					marker = "*"
				}
				fmt.Fprintf(w, "%s\t%s\n", marker, th.Name)
			}
			return w.Flush()
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "also list the built-in themes bundled with ghostty")
	return cmd
}

// newGhosttyCheatCmd builds `ghostty cheat`: the keybind cheatsheet, parsed
// live from `+list-keybinds` (forgectl#7's acceptance criterion — "not
// hard-coded"). Mirrors newTmuxCheatCmd's shape.
func newGhosttyCheatCmd(client *ghosttypkg.Client) *cobra.Command {
	return &cobra.Command{
		Use:   "cheat",
		Short: "ghostty keybind cheatsheet, parsed live from +list-keybinds",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			binds, err := client.Keybinds(cmd.Context())
			if err != nil {
				return err
			}
			noIcons, _ := cmd.Flags().GetBool("no-icons")
			fmt.Fprintln(cmd.OutOrStdout(), tui.KeybindSheet(binds, noIcons))
			return nil
		},
	}
}
