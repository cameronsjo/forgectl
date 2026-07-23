package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/projects"
)

// newProjectsWorktreeCmd initializes a bare-repo worktree layout for a project
// (absorbs git-smart-worktree), without opening it in tmux — the worktree
// sibling of `clone`:
//
//   - A URL or "owner/repo" first arg: worktree that exact target directly,
//     bypassing the inventory.
//   - Any other first arg: fuzzy match by name against the inventory; auto-pick
//     if unique, filtered selector if multiple, error if nothing matches.
//   - An optional second positional selects the branch to check out; omitted, the
//     remote's default branch is used.
//
// The new worktree dir is printed on stdout (the scriptable contract); progress
// annotations go to stderr, same split as `clone`.
func newProjectsWorktreeCmd(client *projects.Client) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "worktree <query | url | owner/repo> [branch]",
		Aliases: []string{"wt"},
		Short:   "Initialize a bare-repo worktree layout for a project",
		Args:    cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			var branch string
			if len(args) == 2 {
				branch = args[1]
			}

			if r, ok := projects.ParseCloneTarget(args[0]); ok {
				return worktreeOnly(ctx, client, cmd, r, branch)
			}

			all, notes, err := client.Inventory(ctx)
			if err != nil {
				return err
			}
			for _, n := range notes {
				fmt.Fprintln(cmd.ErrOrStderr(), "note: "+n)
			}
			if len(all) == 0 {
				return fmt.Errorf("no projects found across local, GitHub, or Gitea")
			}

			query := args[0]
			candidates := filterRepos(all, "", query)
			if len(candidates) == 0 {
				return fmt.Errorf("no project matching %q across local, GitHub, or Gitea", query)
			}
			if len(candidates) == 1 {
				return worktreeOnly(ctx, client, cmd, candidates[0], branch)
			}

			chosen, err := pickRepo(candidates)
			if err != nil {
				return err
			}
			return worktreeOnly(ctx, client, cmd, chosen, branch)
		},
	}
	return cmd
}

// worktreeOnly initializes the bare-repo worktree layout for r and prints the
// new worktree dir on stdout (the scriptable contract); the progress line goes
// to stderr so a `$(forgectl proj worktree …)` capture stays clean.
func worktreeOnly(ctx context.Context, client *projects.Client, cmd *cobra.Command, r projects.Repo, branch string) error {
	fmt.Fprintf(cmd.ErrOrStderr(), "Initializing worktree for %s/%s from %s…\n", r.Owner, r.Name, r.Host)
	dir, err := client.Worktree(ctx, r, branch)
	if err != nil {
		return err
	}
	fmt.Fprintln(cmd.OutOrStdout(), dir)
	return nil
}
