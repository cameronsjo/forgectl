// locate.go resolves a --file flag into a real, contained path — the
// safety rail every env command runs before touching the filesystem.
package env

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/cameronsjo/forgectl/internal/sandbox"
)

// errNotInRepo is returned when no .git entry is found walking up from cwd.
var errNotInRepo = errors.New("not inside a git repository")

// IsEnvFileName reports whether base — a file's basename, not a full path —
// looks like an env file: exactly ".env", ".env."-prefixed (.env.local,
// .env.prod, .env.staging, .env.example), or ".env"-suffixed (prod.env).
// This is repo-MEMBERSHIP's missing sibling check: Locate proves a --file
// is inside the repo, never that it's an env file at all. Without this,
// `forgectl env set sshCommand --file .git/config` writes a bare unquoted
// value that is ALSO valid git-config syntax — core.sshCommand — arbitrary
// execution on the next `git fetch`. .envrc (direnv executes it) and
// Makefile (KEY=value is valid make) are equivalent sinks, which is why a
// .git-only blocklist would be insufficient; an allowlist is the only sound
// shape here. Exported so the rule has a single home the tests pin directly
// (locate_test.go) rather than only through Locate's many-branched path.
func IsEnvFileName(base string) bool {
	return base == ".env" || strings.HasPrefix(base, ".env.") || strings.HasSuffix(base, ".env")
}

// ResolveTarget performs Locate's resolution steps — everything except the
// env-file-name allowlist — so a caller can learn the CANONICAL path a
// --file argument resolves to before deciding whether an allowlist bypass
// even needs a confirmation (see internal/cli/env.go's
// resolveAllowAnyFile, which binds its --any-file prompt to this return
// value rather than the raw, possibly-symlinked argument). Locate itself is
// a thin wrapper: call ResolveTarget, then apply the name check — ONE
// resolution implementation, not two.
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
//     used by clean/quarantine/pr. This catches both a literal ../ escape
//     and a symlink (existing file, or an intermediate directory) that
//     resolves outside the repo.
//  5. For an EXISTING target, refuse anything that isn't a regular file.
//     filepath.EvalSymlinks happily resolves a directory or a FIFO; handing
//     either to parseFile's os.Open would either error strangely (a
//     directory) or block forever (a FIFO with no writer). A not-yet-
//     existing target (the set-new-file path) has nothing to stat, so it's
//     unaffected.
//
// A new file is allowed exactly when its parent directory resolves inside
// the repo; exists reports false so callers know they're about to create,
// not overwrite.
func ResolveTarget(fileFlag, cwd string) (resolved string, exists bool, err error) {
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

	resolved = ""
	if r, rerr := filepath.EvalSymlinks(abs); rerr == nil {
		resolved = r
		exists = true
	} else {
		parent := filepath.Dir(abs)
		realParent, perr := filepath.EvalSymlinks(parent)
		if perr != nil {
			return "", false, fmt.Errorf("resolve parent directory of %s: %w", filepath.Base(abs), perr)
		}
		resolved = filepath.Join(realParent, filepath.Base(abs))
	}

	if !sandbox.WithinWorkspace(realRoot, resolved) {
		return "", false, fmt.Errorf("refusing %s: outside repository %s", filepath.Base(abs), realRoot)
	}

	if exists {
		fi, serr := os.Lstat(resolved)
		if serr != nil {
			return "", false, fmt.Errorf("stat %s: %w", filepath.Base(resolved), serr)
		}
		if !fi.Mode().IsRegular() {
			return "", false, fmt.Errorf("refusing %s: not a regular file", filepath.Base(resolved))
		}
	}

	return resolved, exists, nil
}

// Locate resolves fileFlag via ResolveTarget, then — unless allowAnyFile is
// true — refuses anything whose RESOLVED basename isn't IsEnvFileName,
// checked post-symlink-resolution so a symlink named ".env" can't launder a
// non-env target past this check. allowAnyFile is the CLI's --any-file
// escape hatch, only ever set true after an interactive confirmation (see
// internal/cli/env.go's resolveAllowAnyFile).
func Locate(fileFlag, cwd string, allowAnyFile bool) (realPath string, exists bool, err error) {
	resolved, exists, err := ResolveTarget(fileFlag, cwd)
	if err != nil {
		return "", false, err
	}

	if !allowAnyFile && !IsEnvFileName(filepath.Base(resolved)) {
		return "", false, fmt.Errorf("refusing %s: not an env file (want .env, .env.*, or *.env)", filepath.Base(resolved))
	}

	return resolved, exists, nil
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
