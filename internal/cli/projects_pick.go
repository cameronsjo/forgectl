package cli

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/projects"
)

// newProjectsPickCmd is the workhorse: fuzzy match, interactive huh selector,
// or GitHub clone-on-miss depending on what's found locally.
//
//   - No args: huh.NewSelect over all Discover() results, grouped by git status.
//   - With query: fuzzy match; auto-open if unique, filtered huh.Select if
//     multiple, GitHub clone-on-miss if zero local matches.
func newProjectsPickCmd(client *projects.Client) *cobra.Command {
	return &cobra.Command{
		Use:   "pick [query]",
		Short: "Open a project in a tmux session (interactive or by name)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()

			all, err := client.Discover(ctx)
			if err != nil {
				return err
			}
			if len(all) == 0 {
				return fmt.Errorf("no projects found in %s", client.Dir)
			}

			var candidates []projects.Project

			if len(args) == 1 {
				query := args[0]
				candidates = fuzzyMatch(all, query)
				if len(candidates) == 0 {
					// Clone-on-miss: search GitHub.
					repos, err := client.GitHubSearch(ctx, query)
					if err != nil {
						return err
					}
					if len(repos) == 0 {
						return fmt.Errorf("no local project or GitHub repo matching %q", query)
					}
					name, err := pickString(repos, "Clone which repo?")
					if err != nil {
						return err
					}
					fmt.Fprintf(cmd.OutOrStderr(), "Cloning %s…\n", name)
					return client.CloneFromGitHub(ctx, query, name)
				}
				if len(candidates) == 1 {
					return client.Open(ctx, candidates[0].Dir)
				}
				// Multiple matches → fall through to interactive selector below.
			} else {
				candidates = all
			}

			chosen, err := pickProject(candidates)
			if err != nil {
				return err
			}
			return client.Open(ctx, chosen.Dir)
		},
	}
}

// fuzzyMatch returns projects whose name contains query (case-insensitive).
func fuzzyMatch(ps []projects.Project, query string) []projects.Project {
	q := strings.ToLower(query)
	var out []projects.Project
	for _, p := range ps {
		if strings.Contains(strings.ToLower(p.Name), q) {
			out = append(out, p)
		}
	}
	return out
}

// pickProject runs huh.NewSelect over the given project list and returns the
// chosen project.
func pickProject(ps []projects.Project) (projects.Project, error) {
	opts := make([]huh.Option[string], len(ps))
	for i, p := range ps {
		opts[i] = huh.NewOption(p.DisplayLine(), p.Dir)
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
		return projects.Project{}, err
	}
	// Find the project by dir.
	for _, p := range ps {
		if p.Dir == chosen {
			return p, nil
		}
	}
	return projects.Project{}, fmt.Errorf("no project selected")
}

// pickString runs huh.NewSelect over a plain string list and returns the choice.
func pickString(items []string, title string) (string, error) {
	opts := make([]huh.Option[string], len(items))
	for i, s := range items {
		opts[i] = huh.NewOption(s, s)
	}
	var chosen string
	err := huh.NewForm(
		huh.NewGroup(
			huh.NewSelect[string]().
				Title(title).
				Options(opts...).
				Value(&chosen),
		),
	).Run()
	return chosen, err
}
