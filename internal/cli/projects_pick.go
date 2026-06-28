package cli

import (
	"context"
	"fmt"

	"github.com/charmbracelet/huh"
	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/projects"
)

// newProjectsPickCmd is the interactive workhorse over the unified cross-host
// inventory (local clones + GitHub + Gitea):
//
//   - No args: huh.NewSelect over the whole inventory.
//   - With query: fuzzy match by name; auto-open if unique, filtered selector if
//     multiple, error if nothing matches anywhere.
//
// Choosing an uncloned repo clones it (by host) into the projects dir first,
// then opens it in tmux — same zero-typing affordance as before, now reaching
// repos that aren't checked out yet.
func newProjectsPickCmd(client *projects.Client) *cobra.Command {
	return &cobra.Command{
		Use:   "pick [query]",
		Short: "Open a project in tmux (interactive or by name; clones if needed)",
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
					return openOrClone(ctx, client, cmd, candidates[0])
				}
				// Multiple matches → interactive selector below.
			}

			chosen, err := pickRepo(candidates)
			if err != nil {
				return err
			}
			return openOrClone(ctx, client, cmd, chosen)
		},
	}
}

// openOrClone opens a cloned repo directly, or clones an uncloned one (by host)
// before opening. The clone progress line is a diagnostic → stderr.
func openOrClone(ctx context.Context, client *projects.Client, cmd *cobra.Command, r projects.Repo) error {
	dir := r.LocalPath
	if !r.Cloned {
		fmt.Fprintf(cmd.ErrOrStderr(), "Cloning %s/%s from %s…\n", r.Owner, r.Name, r.Host)
		d, err := client.Clone(ctx, r)
		if err != nil {
			return err
		}
		dir = d
	}
	return client.Open(ctx, dir)
}

// pickRepo runs huh.NewSelect over the inventory and returns the chosen repo.
// Options are keyed by Repo.Key() so the selection round-trips unambiguously
// even when the same name exists on both hosts.
func pickRepo(repos []projects.Repo) (projects.Repo, error) {
	opts := make([]huh.Option[string], len(repos))
	byKey := make(map[string]projects.Repo, len(repos))
	for i, r := range repos {
		key := r.Key()
		opts[i] = huh.NewOption(r.DisplayLine(), key)
		byKey[key] = r
	}
	var chosen string
	err := huh.NewForm(
		huh.NewGroup(
			huh.NewSelect[string]().
				Title("Projects").
				Options(opts...).
				Value(&chosen),
		),
	).Run()
	if err != nil {
		return projects.Repo{}, err
	}
	if r, ok := byKey[chosen]; ok {
		return r, nil
	}
	return projects.Repo{}, fmt.Errorf("no project selected")
}
