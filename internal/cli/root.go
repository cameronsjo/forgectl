// Package cli holds the thin Cobra verbs layered over the domain packages
// (internal/tmux, internal/projects, …). Commands parse flags and call ops;
// they hold no domain logic of their own. Command groups register through the
// module registry in modules.go (ADR-0005).
package cli

import (
	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/meta"
	"github.com/cameronsjo/forgectl/internal/module"
)

// newRoot builds the root command tree from the module registry
// (allModules) — every command group registers through its manifest
// (ADR-0005).
func newRoot(deps module.Deps) *cobra.Command {
	root := &cobra.Command{
		Use:     meta.AppName,
		Short:   meta.Tagline,
		Version: meta.Version,
		// fang renders styled errors/usage; we own when usage appears so an op
		// failure doesn't dump a wall of help. Bare-invoke → TUI is handled in
		// Execute, before Cobra runs.
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	// "did you mean" for fat-fingered verbs; the forgive layer handles the rest.
	root.SuggestionsMinimumDistance = 2
	// Honored by the TUI and the tree verb; swaps Nerd Font glyphs for ASCII.
	root.PersistentFlags().Bool("no-icons", false, "use ASCII markers instead of Nerd Font glyphs")

	for _, m := range allModules() {
		cmd := m.New(deps)
		// Append-if-absent: a constructor may already set its group alias in
		// its own literal (the ForClient test seams pin that surface), so the
		// manifest declaration must not duplicate it.
		for _, a := range m.GroupAliases {
			if !cmd.HasAlias(a) {
				cmd.Aliases = append(cmd.Aliases, a)
			}
		}
		// Deliberate re-application, not dead code: constructors with a
		// SubAliases surface also self-apply (their test seams need the
		// aliases), and applyAliases overwrites with the same map, so this
		// copy is the safety net for any constructor that doesn't.
		applyAliases(cmd, m.SubAliases)
		root.AddCommand(cmd)
	}

	return root
}
