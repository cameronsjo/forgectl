// Package cli holds the thin Cobra verbs layered over internal/tmux. Commands
// parse flags and call ops; they hold no tmux logic of their own.
package cli

import (
	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/meta"
	"github.com/cameronsjo/forgectl/internal/tmux"
)

// newRoot builds the root command tree over the given client. Each module
// registers its parent command here (tmux today; pr/k8s later).
func newRoot(client *tmux.Client) *cobra.Command {
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

	root.AddCommand(newTmuxCmd(client))

	return root
}
