package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/tmux"
)

// newTmuxPickCmd connects to (or smart-creates) a session via sesh. With a
// name it connects directly; with no name it prints the candidate list — the
// TUI picker (M5) is the zero-typing no-arg experience.
func newTmuxPickCmd(client *tmux.Client) *cobra.Command {
	return &cobra.Command{
		Use:   "pick [name]",
		Short: "Connect to or smart-create a session (via sesh)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				return client.Pick(cmd.Context(), args[0])
			}
			names, err := client.SeshList(cmd.Context())
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(names) == 0 {
				fmt.Fprintln(out, "no sesh candidates")
				return nil
			}
			for _, n := range names {
				fmt.Fprintln(out, n)
			}
			return nil
		},
	}
}
