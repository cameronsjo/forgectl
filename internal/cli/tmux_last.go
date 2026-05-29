package cli

import (
	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/tmux"
)

// newTmuxLastCmd jumps to the last-used session. The "-" shorthand is mapped to
// "last" by the argv normalizer before Cobra sees it.
func newTmuxLastCmd(client *tmux.Client) *cobra.Command {
	return &cobra.Command{
		Use:   "last",
		Short: "Jump to the last-used session",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return client.LastSession(cmd.Context())
		},
	}
}
