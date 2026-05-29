package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/tmux"
)

// newTmuxRenameCmd renames a session.
func newTmuxRenameCmd(client *tmux.Client) *cobra.Command {
	return &cobra.Command{
		Use:   "rename <old> <new>",
		Short: "Rename a session",
		Args:    cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			oldName, newName := args[0], args[1]
			if !client.HasSession(cmd.Context(), oldName) {
				return fmt.Errorf("no such session: %s", oldName)
			}
			if err := client.RenameSession(cmd.Context(), oldName, newName); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "renamed %s → %s\n", oldName, newName)
			return nil
		},
	}
}
