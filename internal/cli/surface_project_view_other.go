//go:build !unix

package cli

import (
	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/privdir"
)

func newSurfaceProjectTimelineCmd(_ module.Deps, root *string) *cobra.Command {
	command := &cobra.Command{
		Use:  "timeline <project-id>",
		Args: cobra.ExactArgs(1),
		RunE: func(*cobra.Command, []string) error {
			return projectError(privdir.ErrUnsupported)
		},
	}
	command.Flags().String("thread", "", "show one exact logical thread; otherwise show the project")
	command.Flags().Bool("json", false, "print sanitized evidence and explicit partial/unknown states")
	command.Flags().Bool("live", false, "open Desk's read-only live evidence viewer (requires a terminal)")
	command.Flags().Bool("no-icons", false, "use the ASCII legend")
	return withProjectRoot(root, command)
}
