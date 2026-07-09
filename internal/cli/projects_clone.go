package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/projects"
)

// newProjectsCloneCmd clones a repo from the unified cross-host inventory into
// the canonical {host}/{org}/{repo} layout, without opening it in tmux — the
// non-interactive sibling of `pick`:
//
//   - No args: huh.NewSelect over the whole inventory (same picker as `pick`).
//   - With query: fuzzy match by name; auto-clone if unique, filtered selector
//     if multiple, error if nothing matches anywhere.
//
// A candidate already on disk (canonical or legacy flat layout — Discover
// finds both) is annotated rather than re-cloned, so `clone` is safe to run
// repeatedly to "make sure everything's checked out".
func newProjectsCloneCmd(client *projects.Client) *cobra.Command {
	return &cobra.Command{
		Use:   "clone [query]",
		Short: "Clone a project into the canonical {host}/{org}/{repo} layout",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()

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
