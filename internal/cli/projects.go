package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/githubauth"
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
	ConfigKey:    "projects",
	GroupAliases: []string{"proj"},
	SubAliases:   projectAliases,
	New: func(deps module.Deps) *cobra.Command {
		// The [github] host gates the whole command tree: an invalid host or a
		// config file that failed to decode must fail LOUDLY here, not fall
		// back to github.com — a GHE deployment silently querying public
		// github.com is the exact misinventory forgectl#412 exists to prevent.
		if deps.Cfg.DecodeDegraded() {
			return newProjectsConfigErrorCmd(errors.New("config file failed to decode; refusing to guess the github host"))
		}
		host, err := githubauth.ResolveHost(deps.Cfg.Github.Host)
		if err != nil {
			// err is categorical by ResolveHost's contract — the value is
			// never rendered.
			return newProjectsConfigErrorCmd(fmt.Errorf("invalid [github] host: %w", err))
		}
		return newProjectsCmd(projects.New(deps.Runner,
			projects.WithGitHubOwners(deps.Cfg.Projects.Owners),
			projects.WithGitHubHost(host)))
	},
}

// newProjectsConfigErrorCmd builds a `projects` command tree whose every leaf
// fails immediately with err, before any inventory read or subprocess (mirrors
// newReviewConfigErrorCmd's structure; messages stay category-only). Aliases
// are applied so `forgectl proj ls` reports the config error too, rather than
// an unrelated "unknown command".
func newProjectsConfigErrorCmd(err error) *cobra.Command {
	fail := func(*cobra.Command, []string) error {
		return fmt.Errorf("projects: invalid config: %w", err)
	}
	// DisableFlagParsing + ArbitraryArgs on every node: the error tree
	// declares none of the real tree's flags, so without this a
	// `projects list --json` under a broken config would die on
	// `unknown flag: --json` instead of the config error — fail-closed
	// either way, but the stated cause must survive whatever argv the
	// caller sends.
	cmd := &cobra.Command{
		Use:                "projects",
		Aliases:            []string{"proj"},
		Short:              "Find and open projects across local, GitHub, and Gitea (clones on demand)",
		Args:               cobra.ArbitraryArgs,
		DisableFlagParsing: true,
		RunE:               fail,
	}
	cmd.AddCommand(
		&cobra.Command{Use: "pick", Args: cobra.ArbitraryArgs, DisableFlagParsing: true, RunE: fail},
		&cobra.Command{Use: "list [query]", Args: cobra.ArbitraryArgs, DisableFlagParsing: true, RunE: fail},
		&cobra.Command{Use: "clone <query>", Args: cobra.ArbitraryArgs, DisableFlagParsing: true, RunE: fail},
		&cobra.Command{Use: "worktree <query>", Args: cobra.ArbitraryArgs, DisableFlagParsing: true, RunE: fail},
		&cobra.Command{Use: "pull-all", Args: cobra.ArbitraryArgs, DisableFlagParsing: true, RunE: fail},
	)
	applyAliases(cmd, projectAliases)
	return cmd
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
