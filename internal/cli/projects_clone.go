package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/projects"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// newProjectsCloneCmd clones a repo where Placement says it belongs — its wing
// if it has one, else {host}/{owner}/{repo} — without opening it in tmux, the
// non-interactive sibling of `pick`:
//
//   - No args: huh.NewSelect over the whole cross-host inventory (same picker
//     as `pick`).
//   - A URL or "owner/repo" arg: clone that exact target directly, bypassing
//     the inventory (absorbs git-smart-clone).
//   - Any other arg: fuzzy match by name against the inventory; auto-clone if
//     unique, filtered selector if multiple, error if nothing matches.
//   - --org <login>: bulk-clone every repo GitHub lists for that user/org
//     (absorbs gh-clone-org), one dest per stdout line.
//   - --wing <name>: override the [[projects.wings]] table for this one clone.
//   - --dry-run: print the destination and exit, touching nothing.
//
// A candidate already on disk is annotated rather than re-cloned, so `clone` is
// safe to run repeatedly to "make sure everything's checked out". Discover
// finds all three layouts (wing, host tree, and legacy flat), and Clone
// additionally probes the other of the two placement rules before creating
// anything, so routing a repo to a wing it is not yet filed under reports the
// existing checkout instead of minting a duplicate.
func newProjectsCloneCmd(client *projects.Client) *cobra.Command {
	var org string
	var wing string
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "clone [query | url | owner/repo]",
		Short: "Clone a project into its wing, or the {host}/{owner}/{repo} tree",
		Long: `Clone a project into the canonical layout: <projects>/<wing>/<repo> when the repo
is listed in a [[projects.wings]] entry, otherwise <projects>/<host>/<owner>/<repo>
under the remote's full hostname. --dry-run prints the destination without touching
the filesystem; --wing overrides the table for one clone.

With both stdin and stdout attached to terminals, ambiguous matches use the existing
picker. Otherwise forgectl writes one sanitized candidate identity per line to stdout
and exits 1; use the candidate sshUrl from projects list --json, or rerun
interactively when no sshUrl is available.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()

			if wing != "" && org != "" {
				return fmt.Errorf("--wing and --org are mutually exclusive: --org clones a whole account, and filing all of it into one wing is never what you want")
			}
			if wing != "" {
				// The flag is a SECOND entry point into the wing namespace, so
				// it takes the same rule the config table does — otherwise
				// `--wing github.com` files a repo one level up from where the
				// host walk expects them, and a `:` or leading `.` reaches a
				// directory name.
				validated, err := projects.ValidateWingName(client.GitHubHost(), wing)
				if err != nil {
					return fmt.Errorf("--wing: %w", err)
				}
				wing = validated
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
			// Best-effort diagnostic write, same as every stderr note here.
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "error: %s/%s: %v\n",
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
// server-supplied (gh JSON, tea TSV) and this is a terminal.
//
// LocalPath goes through QuotePath — it is NOT composed from guarded segments,
// whatever it may look like. localRepos builds it from raw os.ReadDir entry
// names, so any directory under the projects root, however it got there, can
// carry ANSI or bidi controls into it. resolve.go wraps the same class of
// value for the same reason. `dest` is genuinely forgectl-composed and stays
// bare, so the scriptable stdout contract keeps emitting a usable path.
func cloneOnly(ctx context.Context, client *projects.Client, cmd *cobra.Command, r projects.Repo, wing string, dryRun bool) error {
	if r.Cloned {
		// Best-effort diagnostic write, same as every stderr note here.
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "%s/%s already on disk at %s\n",
			termsafe.SafeLine(r.Owner), termsafe.SafeLine(r.Name), termsafe.QuotePath(r.LocalPath))
		// The one stdout line is the scriptable contract; a failed write there is

		// the caller's pipe closing, not something this command can act on.
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), r.LocalPath)
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
		// The one stdout line is the scriptable contract; a failed write there is

		// the caller's pipe closing, not something this command can act on.
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), dest)
		return nil
	}
	// Best-effort diagnostic write, same as every stderr note here.
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Cloning %s/%s from %s…\n",
		termsafe.SafeLine(r.Owner), termsafe.SafeLine(r.Name), termsafe.SafeLine(r.Host))
	dest, err := client.CloneInto(ctx, r, wing)
	if err != nil {
		return err
	}
	// The one stdout line is the scriptable contract; a failed write there is

	// the caller's pipe closing, not something this command can act on.
	_, _ = fmt.Fprintln(cmd.OutOrStdout(), dest)
	return nil
}
