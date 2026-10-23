package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"github.com/cameronsjo/forgectl/internal/gitenv"
	"github.com/cameronsjo/forgectl/internal/termsafe"
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
	// Submodules lists what makes git refuse to remove the worktree as one
	// holding submodules: each populated gitlink's path, and the worktree's
	// own modules directory when it has one.
	Submodules []string
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
	if len(f.Submodules) > 0 {
		out = append(out, fmt.Sprintf("it holds submodules (%s), and git worktree remove refuses a worktree that does", submoduleList(f.Submodules)))
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

// maxNamedSubmodules caps how many submodules a blocker names. The paths are
// the worker's to choose, so each is quoted and cut short as well.
const maxNamedSubmodules = 3

func submoduleList(subs []string) string {
	var named []string
	for _, p := range subs[:min(len(subs), maxNamedSubmodules)] {
		named = append(named, termsafe.QuotePathMax(p, 80))
	}
	if more := len(subs) - len(named); more > 0 {
		named = append(named, fmt.Sprintf("and %d more", more))
	}
	return strings.Join(named, ", ")
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
	common, err := gitenv.Run(ctx, run, gitenv.Local, "-C", top, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return WorktreeFacts{}, fmt.Errorf("worker: read the common git dir of %s: %w", top, err)
	}
	gitDir, err := checkGitFile(strings.TrimSpace(common), path)
	if err != nil {
		return WorktreeFacts{}, err
	}

	// Every call below runs against gitDir, the directory checkGitFile found
	// the gitfile naming, and never reads path/.git again: a process that
	// outlives the workspace could swap the gitfile after the check (#1304).
	git := func(args ...string) (string, error) {
		return gitenv.Run(ctx, run, gitenv.Local, append(gitenv.PinnedArgs(path, gitDir), args...)...)
	}
	if f.Submodules, err = submodulesIn(path, gitDir, git); err != nil {
		return WorktreeFacts{}, err
	}
	// status re-hashes stat-dirty files through any filter driver the
	// worktree's .gitattributes names, and the worktree is the worker's to
	// write; RunUnfiltered blanks every driver so none runs as the operator.
	// Refreshing a stat-dirty entry rewrites the index, which fires
	// post-index-change; RunUnfiltered pins hooks off as well. --no-optional-locks
	// (GIT_OPTIONAL_LOCKS=0) stops status from taking index.lock to write that
	// refresh back: this inspection also backs `surface close --dry-run`, which
	// must change nothing, and the answer needs no refreshed index.
	//
	// A submodule keeps the worktree whatever status says, and RunUnfiltered
	// would refuse one whose .git it cannot read, failing close without the
	// reason, so status runs only in a worktree submodulesIn found none in.
	// One populated after that is the worker's too: --ignore-submodules=dirty
	// keeps status from running a child git status in it, which would read a
	// .git that RunUnfiltered's listing never saw and could name drivers
	// nobody blanked (#1338). It still reports a gitlink whose commit moved or
	// is staged, which =all hides (measured on git 2.43: none and untracked
	// run the child, dirty and all do not).
	if len(f.Submodules) == 0 {
		if f.Status, err = gitenv.RunUnfilteredPinned(ctx, run, gitenv.Bin, path, gitDir, "--no-optional-locks", "status", "--porcelain", "--ignored", "--ignore-submodules=dirty"); err != nil {
			return WorktreeFacts{}, fmt.Errorf("worker: git status in %s: %w", path, err)
		}
	}
	// An error from either lookup below is read as "detached" or "no upstream".
	// Both only add blockers or fall back to the stricter base count, so a
	// git failure here can keep a worktree but never remove one.
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
	// worktree remove runs its own dirty check in path, so path's filter
	// drivers are blanked too. RunUnfilteredAlso pins hooks off. The listing
	// for path stays unpinned on purpose: git's own check runs with
	// GIT_DIR=<path>/.git after validating that gitfile itself, and a pinned
	// listing could read a different repository than that check does (#1304).
	if _, err := gitenv.RunUnfilteredAlso(ctx, run, gitenv.Bin, top, []string{path}, "worktree", "remove", "--", path); err != nil {
		return fmt.Errorf("worker: git worktree remove %s: %w", path, err)
	}
	return nil
}

// submodulesIn returns what git worktree remove counts as submodules in the
// worktree at path: its git dir's modules directory, and each gitlink in its
// index whose path holds a .git. git refuses on either ("working trees
// containing submodules cannot be moved or removed"), so close keeps the
// worktree and says why instead of failing at the remove. git runs a git
// call pinned to the worktree's checked git dir.
func submodulesIn(path, gitDir string, git func(...string) (string, error)) ([]string, error) {
	var out []string
	// git's is_directory follows a symbolic link, so this stat does too.
	modules := filepath.Join(gitDir, "modules")
	if info, err := os.Stat(modules); err == nil && info.IsDir() {
		out = append(out, modules)
	}
	index, err := git("ls-files", "-z", "--stage")
	if err != nil {
		return nil, fmt.Errorf("worker: list the index of %s: %w", path, err)
	}
	for entry := range strings.SplitSeq(index, "\x00") {
		meta, p, ok := strings.Cut(entry, "\t")
		// A conflicted gitlink has an entry per stage; name it once.
		if !ok || !strings.HasPrefix(meta, "160000 ") || slices.Contains(out, p) {
			continue
		}
		// Anything at <gitlink>/.git counts, where git counts only a .git
		// naming a repository, and so does any error but its absence: a
		// worker's empty file, dangling link or symlink loop keeps the
		// worktree with a reason, rather than failing close or leaving the
		// decision to git's own check at remove.
		_, err := os.Lstat(filepath.Join(path, filepath.FromSlash(p), ".git"))
		if err == nil || (!errors.Is(err, os.ErrNotExist) && !errors.Is(err, syscall.ENOTDIR)) {
			out = append(out, p)
		}
	}
	return out, nil
}

// maxGitFileBytes bounds the gitfile checkGitFile reads: "gitdir: " and a
// path under the common git dir.
const maxGitFileBytes = 4096

// checkGitFile refuses a worktree whose .git is not the gitfile git wrote:
// a regular file naming a directory under <common git dir>/worktrees. The worktree
// is the worker's to write, and a .git it pointed elsewhere would hand git a
// config the worker wrote. The directory must also be this worktree's own:
// its gitdir file, which git writes back to the worktree's .git, must name
// path, or the calls pinned to it would read another worktree's branch and
// stash (#1338). It returns that directory, resolved, for the caller to pin
// its git calls to.
//
// The read never follows a symbolic link and never blocks (gitenv.OpenGitfile),
// is checked on the open handle, and stops after maxGitFileBytes+1 bytes: a
// process that outlives the workspace could swap a FIFO or an endless file in
// after the Lstat, and close must refuse it rather than hang (#1079).
func checkGitFile(common, path string) (string, error) {
	gitfile := filepath.Join(path, ".git")
	info, err := os.Lstat(gitfile)
	if err != nil {
		return "", fmt.Errorf("worker: check %s: %w", gitfile, err)
	}
	if !info.Mode().IsRegular() || info.Size() > maxGitFileBytes {
		return "", fmt.Errorf("%w: %s is not a gitfile", ErrUnsafeWorktreeRoot, gitfile)
	}
	data, err := readGitFile(gitfile)
	if err != nil {
		return "", err
	}
	dir, ok := strings.CutPrefix(strings.TrimSpace(string(data)), "gitdir: ")
	if ok && !filepath.IsAbs(dir) {
		// worktree.useRelativePaths writes the gitdir relative to the
		// worktree, and git resolves it from there, not from our cwd.
		dir = filepath.Join(path, dir)
	}
	if base := filepath.Base(dir); base == "." || base == ".." {
		ok = false
	}
	admin, aerr := filepath.EvalSymlinks(filepath.Join(common, "worktrees"))
	parent, perr := filepath.EvalSymlinks(filepath.Dir(dir))
	if !ok || aerr != nil || perr != nil || parent != admin {
		return "", fmt.Errorf("%w: %s points outside %s", ErrUnsafeWorktreeRoot, gitfile, admin)
	}
	gitDir := filepath.Join(admin, filepath.Base(dir))
	if err := checkBackLink(gitDir, path); err != nil {
		return "", err
	}
	return gitDir, nil
}

// checkBackLink refuses gitDir unless its gitdir file names path/.git.
// worktree.useRelativePaths writes that path relative to gitDir.
func checkBackLink(gitDir, path string) error {
	link := filepath.Join(gitDir, "gitdir")
	data, err := readGitFile(link)
	if err != nil {
		return err
	}
	back := strings.TrimSpace(string(data))
	if back != "" && !filepath.IsAbs(back) {
		back = filepath.Join(gitDir, back)
	}
	want, werr := filepath.EvalSymlinks(path)
	got, gerr := filepath.EvalSymlinks(filepath.Dir(back))
	if back == "" || filepath.Base(back) != ".git" || werr != nil || gerr != nil || got != want {
		return fmt.Errorf("%w: %s names %s, which %s does not name back", ErrUnsafeWorktreeRoot, filepath.Join(path, ".git"), gitDir, link)
	}
	return nil
}

// readGitFile reads the gitfile at gitfile, already found a small regular
// file by Lstat, through a no-follow, non-blocking open, and refuses it
// unless the open handle is still a regular file of at most maxGitFileBytes.
func readGitFile(gitfile string) ([]byte, error) {
	f, err := gitenv.OpenGitfile(gitfile)
	switch {
	case errors.Is(err, syscall.ELOOP):
		// A symbolic link swapped in after the Lstat. A FIFO opens without
		// blocking and is refused by the mode check below.
		return nil, fmt.Errorf("%w: %s is a symbolic link, not a gitfile", ErrUnsafeWorktreeRoot, gitfile)
	case err != nil:
		// Removed after the Lstat, unreadable, or any other open failure: no
		// gitfile was read, so the worktree is kept, but nothing says the
		// worktree is hostile.
		return nil, fmt.Errorf("worker: open %s: %w", gitfile, err)
	}
	defer f.Close() //nolint:errcheck // read-only
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("worker: check %s: %w", gitfile, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: %s is not a gitfile", ErrUnsafeWorktreeRoot, gitfile)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxGitFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("worker: read %s: %w", gitfile, err)
	}
	if len(data) > maxGitFileBytes {
		return nil, fmt.Errorf("%w: %s is not a gitfile", ErrUnsafeWorktreeRoot, gitfile)
	}
	return data, nil
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
