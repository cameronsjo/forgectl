package cli

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/keymap"
	"github.com/cameronsjo/forgectl/internal/projects"
)

type projectSelectionMode uint8

const (
	projectSelectionPick projectSelectionMode = iota
	projectSelectionClone
	projectSelectionWorktree
)

var pickRepoFn = pickRepo

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
		Long: `Open a project in tmux. With both stdin and stdout attached to terminals,
ambiguous matches use the existing picker. Otherwise forgectl writes one sanitized
candidate identity per line to stdout and exits 1; narrow to a unique project name
when possible, inspect identities with projects list --json, or rerun interactively.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()

			all, notes, err := client.Inventory(ctx)
			if err != nil {
				return err
			}
			renderDegradationNotes(cmd, notes)
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

			chosen, err := chooseRepo(cmd, candidates, projectSelectionPick)
			if err != nil {
				return err
			}
			return openOrClone(ctx, client, cmd, chosen)
		},
	}
}

func chooseRepo(cmd *cobra.Command, repos []projects.Repo, mode projectSelectionMode) (projects.Repo, error) {
	if isInteractiveTTY() {
		return pickRepoFn(repos)
	}
	if err := writeProjectCandidates(cmd.OutOrStdout(), repos); err != nil {
		return projects.Repo{}, err
	}
	return projects.Repo{}, WithExitCode(projectAmbiguityError(mode, len(repos)), 1)
}

func writeProjectCandidates(out io.Writer, repos []projects.Repo) error {
	for _, repo := range repos {
		if _, err := fmt.Fprintln(out, projectCandidateLine(repo)); err != nil {
			return err
		}
	}
	return nil
}

func projectCandidateLine(repo projects.Repo) string {
	var host, identity string
	if repo.Host == "" || repo.Owner == "" || repo.Name == "" {
		host = "local"
		path := repo.LocalPath
		if path == "" {
			path = "<unknown>"
		}
		identity = "path:" + sanitizeCandidate(path)
	} else {
		host = sanitizeCandidate(repo.Host)
		identity = sanitizeCandidate(repo.Owner) + "/" + sanitizeCandidate(repo.Name)
	}
	status := projectCandidateStatus(repo)
	return host + "  " + identity + "  " + sanitizeCandidate(status)
}

func projectCandidateStatus(repo projects.Repo) string {
	var status string
	if !repo.Cloned {
		status = "uncloned"
	} else {
		if label := strings.Trim(repo.Status.Label(), "[]"); label != "" {
			status = label
		} else {
			switch repo.Status.State {
			case projects.StatusNotRepo:
				status = "not-a-repo"
			case projects.StatusUnknown:
				status = "unknown"
			default:
				status = "cloned"
			}
		}
	}
	if repo.Mirror {
		status += ", mirror"
	}
	return status
}

// sanitizeCandidate renders one fixed-column candidate field. It is safeTerm
// alone: the sink used to need a second pass to neutralize the tab the old
// sanitizer deliberately preserved, and SafeLine escapes tab like every other
// non-graphic rune, so the layout rule this sink needs now falls out of the
// shared boundary rather than being maintained beside it.
func sanitizeCandidate(s string) string {
	return safeTerm(s)
}

func projectAmbiguityError(mode projectSelectionMode, count int) error {
	switch mode {
	case projectSelectionClone:
		return fmt.Errorf("%d projects require a clone selection, and there is no interactive terminal — get the candidate's sshUrl from `forgectl projects list --json` and pass that URL; owner/repo is exact only for GitHub candidates, and any candidate without an sshUrl (including local-only) requires an interactive rerun; candidates are on stdout", count)
	case projectSelectionWorktree:
		return fmt.Errorf("%d projects require a worktree selection, and there is no interactive terminal — get the candidate's sshUrl from `forgectl projects list --json` and pass that URL; owner/repo is exact only for GitHub candidates, and any candidate without an sshUrl (including local-only) requires an interactive rerun; candidates are on stdout", count)
	default:
		return fmt.Errorf("%d projects require a selection, and there is no interactive terminal — narrow to a unique project name when possible, or rerun interactively; inspect identities with `forgectl projects list --json`; candidates are on stdout", count)
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
				Title("Projects — enter to pick, esc to cancel").
				Options(opts...).
				Value(&chosen),
		),
	).WithKeyMap(keymap.Cancel()).Run()
	if err != nil {
		return projects.Repo{}, err
	}
	if r, ok := byKey[chosen]; ok {
		return r, nil
	}
	return projects.Repo{}, fmt.Errorf("no project selected")
}
