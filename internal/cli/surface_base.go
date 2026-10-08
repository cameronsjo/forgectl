package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/gitenv"
	"github.com/cameronsjo/forgectl/internal/pr"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// A new worker branch starts at the head of the repository's GitHub default
// branch, not at the checkout's HEAD (forgectl#1061). The checkout's HEAD is
// often stale or on another branch: on sjomba every worker branch had been
// starting at an old local main. forgectl reads the default branch and its
// head from the GitHub API, fetches that branch, checks the fetched commit is
// the one GitHub named, and passes the hash to `git worktree add`.
//
// This is a correctness fix, not a security control. Workers in one
// repository share its .git and are mutually trusting (ADR-0010): an earlier
// worker can rewrite origin, refs, objects or config there and change what
// the next worker starts from, and nothing here can tell.

// workerBaseRefPrefix is where forgectl fetches a base commit. Each launch
// uses its own ref (the commit plus a random suffix), so two launches at the
// same head do not delete each other's ref.
const workerBaseRefPrefix = "refs/forgectl/base/"

// workerBase returns the commit a new worker branch in top starts from: the
// head of the GitHub default branch when origin's URL has host github.com (or
// ssh.github.com), else the checkout's HEAD, as before. An SSH host alias
// for GitHub counts as not GitHub. Each fallback writes one line to note
// naming why, so a stale base is never silent (forgectl#1129). The line names
// only the parsed host, never the raw URL, which can carry a credential.
func workerBase(ctx context.Context, run exec.Runner, top string, note io.Writer) (string, error) {
	fallback := func(why string) (string, error) {
		_, _ = fmt.Fprintf(note, "forgectl: starting from this checkout's HEAD, not GitHub's default branch: %s\n", why)
		return checkoutHead(ctx, run, top)
	}
	origin, err := gitenv.Run(ctx, run, gitenv.Local, "-C", top, "remote", "get-url", "origin")
	if err != nil {
		return fallback("this checkout has no origin remote")
	}
	host, owner, repo, ok := pr.ParseRemoteURL(origin)
	if !ok {
		return fallback("origin's URL is not a form forgectl can read")
	}
	if host != "github.com" && host != "ssh.github.com" {
		return fallback(fmt.Sprintf("origin's host %s is not github.com (an SSH host alias for GitHub counts as not GitHub)", termsafe.QuoteTextMax(host, 40)))
	}
	if !pr.ValidOwnerRepoPart(owner) || !pr.ValidOwnerRepoPart(repo) {
		return fallback("origin's owner or repository name is not one forgectl will use")
	}
	slug := owner + "/" + repo
	branch, err := run.Run(ctx, "gh", "api", "--hostname", "github.com", "repos/"+slug, "--jq", ".default_branch")
	if err != nil {
		return "", fmt.Errorf("forgectl: read %s's default branch from GitHub: %w", slug, err)
	}
	branch = strings.TrimSpace(branch)
	if err := checkDefaultBranch(ctx, run, branch); err != nil {
		return "", err
	}
	sha, err := run.Run(ctx, "gh", "api", "--hostname", "github.com", "repos/"+slug+"/commits/"+escapeRefPath(branch), "--jq", ".sha")
	if err != nil {
		return "", fmt.Errorf("forgectl: read the head of %s %s from GitHub: %w", slug, branch, err)
	}
	sha = strings.TrimSpace(sha)
	if !isCommitHash(sha) {
		return "", fmt.Errorf("forgectl: GitHub returned %q as the head of %s", sha, branch)
	}
	nonce := make([]byte, 4)
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("forgectl: base ref name: %w", err)
	}
	ref := workerBaseRefPrefix + sha + "-" + hex.EncodeToString(nonce)
	if _, err := gitenv.RunRefusing(ctx, run, gitenv.Transport, []string{"ext", "fd"}, "-C", top, "fetch", "--no-tags", "origin", "+refs/heads/"+branch+":"+ref); err != nil {
		return "", fmt.Errorf("forgectl: fetch %s from origin: %w", branch, err)
	}
	got, err := gitenv.Run(ctx, run, gitenv.Local, "-C", top, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	// The ref only brings the commit in; the worktree add references the
	// commit by hash. A failed delete leaves a stray ref and nothing else.
	_, _ = gitenv.Run(ctx, run, gitenv.Local, "-C", top, "update-ref", "-d", ref)
	if err != nil || strings.TrimSpace(got) != sha {
		return "", fmt.Errorf("forgectl: the fetched commit %q is not GitHub's %s head %s (the branch moved, or origin fetches from elsewhere)", strings.TrimSpace(got), branch, sha)
	}
	return sha, nil
}

// checkoutHead is the checkout's HEAD commit, the base for a repository whose
// origin is not on github.com.
func checkoutHead(ctx context.Context, run exec.Runner, top string) (string, error) {
	out, err := gitenv.Run(ctx, run, gitenv.Local, "-C", top, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return "", fmt.Errorf("forgectl: read %s's HEAD: %w", top, err)
	}
	return strings.TrimSpace(out), nil
}

// checkDefaultBranch refuses a branch name git would refuse, or one that
// could change the meaning of the API path or the fetch refspec.
func checkDefaultBranch(ctx context.Context, run exec.Runner, branch string) error {
	if branch == "" || strings.HasPrefix(branch, "-") || strings.ContainsAny(branch, "#%?\\: ") {
		return fmt.Errorf("forgectl: GitHub named default branch %q, which forgectl will not use", branch)
	}
	if _, err := gitenv.Run(ctx, run, gitenv.Local, "check-ref-format", "--branch", branch); err != nil {
		return fmt.Errorf("forgectl: GitHub named default branch %q, which git refuses", branch)
	}
	return nil
}

// escapeRefPath escapes each slash-separated part of a branch name for a
// GitHub API path, keeping the slashes the API reads as part of the ref.
func escapeRefPath(branch string) string {
	parts := strings.Split(branch, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
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
