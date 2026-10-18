//go:build !unix

package cli

import (
	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/privdir"
)

func newSurfaceProjectAgentsCmd(_ module.Deps, root *string) *cobra.Command {
	command := &cobra.Command{
		Use:   "agents <project-id>",
		Short: "Show bounded per-thread native activity without launching or resuming agents",
		Args:  cobra.ExactArgs(1),
		RunE: func(*cobra.Command, []string) error {
			return projectError(privdir.ErrUnsupported)
		},
	}
	command.Flags().Bool("json", false, "print one bounded Agents snapshot as JSON")
	command.Flags().Bool("live", false, "open the terminal-native Agents overview")
	command.Flags().Bool("no-icons", false, "use the ASCII legend")
	command.Flags().String("herdr-session", "", "explicit named herdr session for live focus and evidence actions")
	command.Flags().String("herdr-binary", "", "absolute operator-selected herdr executable for live focus and evidence actions")
	return withProjectRoot(root, command)
}
