package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/tmux"
)

// newTmuxKillCmd kills a session, confirming first unless --yes is given. With
// --others it kills every session EXCEPT the named one.
func newTmuxKillCmd(client *tmux.Client) *cobra.Command {
	var yes, others bool
	cmd := &cobra.Command{
		Use:   "kill <session>",
		Short: "Kill a session (or, with --others, every session but it)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if !client.HasSession(cmd.Context(), name) {
				return fmt.Errorf("no such session: %s", name)
			}
			out := cmd.OutOrStdout()
			prompt := fmt.Sprintf("Kill session %q?", name)
			if others {
				prompt = fmt.Sprintf("Kill ALL sessions except %q?", name)
			}
			if !yes {
				ok, err := confirm(prompt)
				if err != nil {
					return err
				}
				if !ok {
					fmt.Fprintln(out, "cancelled")
					return nil
				}
			}
			if others {
				if err := client.KillOthers(cmd.Context(), name); err != nil {
					return err
				}
				fmt.Fprintf(out, "killed all sessions except %s\n", name)
				return nil
			}
			if err := client.KillSession(cmd.Context(), name); err != nil {
				return err
			}
			fmt.Fprintf(out, "killed %s\n", name)
			return nil
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "skip the confirmation prompt")
	cmd.Flags().BoolVar(&others, "others", false, "kill all sessions EXCEPT the named one")
	return cmd
}
