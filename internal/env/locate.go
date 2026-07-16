// locate.go resolves a --file flag into a real, contained path — the
// safety rail every env command runs before touching the filesystem.
package env

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/cameronsjo/forgectl/internal/sandbox"
)

// errNotInRepo is returned when no .git entry is found walking up from cwd.
var errNotInRepo = errors.New("not inside a git repository")

// Locate resolves fileFlag (a --file value, absolute or relative to cwd)
// into its real, symlink-resolved path, refusing anything that isn't
// contained inside the git repository forgectl is running in:
//
//  1. Absolutize fileFlag against cwd.
//  2. Walk up from cwd for a .git entry (directory or file — a worktree's
//     .git is a file) to find the repo root; none found is a refusal.
//  3. Resolve symlinks in the repo root, and in fileFlag itself if it
//     exists (following a symlinked .env to its real target) — or, for a
//     not-yet-existing file, resolve its parent directory instead (which
//     must already exist) and join the file's base name back on.
//  4. Re-check containment of the resolved path inside the resolved root
//     via sandbox.WithinWorkspace — the existing, tested primitive already
//     used by clean/quarantine/pr. This is the check that actually matters:
//     it catches both a literal ../ escape and a symlink (existing file, or
//     an intermediate directory) that resolves outside the repo.
//
// A new file is allowed exactly when its parent directory resolves inside
// the repo; exists reports false so callers know they're about to create,
// not overwrite.
func Locate(fileFlag, cwd string) (realPath string, exists bool, err error) {
	if fileFlag == "" {
		return "", false, errors.New("env: file path required")
	}

	absCwd, err := filepath.Abs(cwd)
	if err != nil {
		return "", false, fmt.Errorf("resolve cwd: %w", err)
	}

	abs := fileFlag
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(absCwd, fileFlag)
	}

	root, err := findRepoRoot(absCwd)
	if err != nil {
		return "", false, err
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", false, fmt.Errorf("resolve repository root %s: %w", root, err)
	}

	real := ""
	if r, rerr := filepath.EvalSymlinks(abs); rerr == nil {
		real = r
		exists = true
	} else {
		parent := filepath.Dir(abs)
		realParent, perr := filepath.EvalSymlinks(parent)
		if perr != nil {
			return "", false, fmt.Errorf("resolve parent directory of %s: %w", filepath.Base(abs), perr)
		}
		real = filepath.Join(realParent, filepath.Base(abs))
	}

	if !sandbox.WithinWorkspace(realRoot, real) {
		return "", false, fmt.Errorf("refusing %s: outside repository %s", filepath.Base(abs), realRoot)
	}

	return real, exists, nil
}

// findRepoRoot walks up from start looking for a .git entry — a directory
// for an ordinary repo, a file for a worktree (its .git is a "gitdir: …"
// pointer file). No up-walk helper exists elsewhere in forgectl; this is
// genuinely new.
func findRepoRoot(start string) (string, error) {
	dir := start
	for {
		gitPath := filepath.Join(dir, ".git")
		if fi, err := os.Stat(gitPath); err == nil && (fi.IsDir() || fi.Mode().IsRegular()) {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errNotInRepo
		}
		dir = parent
	}
}
