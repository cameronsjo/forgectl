package worker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// GitRunner is the slice of exec.Runner the worktree helper uses.
type GitRunner interface {
	Run(ctx context.Context, name string, args ...string) (string, error)
}

// Worktree is what AddWorktree created.
type Worktree struct {
	// Path is the worktree directory, symlinks resolved.
	Path string
	// Branch is the branch checked out in it.
	Branch string
	// Base is the commit the worktree started at. `close` (T4) uses it to tell
	// a branch with new work from one with none.
	Base string
}

// ErrUnsafeWorktreeRoot reports a worktree root forgectl will not create under.
var ErrUnsafeWorktreeRoot = errors.New("worker: worktree root is unsafe")

// worktreeDirs is the path below the repo top where worker worktrees live.
var worktreeDirs = []string{".claude", "worktrees"}

// RepoTop returns the main checkout's top level for the git repo containing
// dir, with symlinks resolved, so every later path check compares real paths.
//
// From inside a linked worktree (a worker's own, say) --show-toplevel names
// that worktree. Using it would key a second ledger, so name collisions with
// the main checkout's workers would go unseen, and would nest new worktrees
// inside the worker's. When the common git dir is a `.git` directory, its
// parent is the main checkout, and that is the top. A bare-repo layout has no
// main checkout, so it keeps the worktree's own top.
func RepoTop(ctx context.Context, run GitRunner, dir string) (string, error) {
	out, err := run.Run(ctx, "git", "-C", dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("worker: %s is not inside a git checkout: %w", dir, err)
	}
	top := strings.TrimSpace(out)
	common, err := run.Run(ctx, "git", "-C", dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", fmt.Errorf("worker: read the common git dir of %s: %w", dir, err)
	}
	if common = strings.TrimSpace(common); filepath.Base(common) == ".git" {
		top = filepath.Dir(common)
	}
	if !filepath.IsAbs(top) {
		return "", fmt.Errorf("worker: git reported a non-absolute top level for %s", dir)
	}
	resolved, err := filepath.EvalSymlinks(top)
	if err != nil {
		return "", fmt.Errorf("worker: resolve repo top: %w", err)
	}
	return resolved, nil
}

// WorktreePath is where the worker named name gets its worktree.
func WorktreePath(top, name string) string {
	return filepath.Join(append(append([]string{top}, worktreeDirs...), name)...)
}

// AddWorktree creates a worktree for branch at WorktreePath(top, name).
//
// Branch selection, in order: an existing local branch is checked out as is; a
// branch that exists only as origin/<branch> gets a local tracking branch; any
// other name becomes a new branch from the checkout's HEAD.
//
// Every git call that writes runs with core.hooksPath=/dev/null. A branch
// fetched from a remote carries whatever hook-runner config its tree points at
// (husky, .githooks), and a post-checkout hook would run before any folder-trust
// dialog had a chance to ask.
//
// The directories between top and the worktree are refused if any is a
// symlink: a repo can commit .claude as a symlink, and following it would put
// the worktree, and the worker, outside the repo. The created path is checked
// again after git runs, against the real top.
func AddWorktree(ctx context.Context, run GitRunner, top, name, branch string) (Worktree, error) {
	if err := ValidName(name); err != nil {
		return Worktree{}, err
	}
	if err := precheckBranch(branch); err != nil {
		return Worktree{}, err
	}
	if _, err := run.Run(ctx, "git", "check-ref-format", "--branch", branch); err != nil {
		return Worktree{}, fmt.Errorf("%w: git check-ref-format refused it", ErrInvalidBranch)
	}
	if err := ensureWorktreeRoot(top); err != nil {
		return Worktree{}, err
	}
	path := WorktreePath(top, name)
	if _, err := os.Lstat(path); err == nil {
		return Worktree{}, fmt.Errorf("worker: %s already exists; a worker named %q was started here before", path, name)
	} else if !errors.Is(err, os.ErrNotExist) {
		return Worktree{}, fmt.Errorf("worker: check %s: %w", path, err)
	}

	add := []string{"-C", top, "-c", "core.hooksPath=/dev/null", "worktree", "add"}
	switch {
	case refExists(ctx, run, top, "refs/heads/"+branch):
		add = append(add, "--", path, branch)
	case refExists(ctx, run, top, "refs/remotes/origin/"+branch):
		add = append(add, "--track", "-b", branch, "--", path, "origin/"+branch)
	default:
		add = append(add, "-b", branch, "--", path, "HEAD")
	}
	if _, err := run.Run(ctx, "git", add...); err != nil {
		return Worktree{}, fmt.Errorf("worker: git worktree add: %w", err)
	}

	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return Worktree{}, fmt.Errorf("worker: resolve created worktree: %w", err)
	}
	if filepath.Dir(resolved) != filepath.Dir(path) {
		return Worktree{}, fmt.Errorf("%w: the worktree resolved to %s, outside %s", ErrUnsafeWorktreeRoot, resolved, filepath.Dir(path))
	}
	base, err := run.Run(ctx, "git", "-C", resolved, "rev-parse", "HEAD")
	if err != nil {
		return Worktree{}, fmt.Errorf("worker: read worktree HEAD: %w", err)
	}
	return Worktree{Path: resolved, Branch: branch, Base: strings.TrimSpace(base)}, nil
}

// ensureWorktreeRoot creates <top>/.claude/worktrees, refusing any component
// that already exists as a symlink or a non-directory.
func ensureWorktreeRoot(top string) error {
	dir := top
	for _, part := range worktreeDirs {
		dir = filepath.Join(dir, part)
		info, err := os.Lstat(dir)
		switch {
		case errors.Is(err, os.ErrNotExist):
			if err := os.Mkdir(dir, 0o750); err != nil && !errors.Is(err, os.ErrExist) {
				return fmt.Errorf("worker: create %s: %w", dir, err)
			}
			// Re-check: a racing creator could have put a symlink there.
			info, err = os.Lstat(dir)
			if err != nil {
				return fmt.Errorf("worker: check %s: %w", dir, err)
			}
		case err != nil:
			return fmt.Errorf("worker: check %s: %w", dir, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: %s is a symlink", ErrUnsafeWorktreeRoot, dir)
		}
		if !info.IsDir() {
			return fmt.Errorf("%w: %s is not a directory", ErrUnsafeWorktreeRoot, dir)
		}
	}
	return nil
}

func refExists(ctx context.Context, run GitRunner, top, ref string) bool {
	_, err := run.Run(ctx, "git", "-C", top, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	return err == nil
}
