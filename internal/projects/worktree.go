package projects

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// Worktree initializes a bare-repo worktree layout for r under the canonical
// Dir/host/owner/name tree (absorbs git-smart-worktree) and returns the path to
// the checked-out worktree. The layout mirrors the bare-clone + worktree idiom:
//
//	<base>/.bare      the bare clone (the shared object store)
//	<base>/.git       a gitdir pointer file → ./.bare
//	<base>/<branch>   the working tree for <branch>
//
// Unlike Clone (which discovers-or-no-ops on an existing dir), Worktree CREATES
// the base dir and refuses if anything is already there — a git-mutating
// operation, so refuse-if-exists is its safety guard. github.com bare-clones go
// through gh (credential handling); everything else bare-clones the SSH URL.
func (c *Client) Worktree(ctx context.Context, r Repo, branch string) (string, error) {
	slog.Debug("Preparing to create worktree.", "host", r.Host, "owner", r.Owner, "name", r.Name, "branch", branch)
	if !validPathSegment(r.Host) || !validPathSegment(r.Owner) || !validPathSegment(r.Name) {
		return "", fmt.Errorf("refusing to create worktree for %s/%s/%s: unsafe path segment", r.Host, r.Owner, r.Name)
	}
	base := canonicalDest(c.Dir, r.Host, r.Owner, r.Name)
	bareDir := filepath.Join(base, ".bare")

	// Create the parent host/owner dirs, then the leaf with os.Mkdir — an atomic
	// create-or-fail. Mkdir refuses an existing base (dir, file, OR symlink) and
	// never follows a symlink, so a pre-placed symlink at base can't redirect the
	// .git pointer write below (closes a same-user TOCTOU when the projects dir is
	// multi-user-writable). This is the git-mutating analog of Clone's origin-match
	// guard: Worktree creates, so it refuses if the leaf already exists.
	if err := os.MkdirAll(filepath.Dir(base), 0o755); err != nil {
		return "", fmt.Errorf("creating worktree parent dirs for %s: %w", base, err)
	}
	if err := os.Mkdir(base, 0o755); err != nil {
		if errors.Is(err, os.ErrExist) {
			return "", fmt.Errorf("%s already exists; refusing to initialize a worktree layout over it", base)
		}
		return "", fmt.Errorf("creating worktree base dir %s: %w", base, err)
	}

	switch r.Host {
	case "github":
		if err := cloneBareRepo(ctx, c.run, r.Owner+"/"+r.Name, bareDir); err != nil {
			slog.Error("Failed to bare-clone from GitHub.", "repo", r.Owner+"/"+r.Name, "dest", bareDir, "error", err)
			return "", err
		}
	default:
		// gitea and any SSH-reachable host: bare-clone the URL directly.
		if err := cloneBareFromURL(ctx, c.run, r.SSHURL, bareDir); err != nil {
			slog.Error("Failed to bare-clone from host.", "host", r.Host, "name", r.Name, "dest", bareDir, "error", err)
			return "", err
		}
	}

	// Point base/.git at the bare repo so `git` run from the base (or any
	// worktree under it) resolves the shared object store. Safe without a temp-
	// rename: base was created by our own os.Mkdir (never a followed symlink),
	// so this write lands inside a dir we exclusively created.
	if err := os.WriteFile(filepath.Join(base, ".git"), []byte("gitdir: ./.bare\n"), 0o644); err != nil {
		return "", fmt.Errorf("writing .git pointer for %s: %w", base, err)
	}

	// A bare clone's default refspec fetches only the cloned branch; widen it so
	// `fetch origin` populates every remote-tracking branch that worktree add needs.
	if _, err := c.run.Run(ctx, "git", "-C", bareDir, "config", "remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*"); err != nil {
		return "", fmt.Errorf("configuring fetch refspec for %s: %w", bareDir, err)
	}
	if _, err := c.run.Run(ctx, "git", "-C", bareDir, "fetch", "origin"); err != nil {
		return "", fmt.Errorf("fetching origin for %s: %w", bareDir, err)
	}

	if branch == "" {
		branch = defaultBranch(ctx, c.run, bareDir)
	}
	if !validBranch(branch) {
		return "", fmt.Errorf("refusing to create worktree for branch %q: unsafe branch name", branch)
	}

	worktreeDir := filepath.Join(base, branch)
	if _, err := c.run.Run(ctx, "git", "-C", bareDir, "worktree", "add", worktreeDir, branch); err != nil {
		// The branch may exist only on the remote — create a local branch tracking
		// origin/<branch> instead.
		if _, ferr := c.run.Run(ctx, "git", "-C", bareDir, "worktree", "add", worktreeDir, "origin/"+branch, "-b", branch); ferr != nil {
			slog.Error("Failed to add worktree.", "dest", worktreeDir, "branch", branch, "error", ferr)
			return "", fmt.Errorf("adding worktree for branch %q: %w", branch, ferr)
		}
	}

	slog.Info("Successfully created worktree.", "host", r.Host, "name", r.Name, "dest", worktreeDir, "branch", branch)
	return worktreeDir, nil
}

// defaultBranch resolves the remote's default branch by parsing the
// `HEAD branch:` line of `git remote show origin`, falling back to "main" when
// the command fails or the line is absent (a bare repo just cloned from a
// non-standard remote).
func defaultBranch(ctx context.Context, run interface {
	Run(context.Context, string, ...string) (string, error)
}, bareDir string) string {
	out, err := run.Run(ctx, "git", "-C", bareDir, "remote", "show", "origin")
	if err == nil {
		for _, line := range strings.Split(out, "\n") {
			line = strings.TrimSpace(line)
			if rest, ok := strings.CutPrefix(line, "HEAD branch:"); ok {
				if b := strings.TrimSpace(rest); b != "" {
					return b
				}
			}
		}
	}
	return "main"
}

// validBranch guards a branch name before it becomes a `git worktree add` argv
// and a filesystem path under the base dir. Unlike validPathSegment it TOLERATES
// internal "/" (e.g. "feature/foo") — a legitimate branch — but rejects, per
// path component: a ".."/"." traversal, a leading "-" (flag injection), and a
// backslash. An empty component catches a leading "/", trailing "/", or "//",
// which covers an absolute path and a trailing slash without a special case.
func validBranch(branch string) bool {
	if branch == "" {
		return false
	}
	for _, part := range strings.Split(branch, "/") {
		if part == "" || part == "." || part == ".." ||
			strings.HasPrefix(part, "-") ||
			strings.Contains(part, `\`) {
			return false
		}
	}
	return true
}
