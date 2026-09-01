package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/projects"
	"github.com/cameronsjo/forgectl/internal/termsafe"
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
	var wing string
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "clone [query | url | owner/repo]",
		Short: "Clone a project into the canonical {host}/{org}/{repo} layout",
		Long: `Clone a project into the canonical layout. With both stdin and stdout attached
to terminals, ambiguous matches use the existing picker. Otherwise forgectl writes one
sanitized candidate identity per line to stdout and exits 1; use the candidate sshUrl
from projects list --json, or rerun interactively when no sshUrl is available.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()

			if wing != "" && org != "" {
				return fmt.Errorf("--wing and --org are mutually exclusive: --org clones a whole account, and filing all of it into one wing is never what you want")
			}

			if org != "" {
				if len(args) != 0 {
					return fmt.Errorf("--org does not take a query argument")
				}
				return cloneOrg(ctx, client, cmd, org, dryRun)
			}

			if len(args) == 1 {
				if r, ok := projects.ParseCloneTarget(args[0], client.GitHubHost()); ok {
					return cloneOnly(ctx, client, cmd, r, wing, dryRun)
				}
			}

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
					return cloneOnly(ctx, client, cmd, candidates[0], wing, dryRun)
				}
				// Multiple matches → interactive selector below.
			}

			chosen, err := chooseRepo(cmd, candidates, projectSelectionClone)
			if err != nil {
				return err
			}
			return cloneOnly(ctx, client, cmd, chosen, wing, dryRun)
		},
	}
	cmd.Flags().StringVar(&org, "org", "", "bulk-clone every repo owned by this GitHub user/org")
	cmd.Flags().StringVar(&wing, "wing", "", "file this clone under <projects>/<wing>/<repo>, overriding the [[projects.wings]] table")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print where the clone would land and exit, touching nothing")
	return cmd
}

// cloneOrg bulk-clones every repo GitHub lists for org, sequentially. Each
// dest (or already-on-disk annotation) goes through cloneOnly, so the stdout
// contract stays one path per line; a single repo's clone failure is reported
// on stderr and counted rather than aborting the rest of the batch.
func cloneOrg(ctx context.Context, client *projects.Client, cmd *cobra.Command, org string, dryRun bool) error {
	repos, err := client.ListOrg(ctx, org)
	if err != nil {
		return fmt.Errorf("listing %s's GitHub repos: %w", org, err)
	}
	if len(repos) == 0 {
		return fmt.Errorf("no repos found for GitHub user/org %q", org)
	}
	var failed int
	for _, r := range repos {
		if err := cloneOnly(ctx, client, cmd, r, "", dryRun); err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "error: %s/%s: %v\n",
				termsafe.SafeLine(r.Owner), termsafe.SafeLine(r.Name), err)
			failed++
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d repos failed to clone", failed, len(repos))
	}
	return nil
}

// cloneOnly clones r where Placement says it belongs unless it's already on
// disk, in which case it's annotated (stderr) rather than re-cloned — same
// already-on-disk shape as openOrClone, minus the tmux Open step. The
// destination path is the scriptable stdout contract, and every branch here
// honors it: exactly one path to stdout, diagnostics to stderr.
//
// wing overrides the configured table for this one clone; "" means "ask the
// table", which is the normal path. dryRun resolves and prints the destination
// without touching the filesystem.
//
// Owner, Name, and Host go through termsafe on the stderr lines: they are
// server-supplied (gh JSON, tea TSV) and this is a terminal. LocalPath and
// dest are forgectl-composed from guarded segments.
func cloneOnly(ctx context.Context, client *projects.Client, cmd *cobra.Command, r projects.Repo, wing string, dryRun bool) error {
	if r.Cloned {
		fmt.Fprintf(cmd.ErrOrStderr(), "%s/%s already on disk at %s\n",
			termsafe.SafeLine(r.Owner), termsafe.SafeLine(r.Name), r.LocalPath)
		fmt.Fprintln(cmd.OutOrStdout(), r.LocalPath)
		return nil
	}
	if wing == "" {
		wing = client.WingFor(r)
	}
	if dryRun {
		dest, err := projects.Placement(client.ProjectsDir(), r, wing)
		if err != nil {
			return err
		}
		fmt.Fprintln(cmd.OutOrStdout(), dest)
		return nil
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "Cloning %s/%s from %s…\n",
		termsafe.SafeLine(r.Owner), termsafe.SafeLine(r.Name), termsafe.SafeLine(r.Host))
	dest, err := client.CloneInto(ctx, r, wing)
	if err != nil {
		return err
	}
	fmt.Fprintln(cmd.OutOrStdout(), dest)
	return nil
}
