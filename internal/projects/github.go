package projects

import (
	"context"
	"fmt"
	"strings"
)

// githubSearch returns repo names from `gh repo list` that match query
// (case-insensitive substring match). Returns empty slice when gh is absent.
func githubSearch(ctx context.Context, run interface {
	Run(context.Context, string, ...string) (string, error)
}, query string) ([]string, error) {
	out, err := run.Run(ctx, "gh", "repo", "list",
		"--no-archived", "--limit", "1000", "--json", "name", "--jq", ".[].name")
	if err != nil {
		// gh not installed or no auth — treat as no results.
		return nil, nil
	}
	var matches []string
	for _, name := range strings.Split(out, "\n") {
		name = strings.TrimSpace(name)
		if name != "" && strings.Contains(strings.ToLower(name), strings.ToLower(query)) {
			matches = append(matches, name)
		}
	}
	return matches, nil
}

// cloneRepo runs `gh repo clone name dest` and returns an error on failure.
func cloneRepo(ctx context.Context, run interface {
	Run(context.Context, string, ...string) (string, error)
}, name, dest string) error {
	_, err := run.Run(ctx, "gh", "repo", "clone", name, dest)
	if err != nil {
		return fmt.Errorf("gh repo clone %s: %w", name, err)
	}
	return nil
}
