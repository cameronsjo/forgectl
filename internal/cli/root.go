// Package cli holds the thin Cobra verbs layered over internal/tmux. Commands
// parse flags and call ops; they hold no tmux logic of their own.
package cli

import (
	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/meta"
	"github.com/cameronsjo/forgectl/internal/tmux"
)

// NewRoot builds the root command tree. Each module registers its parent
// command here (tmux today; pr/k8s later).
func NewRoot() *cobra.Command {
	client := tmux.New(exec.OSRunner{})

	root := &cobra.Command{
		Use:     meta.AppName,
		Short:   meta.Tagline,
		Version: meta.Version,
		// fang renders styled errors/usage; we own when usage appears so an op
		// failure doesn't dump a wall of help. Bare-invoke → TUI is wired later.
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	// "did you mean" for fat-fingered verbs; forgiveness layer (M3) tightens this.
	root.SuggestionsMinimumDistance = 2

	root.AddCommand(newTmuxCmd(client))

	return root
}
