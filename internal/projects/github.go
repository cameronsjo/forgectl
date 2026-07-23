package projects

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
)

// githubOwner is the GitHub account whose repos the inventory enumerates.
const githubOwner = "cameronsjo"

// githubList returns structured records for every repo owned by githubOwner —
// a thin wrapper over githubListOrg for the single account the inventory
// tracks.
func githubList(ctx context.Context, run interface {
	Run(context.Context, string, ...string) (string, error)
}) ([]Repo, error) {
	return githubListOrg(ctx, run, githubOwner)
}

// githubListOrg returns structured records for every repo owned by org (any
// GitHub user or org login, not just githubOwner) via `gh repo list --json` —
// the bulk-clone path (`projects clone --org`) needs an arbitrary login, not
// just the inventory's own account. Archived repos are included — the
// inventory is a finder, and you may still want to open an archived project.
// Returns the command error on failure so callers can note the degraded host;
// a JSON parse failure is treated the same way.
func githubListOrg(ctx context.Context, run interface {
	Run(context.Context, string, ...string) (string, error)
}, org string) ([]Repo, error) {
	slog.Debug("Preparing to fetch GitHub repos.", "owner", org)
	out, err := run.Run(ctx, "gh", "repo", "list", org,
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
		repos = append(repos, Repo{
			Host:    "github",
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
func cloneRepo(ctx context.Context, run interface {
	Run(context.Context, string, ...string) (string, error)
}, name, dest string) error {
	slog.Debug("Preparing to clone from GitHub.", "repo", name, "dest", dest)
	_, err := run.Run(ctx, "gh", "repo", "clone", name, dest)
	if err != nil {
		slog.Error("Failed to clone from GitHub.", "repo", name, "dest", dest, "error", err)
		return fmt.Errorf("gh repo clone %s: %w", name, err)
	}
	slog.Info("Successfully cloned from GitHub.", "repo", name, "dest", dest)
	return nil
}
