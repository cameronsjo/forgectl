package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/projects"
)

// newProjectsCloneCmd clones a repo into the canonical {host}/{org}/{repo}
// layout, without opening it in tmux — the non-interactive sibling of `pick`:
//
//   - No args: huh.NewSelect over the whole cross-host inventory (same picker
//     as `pick`).
//   - A URL or "owner/repo" arg: clone that exact target directly, bypassing
//     the inventory (absorbs git-smart-clone).
//   - Any other arg: fuzzy match by name against the inventory; auto-clone if
//     unique, filtered selector if multiple, error if nothing matches.
//   - --org <login>: bulk-clone every repo GitHub lists for that user/org
//     (absorbs gh-clone-org), one dest per stdout line.
//
// A candidate already on disk (canonical or legacy flat layout — Discover
// finds both) is annotated rather than re-cloned, so `clone` is safe to run
// repeatedly to "make sure everything's checked out".
func newProjectsCloneCmd(client *projects.Client) *cobra.Command {
	var org string
	cmd := &cobra.Command{
		Use:   "clone [query | url | owner/repo]",
		Short: "Clone a project into the canonical {host}/{org}/{repo} layout",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()

			if org != "" {
				if len(args) != 0 {
					return fmt.Errorf("--org does not take a query argument")
				}
				return cloneOrg(ctx, client, cmd, org)
			}

			if len(args) == 1 {
				if r, ok := projects.ParseCloneTarget(args[0]); ok {
					return cloneOnly(ctx, client, cmd, r)
				}
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

			candidates := all
			if len(args) == 1 {
				query := args[0]
				candidates = filterRepos(all, "", query)
				if len(candidates) == 0 {
					return fmt.Errorf("no project matching %q across local, GitHub, or Gitea", query)
				}
				if len(candidates) == 1 {
					return cloneOnly(ctx, client, cmd, candidates[0])
				}
				// Multiple matches → interactive selector below.
			}

			chosen, err := pickRepo(candidates)
			if err != nil {
				return err
			}
			return cloneOnly(ctx, client, cmd, chosen)
		},
	}
	cmd.Flags().StringVar(&org, "org", "", "bulk-clone every repo owned by this GitHub user/org")
	return cmd
}

// cloneOrg bulk-clones every repo GitHub lists for org, sequentially. Each
// dest (or already-on-disk annotation) goes through cloneOnly, so the stdout
// contract stays one path per line; a single repo's clone failure is reported
// on stderr and counted rather than aborting the rest of the batch.
func cloneOrg(ctx context.Context, client *projects.Client, cmd *cobra.Command, org string) error {
	repos, err := client.ListOrg(ctx, org)
	if err != nil {
		return fmt.Errorf("listing %s's GitHub repos: %w", org, err)
	}
	if len(repos) == 0 {
		return fmt.Errorf("no repos found for GitHub user/org %q", org)
	}
	var failed int
	for _, r := range repos {
		if err := cloneOnly(ctx, client, cmd, r); err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "error: %s/%s: %v\n", r.Owner, r.Name, err)
			failed++
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d repos failed to clone", failed, len(repos))
	}
	return nil
}

// cloneOnly clones r into the canonical layout unless it's already on disk, in
// which case it's annotated (stderr) rather than re-cloned — same
// already-on-disk shape as openOrClone, minus the tmux Open step. The
// destination path is the scriptable stdout contract.
func cloneOnly(ctx context.Context, client *projects.Client, cmd *cobra.Command, r projects.Repo) error {
	if r.Cloned {
		fmt.Fprintf(cmd.ErrOrStderr(), "%s/%s already on disk at %s\n", r.Owner, r.Name, r.LocalPath)
		fmt.Fprintln(cmd.OutOrStdout(), r.LocalPath)
		return nil
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "Cloning %s/%s from %s…\n", r.Owner, r.Name, r.Host)
	dest, err := client.Clone(ctx, r)
	if err != nil {
		return err
	}
	fmt.Fprintln(cmd.OutOrStdout(), dest)
	return nil
}
