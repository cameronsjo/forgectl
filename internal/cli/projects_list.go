package cli

import (
	"fmt"
	"io"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/githubauth"
	"github.com/cameronsjo/forgectl/internal/projects"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// newProjectsListCmd builds `forgectl projects list [query]` — the scriptable,
// no-TTY-required inventory of every project across local clones, GitHub, and
// Gitea. This is the Claude-callable contract: `--json` emits the raw record
// list to stdout; per-host degradation notes and the human summary go to
// stderr, so a `--json` pipe is never polluted by progress chatter.
func newProjectsListCmd(client *projects.Client) *cobra.Command {
	var asJSON bool
	var host string
	cmd := &cobra.Command{
		Use:   "list [query]",
		Short: "List projects across local, GitHub, and Gitea (cloned + uncloned)",
		Long: "List every project across local clones, the configured GitHub host,\n" +
			"and the Gitea instance tea is logged into, marking which are checked\n" +
			"out. Each row's host is its full hostname; Gitea rows take theirs from\n" +
			"the repo's own clone URL, so no Gitea host is configured anywhere.\n\n" +
			"GitHub scope comes from [projects] owners in config.toml; leave it unset\n" +
			"and forgectl lists the repos of whoever gh is authenticated as. Every gh\n" +
			"call is pinned to the deployment's [github] host (default github.com) —\n" +
			"an ambient GH_HOST is overridden, and a non-default host requires a\n" +
			"stored `gh auth login --hostname <host>` credential.\n\n" +
			"Examples:\n" +
			"  forgectl projects list                 # human table, all hosts\n" +
			"  forgectl projects list --json          # machine-readable, for scripts\n" +
			"  forgectl projects list --host git.sjo.lol   # only that host's repos\n" +
			"  forgectl projects find homeclaw        # 'find' alias + a name filter",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()

			// One stderr line names a non-default GitHub host, so a surprising
			// inventory is never silently attributable to the wrong forge. The
			// value is ResolveHost-validated at construction; termsafe guards
			// the render anyway. Stderr only — a --json pipe stays clean.
			if gh := client.GitHubHost(); gh != githubauth.DefaultHost {
				// Best-effort diagnostic write, same as every stderr note here.
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "github host: %s\n", termsafe.SafeLine(gh))
			}

			repos, notes, err := client.Inventory(ctx)
			// Per-host degradation notes are diagnostics → stderr, never stdout,
			// and escaped on the way: a note can carry a filesystem path or a
			// host label built from low-trust config. They render BEFORE the
			// error return for the same reason `review` does it — an
			// all-hosts failure is exactly when the per-host notes matter most.
			renderDegradationNotes(cmd, notes)
			if err != nil {
				return err
			}

			// --host is a CLOSED allowlist over the hosts this deployment can
			// actually produce, not a free-text match: the configured GitHub
			// host, "local" for repos with no parseable origin, or any host
			// the current inventory carries. Anything else is a typo that
			// would otherwise return a confident empty list — the same shape
			// as a real "no repos there" answer.
			//
			// The allowlist is checked AFTER the inventory is built, since the
			// Gitea hosts are discovered from the rows themselves rather than
			// configured. Rejecting is worth the wait: an empty result the
			// operator reads as fact is the failure being prevented.
			if host != "" {
				known := knownHosts(repos)
				known[client.GitHubHost()] = true // valid even when it returned no rows
				if !known[strings.ToLower(host)] {
					// The operator typed this, so echoing it is safe and is the
					// whole diagnostic; the suggestion list is derived from
					// server-supplied hostnames, so it goes through termsafe.
					return fmt.Errorf("unknown --host %q; this inventory has: %s",
						host, termsafe.SafeLine(strings.Join(slices.Sorted(maps.Keys(known)), ", ")))
				}
			}

			query := ""
			if len(args) == 1 {
				query = args[0]
			}
			repos = filterRepos(repos, host, query)

			if asJSON {
				if repos == nil {
					repos = []projects.Repo{}
				}
				enc := termsafe.JSONEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(repos)
			}
			return renderRepoTable(cmd.OutOrStdout(), cmd.ErrOrStderr(), repos)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable JSON to stdout")
	cmd.Flags().StringVar(&host, "host", "", "filter by hostname (e.g. github.com, git.sjo.lol) or \"local\"")
	return cmd
}

// filterRepos narrows the inventory by host and/or a case-insensitive name
// substring. Either filter empty means "don't filter on it".
func filterRepos(repos []projects.Repo, host, query string) []projects.Repo {
	if host == "" && query == "" {
		return repos
	}
	// Host comparison is case-insensitive even though canonicalHost normalizes
	// every value it produces: the operator types this one, and a --host
	// GitHub.com that silently matched nothing would read as "no repos there".
	h := strings.ToLower(host)
	q := strings.ToLower(query)
	out := make([]projects.Repo, 0, len(repos))
	for _, r := range repos {
		if h != "" && !hostMatches(r, h) {
			continue
		}
		if q != "" && !repoMatchesQuery(r, q) {
			continue
		}
		out = append(out, r)
	}
	return out
}

// hostMatches reports whether r belongs to the lowercased host filter, with
// "local" naming the repos that have no parseable origin (Host == "").
func hostMatches(r projects.Repo, host string) bool {
	if host == localHostFilter {
		return r.Host == ""
	}
	return strings.ToLower(r.Host) == host
}

// localHostFilter is the --host value naming repos with no parseable origin.
// They have an empty Host, which is not a value an operator can type.
const localHostFilter = "local"

// knownHosts returns the set of --host values valid for this inventory: every
// host actually present, plus "local" when any origin-less repo is in it.
func knownHosts(repos []projects.Repo) map[string]bool {
	out := make(map[string]bool, 4)
	for _, r := range repos {
		if r.Host == "" {
			out[localHostFilter] = true
			continue
		}
		out[strings.ToLower(r.Host)] = true
	}
	return out
}

// repoMatchesQuery reports whether r matches a lowercased name query — by repo
// name, or (for a local clone whose directory name differs from the repo name,
// e.g. a fork or renamed checkout) by its directory basename, so the project
// stays findable by the name the user actually sees on disk.
func repoMatchesQuery(r projects.Repo, q string) bool {
	if strings.Contains(strings.ToLower(r.Name), q) {
		return true
	}
	if r.LocalPath != "" && strings.Contains(strings.ToLower(filepath.Base(r.LocalPath)), q) {
		return true
	}
	return false
}

// renderRepoTable writes a grep-friendly HOST/REPO/STATUS table to out (the
// human payload) and a one-line count summary to errOut (a diagnostic).
func renderRepoTable(out, errOut io.Writer, repos []projects.Repo) error {
	tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "HOST\tREPO\tSTATUS"); err != nil {
		return err
	}
	cloned := 0
	for _, r := range repos {
		// Host, Owner, and Name are all server-supplied — a remote URL's
		// hostname, gh's JSON, tea's TSV columns — and this writes them to a
		// terminal. Host reaches here for EVERY row now that it is a hostname
		// rather than one of two fixed tokens, but owner and name always did.
		host := termsafe.SafeLine(r.Host)
		if host == "" {
			host = "local"
		}
		status := "uncloned"
		if r.Cloned {
			cloned++
			status = strings.Trim(r.Status.Label(), "[]")
			if status == "" {
				status = "unknown"
				if r.Status.State == projects.StatusNotRepo {
					status = "not-a-repo"
				}
			}
		}
		name := termsafe.SafeLine(r.Name)
		if r.Owner != "" {
			name = termsafe.SafeLine(r.Owner) + "/" + name
		}
		if r.Mirror {
			name += " (mirror)"
		}
		if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\n", host, name, status); err != nil {
			return err
		}
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Fprintf(errOut, "%d projects (%d cloned, %d remote-only)\n",
		len(repos), cloned, len(repos)-cloned)
	return nil
}
