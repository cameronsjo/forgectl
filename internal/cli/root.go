// Package cli holds the thin Cobra verbs layered over the domain packages
// (internal/tmux, internal/projects, …). Commands parse flags and call ops;
// they hold no domain logic of their own. Command groups register through the
// module registry in modules.go (ADR-0005).
package cli

import (
	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/meta"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/projects"
	"github.com/cameronsjo/forgectl/internal/quarantine"
	"github.com/cameronsjo/forgectl/internal/tmux"
)

// newRoot builds the root command tree: registry-driven modules first
// (allModules), then the groups not yet converted to manifests, hand-wired.
// The hybrid is migration state — each conversion moves one AddCommand into
// the loop until only the loop remains.
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
		applyAliases(cmd, m.SubAliases)
		root.AddCommand(cmd)
	}

	// Hand-wired groups awaiting manifest conversion.
	root.AddCommand(newTmuxCmd(tmux.New(deps.Runner)))
	root.AddCommand(newProjectsCmd(projects.New(deps.Runner)))
	root.AddCommand(newConfigCmd(deps.Cfg))
	root.AddCommand(newLaunchCmd(deps.Cfg))
	root.AddCommand(newWorkflowCmd(deps.Cfg))
	root.AddCommand(newQuarantineCmd(quarantine.New(deps.Runner)))
	root.AddCommand(newPrCmd(deps.Cfg))
	root.AddCommand(newSessionsCmd(deps.Cfg))
	root.AddCommand(newReviewCmd(deps.Cfg))

	return root
}
