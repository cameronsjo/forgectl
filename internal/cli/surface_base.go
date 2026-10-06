package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/gitenv"
	"github.com/cameronsjo/forgectl/internal/pr"
)

// A new worker branch starts from a commit forgectl read from GitHub, not
// from a local ref (forgectl#1061). A worker's worktree shares the main
// checkout's .git, so a worker can rewrite refs/remotes/origin/<default> or
// the checkout's HEAD; the next worker branched from either would start at a
// commit the earlier worker chose. Local refs are never trusted for the base:
// the commit hash comes from the GitHub API, the object is fetched into a ref
// namespace only forgectl writes, and the hash, not a ref, goes to
// `git worktree add`.

// workerBaseRefPrefix is where forgectl fetches the default branch's head.
const workerBaseRefPrefix = "refs/forgectl/base/"

// errNoTrustedBase reports a repo whose base cannot be read from GitHub.
var errNoTrustedBase = errors.New("forgectl: cannot read this repo's default branch from GitHub")

// workerBase returns the commit hash a new worker branch in top starts from:
// the head of the GitHub repository's default branch, present locally.
func workerBase(ctx context.Context, run exec.Runner, top string) (string, error) {
	url, err := gitenv.Run(ctx, run, gitenv.Local, "-C", top, "remote", "get-url", "origin")
	if err != nil {
		return "", fmt.Errorf("%w: no origin remote: %w", errNoTrustedBase, err)
	}
	host, owner, repo, ok := pr.ParseRemoteURL(url)
	if !ok || host != "github.com" {
		return "", fmt.Errorf("%w: origin is not a github.com repository", errNoTrustedBase)
	}
	slug := owner + "/" + repo
	branch, err := run.Run(ctx, "gh", "api", "repos/"+slug, "--jq", ".default_branch")
	if err != nil {
		return "", fmt.Errorf("%w: read %s: %w", errNoTrustedBase, slug, err)
	}
	branch = strings.TrimSpace(branch)
	if _, err := gitenv.Run(ctx, run, gitenv.Local, "check-ref-format", "--branch", branch); err != nil || strings.HasPrefix(branch, "-") {
		return "", fmt.Errorf("%w: GitHub named default branch %q", errNoTrustedBase, branch)
	}
	sha, err := run.Run(ctx, "gh", "api", "repos/"+slug+"/commits/"+branch, "--jq", ".sha")
	if err != nil {
		return "", fmt.Errorf("%w: read the head of %s %s: %w", errNoTrustedBase, slug, branch, err)
	}
	sha = strings.TrimSpace(sha)
	if !isCommitHash(sha) {
		return "", fmt.Errorf("%w: GitHub returned %q as the head of %s", errNoTrustedBase, sha, branch)
	}
	ref := workerBaseRefPrefix + branch
	if _, err := gitenv.RunRefusing(ctx, run, gitenv.Transport, []string{"ext", "fd"}, "-C", top, "fetch", "--no-tags", "origin", "+refs/heads/"+branch+":"+ref); err != nil {
		return "", fmt.Errorf("forgectl: fetch %s from origin: %w", branch, err)
	}
	got, err := gitenv.Run(ctx, run, gitenv.Local, "-C", top, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	if err != nil || strings.TrimSpace(got) != sha {
		// The branch moved between the API read and the fetch, or the fetch
		// did not land what GitHub named. Either way the base is unproven.
		return "", fmt.Errorf("forgectl: %s at origin is %q, GitHub reported %s; retry the launch", branch, strings.TrimSpace(got), sha)
	}
	return sha, nil
}

// isCommitHash reports whether s is a full lowercase SHA-1 or SHA-256 hash.
func isCommitHash(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}
