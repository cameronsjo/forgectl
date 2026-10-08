package cli

import (
	"errors"
	"fmt"
	"log/slog"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/termsafe"
	"github.com/cameronsjo/forgectl/internal/theme"
	"github.com/cameronsjo/forgectl/internal/tmux"
)

// newTmuxKillCmd kills a session, confirming first unless --yes is given. With
// --others it kills every session EXCEPT the named one.
func newTmuxKillCmd(client *tmux.Client, th theme.Theme) *cobra.Command {
	var yes, others bool
	cmd := &cobra.Command{
		Use:   "kill <session>",
		Short: "Kill a session (or, with --others, every session but it)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			// Resolved once, by exact name, up front — so the confirmation the
			// operator reads and the session the command kills are the same
			// object. The identity, not the name, is what the kill targets.
			session, err := client.ResolveSessionExact(cmd.Context(), name)
			if err != nil {
				if errors.Is(err, tmux.ErrSessionNotFound) {
					return fmt.Errorf("no such session: %q", name)
				}
				if errors.Is(err, tmux.ErrServerExited) {
					return noServerForSession("kill", name, err)
				}
				return err
			}
			out := cmd.OutOrStdout()
			prompt := fmt.Sprintf("Kill session %q?", name)
			if !yes {
				if others {
					all, err := client.ListSessions(cmd.Context())
					if err != nil {
						return err
					}
					var doomed []string
					for _, o := range all {
						if o.ID != session.ID {
							doomed = append(doomed, o.Name)
						}
					}
					if len(doomed) == 0 {
						_, _ = fmt.Fprintf(out, "no other sessions besides %q\n", name)
						return nil
					}
					prompt = termsafe.KillOthersPrompt(name, doomed)
				}
				ok, err := confirm(th, prompt)
				if err != nil {
					return err
				}
				if !ok {
					return noteCancelled(out)
				}
			}
			if others {
				slog.Debug("Preparing to kill others.", "keep", name, "session_id", session.ID)
				if err := client.KillOthers(cmd.Context(), session); err != nil {
					slog.Error("Failed to kill others.", "keep", name, "error", err)
					return err
				}
				slog.Info("Successfully killed others.", "keep", name)
				fmt.Fprintf(out, "killed all sessions except %s\n", name)
				return nil
			}
			slog.Debug("Preparing to kill session.", "session", name, "session_id", session.ID)
			if err := client.KillSession(cmd.Context(), session); err != nil {
				slog.Error("Failed to kill session.", "session", name, "error", err)
				return err
			}
			slog.Info("Successfully killed session.", "session", name)
			fmt.Fprintf(out, "killed %s\n", name)
			return nil
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "skip the confirmation prompt")
	cmd.Flags().BoolVar(&others, "others", false, "kill all sessions EXCEPT the named one")
	return cmd
}
