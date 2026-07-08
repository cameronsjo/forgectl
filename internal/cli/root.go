// Package cli holds the thin Cobra verbs layered over internal/tmux and
// internal/projects. Commands parse flags and call ops; they hold no domain
// logic of their own.
package cli

import (
	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/meta"
	"github.com/cameronsjo/forgectl/internal/projects"
	"github.com/cameronsjo/forgectl/internal/quarantine"
	"github.com/cameronsjo/forgectl/internal/tmux"
)

// newRoot builds the root command tree. Each domain module registers its
// parent command here (tmux, projects today; pr/k8s later).
func newRoot(tmuxClient *tmux.Client, projClient *projects.Client, quarantineClient *quarantine.Client, cfg config.Config) *cobra.Command {
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

	root.AddCommand(newTmuxCmd(tmuxClient))
	root.AddCommand(newProjectsCmd(projClient))
	root.AddCommand(newConfigCmd(cfg))
	root.AddCommand(newLaunchCmd(cfg))
	root.AddCommand(newWorkflowCmd(cfg))
	root.AddCommand(newNetCmd(cfg))
	root.AddCommand(newQuarantineCmd(quarantineClient))

	return root
}
