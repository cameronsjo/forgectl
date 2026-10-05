package cli

import (
	"errors"
	"fmt"
	"log/slog"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/termsafe"
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
					return fmt.Errorf("no such session: %q", oldName)
				}
				if errors.Is(err, tmux.ErrServerExited) {
					return noServerForSession("rename", oldName, err)
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

// noServerForSession is `tmux kill`/`tmux rename`'s answer when the server has
// exited and left its socket behind (forgectl#805). The strict resolve still
// refuses, which keeps the exit non-zero, but its "state could not be read"
// wording misstates what tmux said: there is no server, so there is no
// session to act on. Only the ErrServerExited sentinel is carried on — its
// text is the remedy — so errors.Is still sees it. %q, not %s: the name is
// operator-typed and reaches a terminal.
//
// The leftover socket's path rides along when resolveErr names one
// (forgectl#815): an operator running more than one server needs to know
// which socket the remedy is about.
func noServerForSession(verb, name string, resolveErr error) error {
	if socket, ok := tmux.ExitedSocketPath(resolveErr); ok {
		return fmt.Errorf("no tmux server is running on socket %s, so there is no session %q to %s: %w",
			termsafe.QuotePath(socket), name, verb, tmux.ErrServerExited)
	}
	return fmt.Errorf("no tmux server is running, so there is no session %q to %s: %w", name, verb, tmux.ErrServerExited)
}
