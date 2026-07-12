package cli

import (
	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/forgive"
	"github.com/cameronsjo/forgectl/internal/projects"
)

// newProjectsCmd builds the `projects` parent command. The bare `forgectl projects`
// (or `forgectl proj`) invocation runs the interactive picker — same zero-typing
// affordance as `forgectl tmux`.
func newProjectsCmd(client *projects.Client) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "projects",
		Aliases: []string{"proj"},
		Short:   "Find and open projects across local, GitHub, and Gitea (clones on demand)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return newProjectsPickCmd(client).RunE(cmd, nil)
		},
	}
	cmd.AddCommand(newProjectsPickCmd(client))
	cmd.AddCommand(newProjectsListCmd(client))
	cmd.AddCommand(newProjectsCloneCmd(client))
	applyAliases(cmd, forgive.ProjectAliases)
	return cmd
}
