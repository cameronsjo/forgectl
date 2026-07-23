package cli

import (
	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/projects"
)

// projectAliases is the single source of truth for projects' subverb
// shorthands — migrated here from forgive.ProjectAliases at conversion.
// Separate var for the same initialization-cycle reason as yAliases.
var projectAliases = map[string][]string{
	"pick":     {"p", "open"},
	"list":     {"l", "ls", "find"},
	"clone":    {"c"},
	"worktree": {"wt"},
	"pull-all": {"pull"},
}

// projectsModule declares the projects core module (ADR-0005): daily
// project-jumping verbs, "proj" group shorthand.
var projectsModule = module.Manifest{
	Name:         "projects",
	Tier:         module.TierCore,
	GroupAliases: []string{"proj"},
	SubAliases:   projectAliases,
	New: func(deps module.Deps) *cobra.Command {
		return newProjectsCmd(projects.New(deps.Runner))
	},
}

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
	cmd.AddCommand(newProjectsWorktreeCmd(client))
	cmd.AddCommand(newProjectsPullAllCmd(client))
	applyAliases(cmd, projectAliases)
	return cmd
}
