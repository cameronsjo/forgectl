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
	applyProjectAliases(cmd)
	return cmd
}

// applyProjectAliases sets each projects subcommand's Cobra aliases from the
// forgive registry — mirrors applyAliases in tmux.go.
func applyProjectAliases(parent *cobra.Command) {
	for _, sub := range parent.Commands() {
		var valid []string
		for _, alias := range forgive.ProjectAliases[sub.Name()] {
			if alias == sub.Name() {
				continue
			}
			valid = append(valid, alias)
		}
		if len(valid) > 0 {
			sub.Aliases = valid
		}
	}
}
