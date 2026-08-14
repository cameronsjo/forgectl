package cli

import (
	"errors"
	"fmt"
	"log/slog"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/tmux"
)

// newTmuxRenameCmd renames a session.
func newTmuxRenameCmd(client *tmux.Client) *cobra.Command {
	return &cobra.Command{
		Use:   "rename <old> <new>",
		Short: "Rename a session",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			oldName, newName := args[0], args[1]
			// oldName is resolved by exact equality; newName is a rename operand
			// and is never resolved at all. Conflating the two is how a rename
			// lands on a prefix sibling.
			session, err := client.ResolveSessionExact(cmd.Context(), oldName)
			if err != nil {
				if errors.Is(err, tmux.ErrSessionNotFound) {
					return fmt.Errorf("no such session: %s", oldName)
				}
				return err
			}
			slog.Debug("Preparing to rename session.", "from", oldName, "to", newName, "session_id", session.ID)
			if err := client.RenameSession(cmd.Context(), session, newName); err != nil {
				slog.Error("Failed to rename session.", "from", oldName, "to", newName, "error", err)
				return err
			}
			slog.Info("Successfully renamed session.", "from", oldName, "to", newName)
			fmt.Fprintf(cmd.OutOrStdout(), "renamed %s → %s\n", oldName, newName)
			return nil
		},
	}
}
