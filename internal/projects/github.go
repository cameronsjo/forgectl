package projects

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/githubauth"
)

// ownerListResult is one owner's repo-list outcome, held only long enough to
// fold it in input order. The error stays internal: it is reduced to a
// categorical note before anything reaches a human sink.
type ownerListResult struct {
	repos []Repo
	err   error
}

// githubList returns structured records for every repo owned by the resolved
// owner set, plus one categorical note per owner whose query failed.
//
// configured is the low-trust [projects].owners list. Empty means "whoever is
// authenticated on GitHub.com", resolved once. Resolution happens in full
// before any repo query, so a malformed owner anywhere in the list means zero
// queries rather than a silently narrowed inventory.
//
// A partial failure is not an error: healthy owners' repos come back with a
// note naming each failed owner. Only an every-owner failure returns an error,
// and neither the notes nor that error ever carry raw gh output — gh stderr
// can hold tokens and terminal control sequences, and these strings end up on
// a terminal.
func githubList(ctx context.Context, run exec.Runner, configured []string, host string) ([]Repo, []string, error) {
	owners, err := githubauth.ResolveOwners(ctx, run, configured, host)
	if err != nil {
		slog.Warn("Failed to resolve GitHub owners.", "configured", len(configured), "error", err)
		return nil, nil, err
	}

	results := fanOut(owners, func(owner string) ownerListResult {
		repos, err := githubListOrg(ctx, run, owner, host)
		return ownerListResult{repos: repos, err: err}
	})

	var (
		repos  []Repo
		notes  []string
		failed int
	)
	for i, res := range results {
		if res.err != nil {
			failed++
			notes = append(notes, fmt.Sprintf("github(%s): query failed", owners[i]))
			continue
		}
		repos = append(repos, res.repos...)
	}
	if failed > 0 && failed == len(owners) {
		slog.Warn("Every GitHub owner query failed.", "owners", len(owners))
		return nil, notes, errors.New("github: every owner query failed")
	}
	slog.Info("Successfully fetched GitHub repos across owners.", "owners", len(owners), "failed", failed, "count", len(repos))
	return repos, notes, nil
}

// githubListOrg returns structured records for every repo owned by org (any
// GitHub user or org login) via `gh repo list --json`. Archived repos are
// included — the inventory is a finder, and you may still want to open an
// archived project. Returns the command error on failure so callers can note
// the degraded owner; a JSON parse failure is treated the same way.
//
// org is revalidated here, immediately before argv construction, rather than
// trusted from the caller: this is the last point before a config value, a
// discovered login, or a `clone --org` argument becomes a subprocess argument.
// The call is host-pinned, so an ambient GH_HOST cannot silently redirect the
// listing at a GitHub Enterprise instance and have its repos stamped
// Host: "github".
func githubListOrg(ctx context.Context, run exec.Runner, org, host string) ([]Repo, error) {
	if !githubauth.ValidOwner(org) {
		// Deliberately value-free: this string can reach a terminal, and org
		// is exactly the untrusted value that failed validation.
		return nil, errors.New("GitHub owner is outside the allowed owner charset")
	}
	slog.Debug("Preparing to fetch GitHub repos.", "owner", org)
	out, err := githubauth.Runner(run, host).Run(ctx, "gh", "repo", "list", org,
		"--limit", "1000", "--json", "name,sshUrl,isPrivate")
	if err != nil {
		slog.Error("Failed to fetch GitHub repos.", "owner", org, "error", err)
		return nil, err
	}

	var raw []struct {
		Name      string `json:"name"`
		SSHURL    string `json:"sshUrl"`
		IsPrivate bool   `json:"isPrivate"`
	}
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		slog.Error("Failed to parse GitHub JSON.", "owner", org, "error", err)
		return nil, fmt.Errorf("parsing gh repo list JSON: %w", err)
	}

	repos := make([]Repo, 0, len(raw))
	for _, r := range raw {
		// r.Name is server-supplied and becomes a directory. Drop rather than
		// repair: a name we cannot file is a row we cannot act on, and a
		// ".git"/".bare" name would collide with the layout itself. Categorical
		// log — the rejected value is exactly the untrusted one.
		if !validRepoSegment(r.Name) {
			slog.Warn("Dropped a GitHub repo whose name is not a safe path segment.", "owner", org)
			continue
		}
		repos = append(repos, Repo{
			Host:    host,
			Owner:   org,
			Name:    r.Name,
			SSHURL:  r.SSHURL,
			Private: r.IsPrivate,
		})
	}
	slog.Info("Successfully fetched GitHub repos.", "owner", org, "count", len(repos))
	return repos, nil
}

// cloneRepo runs `gh repo clone name dest` and returns an error on failure. It
// keeps gh's credential handling for github.com clones (vs. a bare git clone).
//
// The call is host-pinned, and the pin is applied HERE rather than left to the
// call site: a clone's result persists to disk at Dir/github/<owner>/<name>, so
// an ambient GH_HOST redirecting it at an enterprise instance does not merely
// mislabel a table row — it leaves a checkout that originMatches then disagrees
// with on every later run. Taking exec.Runner (not a bare Run-only interface)
// is what lets the wrap live in here, where no future caller can forget it.
func cloneRepo(ctx context.Context, run exec.Runner, name, dest, host string) error {
	slog.Debug("Preparing to clone from GitHub.", "repo", name, "dest", dest)
	_, err := githubauth.Runner(run, host).Run(ctx, "gh", "repo", "clone", name, dest)
	if err != nil {
		slog.Error("Failed to clone from GitHub.", "repo", name, "dest", dest, "error", err)
		return fmt.Errorf("gh repo clone %s: %w", name, err)
	}
	slog.Info("Successfully cloned from GitHub.", "repo", name, "dest", dest)
	return nil
}

// cloneBareRepo runs `gh repo clone name dest -- --bare`, forwarding --bare to
// the underlying git clone (gh passes post-`--` args straight through). It keeps
// gh's credential handling for github.com, same as cloneRepo — the worktree
// layout's bare-clone step — and the same in-function host pin, for the same
// reason: a bare clone persists to disk too.
func cloneBareRepo(ctx context.Context, run exec.Runner, name, dest, host string) error {
	slog.Debug("Preparing to bare-clone from GitHub.", "repo", name, "dest", dest)
	_, err := githubauth.Runner(run, host).Run(ctx, "gh", "repo", "clone", name, dest, "--", "--bare")
	if err != nil {
		slog.Error("Failed to bare-clone from GitHub.", "repo", name, "dest", dest, "error", err)
		return fmt.Errorf("gh repo clone --bare %s: %w", name, err)
	}
	slog.Info("Successfully bare-cloned from GitHub.", "repo", name, "dest", dest)
	return nil
}
