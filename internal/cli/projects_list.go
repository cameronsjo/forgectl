package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/projects"
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
		Long: "List every project across local clones, github.com/cameronsjo, and the\n" +
			"self-hosted Gitea (git.sjo.lol/cameron), marking which are checked out.\n\n" +
			"Examples:\n" +
			"  forgectl projects list                 # human table, all hosts\n" +
			"  forgectl projects list --json          # machine-readable, for scripts\n" +
			"  forgectl projects list --host gitea    # only git.sjo.lol repos\n" +
			"  forgectl projects find homeclaw        # 'find' alias + a name filter",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()

			if host != "" && host != "github" && host != "gitea" {
				return fmt.Errorf("invalid --host %q: want github or gitea", host)
			}

			repos, notes, err := client.Inventory(ctx)
			if err != nil {
				return err
			}
			// Per-host degradation notes are diagnostics → stderr, never stdout.
			for _, n := range notes {
				fmt.Fprintln(cmd.ErrOrStderr(), "note: "+n)
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
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(repos)
			}
			return renderRepoTable(cmd.OutOrStdout(), cmd.ErrOrStderr(), repos)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable JSON to stdout")
	cmd.Flags().StringVar(&host, "host", "", "filter by host: github | gitea")
	return cmd
}

// filterRepos narrows the inventory by host and/or a case-insensitive name
// substring. Either filter empty means "don't filter on it".
func filterRepos(repos []projects.Repo, host, query string) []projects.Repo {
	if host == "" && query == "" {
		return repos
	}
	q := strings.ToLower(query)
	out := make([]projects.Repo, 0, len(repos))
	for _, r := range repos {
		if host != "" && r.Host != host {
			continue
		}
		if q != "" && !repoMatchesQuery(r, q) {
			continue
		}
		out = append(out, r)
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
		host := r.Host
		if host == "" {
			host = "local"
		}
		status := "uncloned"
		if r.Cloned {
			cloned++
			status = strings.Trim(r.Status.Label(), "[]")
			if status == "" {
				status = "cloned"
			}
		}
		name := r.Name
		if r.Owner != "" {
			name = r.Owner + "/" + r.Name
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
