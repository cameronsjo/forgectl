package projects

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
)

// giteaList returns repos from the self-hosted Gitea via
// `tea repo ls --output tsv`, letting tea resolve its own configured login.
// The TSV columns are owner, name, type, ssh. The header row and tea's stderr
// `NOTE: … falling back to login 'cameron'` line are filtered out defensively
// (only well-formed 4-field rows survive), even though OSRunner captures
// stdout alone. Returns the command error on failure so Inventory can note the
// degraded host; callers treat a nil slice as "no rows".
//
// EACH ROW'S HOST COMES FROM ITS OWN CLONE URL, not from configuration. tea
// reports the full URL in the fourth column — ssh or https, whichever the
// server hands back — and parseRemoteURL already extracts a normalized
// hostname from either form. That is strictly closer to the data than a
// configured host would be: it is literally where the repo lives, it
// generalizes to any Gitea instance without a config key, and it keeps a
// personal hostname out of this source file.
//
// gitHubHost is passed only to reject with. Two rows are dropped rather than
// filed, both fail-closed:
//
//   - No derivable hostname. There is no default to fall back to, and a
//     hostless Repo keys as "local:" with an empty path — colliding with every
//     other such row and displacing real local clones out of the inventory.
//   - A hostname equal to the configured GitHub host. This is the one thing a
//     hostile or misconfigured Gitea could do that matters: a row claiming the
//     GitHub host would take the gh dispatch branch in Clone and Worktree, and
//     be fetched from GitHub by the server's own owner/name strings.
func giteaList(ctx context.Context, run interface {
	Run(context.Context, string, ...string) (string, error)
}, gitHubHost string) ([]Repo, error) {
	slog.Debug("Preparing to fetch Gitea repos.")
	out, err := run.Run(ctx, "tea", "repo", "ls", "--output", "tsv", "--limit", "1000")
	if err != nil {
		slog.Error("Failed to fetch Gitea repos.", "error", err)
		return nil, err
	}

	var repos []Repo
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Split(strings.TrimRight(line, "\r"), "\t")
		if len(fields) != 4 {
			// NOTE/blank lines lack the 4 tab-fields; the header has them but is
			// matched out below.
			continue
		}
		owner, name, typ, ssh := fields[0], fields[1], fields[2], fields[3]
		if owner == "owner" && name == "name" && typ == "type" && ssh == "ssh" {
			continue // header row — match all four columns exactly
		}
		if owner == "" || name == "" {
			continue // malformed row — a real repo always has an owner and name
		}
		// Both become directories; drop rather than repair (see validRepoSegment).
		if !validRepoSegment(owner) || !validRepoSegment(name) {
			slog.Warn("Dropped a Gitea row whose owner or name is not a safe path segment.")
			continue
		}
		host, _, _ := parseRemoteURL(ssh, gitHubHost)
		if host == "" {
			slog.Warn("Dropped a Gitea row with no derivable host in its clone URL.")
			continue
		}
		if host == gitHubHost {
			slog.Warn("Dropped a Gitea row claiming the configured GitHub host.")
			continue
		}
		repos = append(repos, Repo{
			Host:   host,
			Owner:  owner,
			Name:   name,
			SSHURL: ssh,
			Mirror: typ == "mirror",
		})
	}
	slog.Info("Successfully fetched Gitea repos.", "count", len(repos))
	return repos, nil
}

// cloneFromGitea clones a Gitea repo into dest over SSH (port 222 is baked into
// the URL tea reports). HTTPS create/clone is irrelevant here — push and clone
// both ride Apple's ssh transport.
func cloneFromGitea(ctx context.Context, run interface {
	Run(context.Context, string, ...string) (string, error)
}, sshURL, dest string) error {
	if sshURL == "" {
		slog.Error("Cannot clone over SSH: empty URL.")
		return fmt.Errorf("cannot clone over SSH: empty URL")
	}
	slog.Debug("Preparing to clone over SSH.", "dest", dest)
	// The URL is server-controlled (it comes from the repo-list output), and git's
	// ext::/fd:: smart transports execute arbitrary commands — so a malicious or
	// MITM'd list source could smuggle `ext::sh -c …` into a clone. Disable those
	// transports, and end options with `--` so a "-"-leading URL can't be read as
	// a flag.
	if _, err := run.Run(ctx, "git",
		"-c", "protocol.ext.allow=never",
		"-c", "protocol.fd.allow=never",
		"clone", "--", sshURL, dest); err != nil {
		slog.Error("Failed to clone over SSH.", "dest", dest, "error", err)
		return fmt.Errorf("git clone %s: %w", sshURL, err)
	}
	slog.Info("Successfully cloned over SSH.", "dest", dest)
	return nil
}

// cloneBareFromURL bare-clones sshURL into dest over SSH — the worktree layout's
// bare-clone step for any non-github host. It carries the same ext::/fd:: smart-
// transport guard and `--` hardening as cloneFromGitea: the URL is server-
// controlled (it comes from the repo-list output), so it must not be able to
// smuggle an `ext::sh -c …` command or a flag-leading value into the git argv.
func cloneBareFromURL(ctx context.Context, run interface {
	Run(context.Context, string, ...string) (string, error)
}, sshURL, dest string) error {
	if sshURL == "" {
		slog.Error("Cannot bare-clone over SSH: empty URL.")
		return fmt.Errorf("cannot bare-clone over SSH: empty URL")
	}
	slog.Debug("Preparing to bare-clone over SSH.", "dest", dest)
	if _, err := run.Run(ctx, "git",
		"-c", "protocol.ext.allow=never",
		"-c", "protocol.fd.allow=never",
		"clone", "--bare", "--", sshURL, dest); err != nil {
		slog.Error("Failed to bare-clone over SSH.", "dest", dest, "error", err)
		return fmt.Errorf("git clone --bare %s: %w", sshURL, err)
	}
	slog.Info("Successfully bare-cloned over SSH.", "dest", dest)
	return nil
}
