package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/tmux"
)

// newTmuxKillCmd kills a session, confirming first unless --yes is given.
func newTmuxKillCmd(client *tmux.Client) *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:     "kill <session>",
		Aliases: []string{"k", "rm", "delete", "x"},
		Short:   "Kill a session",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if !client.HasSession(cmd.Context(), name) {
				return fmt.Errorf("no such session: %s", name)
			}
			out := cmd.OutOrStdout()
			if !yes {
				ok, err := confirm(fmt.Sprintf("Kill session %q?", name))
				if err != nil {
					return err
				}
				if !ok {
					fmt.Fprintln(out, "cancelled")
					return nil
				}
			}
			if err := client.KillSession(cmd.Context(), name); err != nil {
				return err
			}
			fmt.Fprintf(out, "killed %s\n", name)
			return nil
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "skip the confirmation prompt")
	return cmd
}
