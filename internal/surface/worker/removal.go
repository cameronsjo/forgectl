package worker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/cameronsjo/forgectl/internal/gitenv"
)

// `surface close` removes a worker's worktree only when doing so cannot lose
// work. WorktreeFacts is what git says about the worktree; RemovalBlockers is
// the decision, kept pure so every check has a test of its own.

// WorktreeFacts is what InspectWorktree read from git.
type WorktreeFacts struct {
	// Path is the worktree directory, from `git worktree list`, never from a
	// ledger row.
	Path string
	// Listed reports that git lists Path as a worktree. When it does not and
	// OnDisk is false, there is nothing to remove.
	Listed bool
	// OnDisk reports that something exists at Path.
	OnDisk bool
	// Status is `git status --porcelain --ignored` output.
	Status string
	// Branch is the checked-out branch, or "" for a detached HEAD.
	Branch string
	// Upstream is the branch's upstream, or "" for none.
	Upstream string
	// Ahead counts commits on HEAD missing from the upstream, or, with no
	// upstream, beyond the base the worktree started at. -1 when the base is
	// unknown.
	Ahead int
	// Stashes counts stash entries made on Branch.
	Stashes int
}

// Present reports whether there is a worktree to remove.
func (f WorktreeFacts) Present() bool { return f.Listed || f.OnDisk }

// RemovalBlockers lists every reason the worktree must be kept, in the order
// the plan names them. An empty list means removal loses nothing.
func RemovalBlockers(f WorktreeFacts) []string {
	if !f.Listed {
		// Something sits at the path that git does not call a worktree. It is
		// not forgectl's to delete.
		return []string{fmt.Sprintf("%s exists but git does not list it as a worktree", f.Path)}
	}
	var out []string
	if strings.TrimSpace(f.Status) != "" {
		out = append(out, "it has uncommitted, untracked, or ignored files (git status --porcelain --ignored is not empty)")
	}
	if f.Branch == "" {
		out = append(out, "HEAD is detached, so its commits are reachable only from the worktree's own reflog")
	}
	switch {
	case f.Ahead < 0:
		out = append(out, "the branch has no upstream and the worktree's base commit is unknown, so new commits cannot be ruled out")
	case f.Ahead > 0 && f.Upstream != "":
		out = append(out, fmt.Sprintf("the branch has %d commit(s) not on %s", f.Ahead, f.Upstream))
	case f.Ahead > 0:
		out = append(out, fmt.Sprintf("the branch has no upstream and %d commit(s) beyond the worktree's base", f.Ahead))
	}
	if f.Stashes > 0 {
		out = append(out, fmt.Sprintf("%d stash entr(y/ies) were made on branch %s", f.Stashes, f.Branch))
	}
	return out
}

// InspectWorktree reads the facts RemovalBlockers decides on, for the worker
// named name under top. base is the commit the worktree started at (the
// ledger's Base), used only when the branch has no upstream; a value that is
// not a full commit id counts as unknown.
//
// The path is WorktreePath(top, name), and it must appear in `git worktree
// list`; the ledger's stored path is never used.
func InspectWorktree(ctx context.Context, run GitRunner, top, name, base string) (WorktreeFacts, error) {
	if err := ValidName(name); err != nil {
		return WorktreeFacts{}, err
	}
	path := WorktreePath(top, name)
	f := WorktreeFacts{Path: path, Ahead: -1}

	listed, err := ListedWorktrees(ctx, run, top)
	if err != nil {
		return WorktreeFacts{}, err
	}
	f.Listed = listed[path]
	if _, err := os.Lstat(path); err == nil {
		f.OnDisk = true
	} else if !errors.Is(err, os.ErrNotExist) {
		return WorktreeFacts{}, fmt.Errorf("worker: check %s: %w", path, err)
	}
	if !f.Listed {
		return f, nil
	}

	git := func(args ...string) (string, error) {
		return gitenv.Run(ctx, run, gitenv.Local, append([]string{"-C", path}, args...)...)
	}
	if f.Status, err = git("status", "--porcelain", "--ignored"); err != nil {
		return WorktreeFacts{}, fmt.Errorf("worker: git status in %s: %w", path, err)
	}
	if b, err := git("symbolic-ref", "-q", "--short", "HEAD"); err == nil {
		f.Branch = strings.TrimSpace(b)
	}
	if u, err := git("rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{upstream}"); err == nil {
		f.Upstream = strings.TrimSpace(u)
	}
	var since string
	switch {
	case f.Upstream != "":
		since = "@{upstream}"
	case validCommitID(base):
		since = base
	}
	if since != "" {
		out, err := git("rev-list", "--count", since+"..HEAD")
		if err != nil {
			return WorktreeFacts{}, fmt.Errorf("worker: count commits beyond %s in %s: %w", since, path, err)
		}
		if f.Ahead, err = strconv.Atoi(strings.TrimSpace(out)); err != nil {
			return WorktreeFacts{}, fmt.Errorf("worker: git rev-list --count printed %q", strings.TrimSpace(out))
		}
	}
	if f.Branch != "" {
		// The stash is shared by every worktree of the repo, and an entry's
		// subject names the branch it was made on.
		out, err := gitenv.Run(ctx, run, gitenv.Local, "-C", top, "stash", "list", "--format=%gs")
		if err != nil {
			return WorktreeFacts{}, fmt.Errorf("worker: git stash list: %w", err)
		}
		f.Stashes = stashesOn(out, f.Branch)
	}
	return f, nil
}

// RemoveWorktree runs `git worktree remove` without --force, so git refuses
// on its own if the worktree changed since it was inspected.
func RemoveWorktree(ctx context.Context, run GitRunner, top, path string) error {
	if filepath.Dir(path) != filepath.Join(append([]string{top}, worktreeDirs...)...) {
		return fmt.Errorf("%w: %s is not directly under %s", ErrUnsafeWorktreeRoot, path, filepath.Join(append([]string{top}, worktreeDirs...)...))
	}
	if _, err := gitenv.Run(ctx, run, gitenv.Local, "-C", top, "-c", "core.hooksPath=/dev/null", "worktree", "remove", "--", path); err != nil {
		return fmt.Errorf("worker: git worktree remove %s: %w", path, err)
	}
	return nil
}

// ListedWorktrees returns the paths `git worktree list --porcelain` names.
func ListedWorktrees(ctx context.Context, run GitRunner, top string) (map[string]bool, error) {
	out, err := gitenv.Run(ctx, run, gitenv.Local, "-C", top, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, fmt.Errorf("worker: git worktree list: %w", err)
	}
	return parseWorktreeList(out), nil
}

func parseWorktreeList(out string) map[string]bool {
	paths := map[string]bool{}
	for line := range strings.SplitSeq(out, "\n") {
		if p, ok := strings.CutPrefix(line, "worktree "); ok {
			paths[p] = true
		}
	}
	return paths
}

// stashesOn counts stash subjects made on branch: git writes "WIP on
// <branch>: …" for a bare stash and "On <branch>: …" for one with a message.
func stashesOn(list, branch string) int {
	n := 0
	for line := range strings.SplitSeq(list, "\n") {
		if strings.HasPrefix(line, "WIP on "+branch+": ") || strings.HasPrefix(line, "On "+branch+": ") {
			n++
		}
	}
	return n
}

func validCommitID(s string) bool {
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
