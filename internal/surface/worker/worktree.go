package worker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/cameronsjo/forgectl/internal/gitenv"
)

// GitRunner is the slice of exec.Runner the worktree helper uses. Every git
// call goes through gitenv under its Local profile, which pins the hardening
// every forgectl git call carries; AddWorktree adds core.hooksPath on top.
type GitRunner = gitenv.Runner

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
	out, err := gitenv.Run(ctx, run, gitenv.Local, "-C", dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("worker: %s is not inside a git checkout: %w", dir, err)
	}
	top := strings.TrimSpace(out)
	common, err := gitenv.Run(ctx, run, gitenv.Local, "-C", dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
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
// other name becomes a new branch at the commit base returns, called only then
// (forgectl#1061 reads it from GitHub rather than the often-stale HEAD).
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
func AddWorktree(ctx context.Context, run GitRunner, top, name, branch string, base func() (string, error)) (Worktree, error) {
	if err := checkWorktreeRequest(ctx, run, name, branch); err != nil {
		return Worktree{}, err
	}
	if err := ensureWorktreeRoot(top); err != nil {
		return Worktree{}, err
	}
	path := WorktreePath(top, name)
	if err := checkWorktreePathFree(path, name); err != nil {
		return Worktree{}, err
	}

	add := []string{"-C", top, "-c", "core.hooksPath=/dev/null", "worktree", "add"}
	switch branchSource(ctx, run, top, branch) {
	case BranchLocal:
		add = append(add, "--", path, branch)
	case BranchOrigin:
		add = append(add, "--track", "-b", branch, "--", path, "origin/"+branch)
	default:
		sha, err := base()
		if err != nil {
			return Worktree{}, err
		}
		if !validCommitID(sha) {
			return Worktree{}, fmt.Errorf("worker: base %q is not a full commit id", sha)
		}
		add = append(add, "-b", branch, "--", path, sha)
	}
	if _, err := gitenv.Run(ctx, run, gitenv.Local, add...); err != nil {
		return Worktree{}, fmt.Errorf("worker: git worktree add: %w", err)
	}

	// The worktree exists from here on. A later failure still returns its
	// path, so the caller records it and does not take the attempt for one
	// that created nothing (a retry would find the path taken).
	created := Worktree{Path: path, Branch: branch}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return created, fmt.Errorf("worker: resolve created worktree: %w", err)
	}
	if filepath.Dir(resolved) != filepath.Dir(path) {
		return created, fmt.Errorf("%w: the worktree resolved to %s, outside %s", ErrUnsafeWorktreeRoot, resolved, filepath.Dir(path))
	}
	head, err := gitenv.Run(ctx, run, gitenv.Local, "-C", resolved, "rev-parse", "HEAD")
	if err != nil {
		return Worktree{Path: resolved, Branch: branch}, fmt.Errorf("worker: read worktree HEAD: %w", err)
	}
	return Worktree{Path: resolved, Branch: branch, Base: strings.TrimSpace(head)}, nil
}

// Where a worker's branch comes from.
const (
	// BranchLocal checks out a local branch that already exists.
	BranchLocal = "local"
	// BranchOrigin makes a local tracking branch from origin/<branch>.
	BranchOrigin = "origin"
	// BranchNew creates the branch at a base commit.
	BranchNew = "new"
)

// checkWorktreeRequest refuses a worker name or branch before anything is
// created. AddWorktree and PlanWorktree both call it, so a preview refuses
// exactly what the launch would.
func checkWorktreeRequest(ctx context.Context, run GitRunner, name, branch string) error {
	if err := ValidName(name); err != nil {
		return err
	}
	if err := precheckBranch(branch); err != nil {
		return err
	}
	if _, err := gitenv.Run(ctx, run, gitenv.Local, "check-ref-format", "--branch", branch); err != nil {
		return fmt.Errorf("%w: git check-ref-format refused it", ErrInvalidBranch)
	}
	return nil
}

// checkWorktreePathFree refuses a worktree path that already exists.
func checkWorktreePathFree(path, name string) error {
	if _, err := os.Lstat(path); err == nil {
		return fmt.Errorf("worker: %s already exists; a worker named %q was started here before", path, name)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("worker: check %s: %w", path, err)
	}
	return nil
}

// branchSource decides where branch comes from, in the order AddWorktree
// uses: a local branch, then origin/<branch>, then a new branch.
func branchSource(ctx context.Context, run GitRunner, top, branch string) string {
	switch {
	case refExists(ctx, run, top, "refs/heads/"+branch):
		return BranchLocal
	case refExists(ctx, run, top, "refs/remotes/origin/"+branch):
		return BranchOrigin
	default:
		return BranchNew
	}
}

// WorktreePlan is what AddWorktree would do for a request.
type WorktreePlan struct {
	// Path is where the worktree would be created. The directory chain above
	// it may not exist yet; unlike Worktree.Path it is not symlink-resolved.
	Path string
	// Branch is the branch it would hold.
	Branch string
	// BranchFrom is BranchLocal, BranchOrigin or BranchNew.
	BranchFrom string
}

// PlanWorktree runs every check AddWorktree runs before it writes and returns
// what it would create, writing nothing: it does not create the worktree root,
// the worktree, or a branch, and a new branch's base commit is not read (that
// is a GitHub call). It refuses what AddWorktree refuses up to that point.
func PlanWorktree(ctx context.Context, run GitRunner, top, name, branch string) (WorktreePlan, error) {
	if err := checkWorktreeRequest(ctx, run, name, branch); err != nil {
		return WorktreePlan{}, err
	}
	if err := checkWorktreeRoot(top); err != nil {
		return WorktreePlan{}, err
	}
	path := WorktreePath(top, name)
	if err := checkWorktreePathFree(path, name); err != nil {
		return WorktreePlan{}, err
	}
	from := branchSource(ctx, run, top, branch)
	if from == BranchLocal {
		at, err := checkedOutAt(ctx, run, top, branch)
		if err != nil {
			return WorktreePlan{}, err
		}
		if at != "" {
			return WorktreePlan{}, fmt.Errorf("%w: branch %q is already checked out at %s; git would refuse a second worktree for it", ErrBranchCheckedOut, branch, at)
		}
	}
	return WorktreePlan{Path: path, Branch: branch, BranchFrom: from}, nil
}

// ErrBranchCheckedOut reports a local branch that another worktree (the main
// checkout included) already has checked out, which git refuses to add again.
var ErrBranchCheckedOut = errors.New("worker: branch is checked out in another worktree")

// checkedOutAt returns the path of the worktree that has branch checked out, or
// "" when none does. It reads `git worktree list --porcelain` and changes
// nothing.
func checkedOutAt(ctx context.Context, run GitRunner, top, branch string) (string, error) {
	out, err := gitenv.Run(ctx, run, gitenv.Local, "-C", top, "worktree", "list", "--porcelain")
	if err != nil {
		return "", fmt.Errorf("worker: list worktrees: %w", err)
	}
	var path string
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "worktree "):
			path = strings.TrimPrefix(line, "worktree ")
		case line == "branch refs/heads/"+branch:
			return path, nil
		}
	}
	return "", nil
}

// checkWorktreeRoot is ensureWorktreeRoot without the creating: it refuses a
// component that exists as a symlink or a non-directory, and stops at the
// first one that does not exist yet.
func checkWorktreeRoot(top string) error { return walkWorktreeRoot(top, false) }

// ensureWorktreeRoot creates <top>/.claude/worktrees, refusing any component
// that already exists as a symlink or a non-directory.
func ensureWorktreeRoot(top string) error { return walkWorktreeRoot(top, true) }

// walkWorktreeRoot is the one walk down <top>/.claude/worktrees behind both:
// with create it makes a missing component, without it a missing component
// ends the walk with nothing wrong found (nothing below it can exist).
func walkWorktreeRoot(top string, create bool) error {
	dir := top
	for _, part := range worktreeDirs {
		dir = filepath.Join(dir, part)
		info, err := os.Lstat(dir)
		if errors.Is(err, os.ErrNotExist) {
			if !create {
				return nil
			}
			if err := os.Mkdir(dir, 0o750); err != nil && !errors.Is(err, os.ErrExist) {
				return fmt.Errorf("worker: create %s: %w", dir, err)
			}
			// Re-check: a racing creator could have put a symlink there.
			info, err = os.Lstat(dir)
		}
		if err != nil {
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
	_, err := gitenv.Run(ctx, run, gitenv.Local, "-C", top, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	return err == nil
}
