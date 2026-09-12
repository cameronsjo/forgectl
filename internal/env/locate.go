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

// Target is a --file argument that has already been resolved to its canonical
// path and cleared for use — by the env-file-name allowlist, or by an
// interactive confirmation a human granted against this exact path.
//
// It is a type rather than a (string, bool) pair because the type is the fix
// for a real defect. The previous shape resolved the flag once to build the
// --any-file confirmation prompt, returned only a bool, and let the caller
// resolve the RAW FLAG STRING a second time to decide what to write. The
// window between those two resolutions is the operator's think-time at the
// prompt, and a symlink repointed inside it made the confirmed path and the
// written path two different files: a `link` shown as "notes.txt" in the
// prompt, repointed to `.git/config` before the operator answered, took the
// write — and `core.fsmonitor` is executed by the next `git status`.
//
// So there is exactly one resolution, its result travels as this value, and no
// consumer downstream of the confirmation ever sees the flag string again.
//
// # Why the path fields are unexported
//
// Because otherwise the paragraph above is a claim rather than a property.
// With `Path` and `Root` exported, `Target{Path: "/etc/shadow", Root: "/"}` is
// constructible from any package and passes every downstream check, since the
// consumers read those fields and re-derive nothing — so the allowlist and the
// containment rail would live only in the CLI, and the next domain caller to
// arrive would inherit no rail at all. Unexported, the only Target another
// package can build carries an empty path, which every consumer refuses.
//
// `Exists` stays exported because the CLI legitimately reads it to distinguish
// "about to create" from "about to overwrite", and a forged `Exists` is
// harmless: the write path re-derives existence under the lock and ignores it.
// A Target owns an open descriptor and MUST be closed by whoever resolved it.
type Target struct {
	// path is absolute and symlink-resolved, and exists only to render
	// messages. No filesystem operation goes through it — that is the point of
	// dir below.
	path string

	// base is the final component, the name every operation passes to an *at
	// syscall relative to dir.
	base string

	// Exists reports whether the target existed at resolution time. It is a
	// pre-lock snapshot and deliberately not trusted by the write path; see
	// loadOrEmpty, which re-derives existence under the lock by attempting
	// the open.
	Exists bool

	// root is the resolved repository root the path was proven to live inside,
	// used to render the repo-relative form.
	root string

	// dir is the containing directory, held open from resolution onward. Every
	// read, write, rename, and stat happens relative to this descriptor rather
	// than by re-walking path — see dirPin for why that is the fix and not an
	// optimisation.
	dir *dirPin
}

// Close releases the pinned directory descriptor. Every caller that resolves a
// Target owns closing it; a Target passed onward is borrowed, not transferred.
func (t Target) Close() {
	t.dir.close()
}

// validate refuses a Target that did not come from ResolveTarget. Since every
// field that matters is unexported, the only such value is a literal built
// outside this package, and the missing descriptor is what gives it away.
func (t Target) validate() error {
	if t.path == "" || t.root == "" || t.base == "" || t.dir == nil {
		return errors.New("env: target was not resolved")
	}
	return nil
}

// isEnvFile reports whether the RESOLVED basename passes the allowlist. It is
// checked post-resolution so a symlink named ".env" cannot launder a non-env
// target past it. Unexported because Clear is the only caller and
// IsEnvFileName is already exported for the tests to pin the rule directly.
func (t Target) isEnvFile() bool { return IsEnvFileName(filepath.Base(t.path)) }

// Clear applies the env-file-name allowlist to an already-resolved target,
// returning the refusal when it does not pass.
//
// It lives here rather than in the CLI so the refusal has exactly one wording
// and one implementation: the CLI's resolveEnvTarget calls this and owns only
// the --any-file confirmation that can override it, and the package tests call
// the same function rather than restating its message — a test carrying its
// own copy of the wording would pass while production's drifted.
func (t Target) Clear() error {
	if err := t.validate(); err != nil {
		return err
	}
	if t.isEnvFile() {
		return nil
	}
	return fmt.Errorf("refusing %s: not an env file (want .env, .env.*, or *.env)", t.Rel())
}

// Rel renders the target relative to the repository root — the form every
// message uses, so a machine-specific absolute prefix the caller never typed
// stays out of terminal errors, --json objects, and session transcripts
// (forgectl#481).
//
// The fallback is the basename, never the absolute path: filepath.Rel fails
// only when the two paths share no root, which ResolveTarget's containment
// check already makes impossible — so this is defensive, and it must fail
// toward saying less rather than more.
func (t Target) Rel() string {
	rel, err := filepath.Rel(t.root, t.path)
	if err != nil {
		return filepath.Base(t.path)
	}
	return rel
}

// IsEnvFileName reports whether base — a file's basename, not a full path —
// looks like an env file: exactly ".env", ".env."-prefixed (.env.local,
// .env.prod, .env.staging, .env.example), or ".env"-suffixed (prod.env).
// This is repo-MEMBERSHIP's missing sibling check: ResolveTarget proves a
// --file is inside the repo, never that it's an env file at all. Without this,
// `forgectl env set sshCommand --file .git/config` writes a bare unquoted
// value that is ALSO valid git-config syntax — core.sshCommand — arbitrary
// execution on the next `git fetch`. .envrc (direnv executes it) and
// Makefile (KEY=value is valid make) are equivalent sinks, which is why a
// .git-only blocklist would be insufficient; an allowlist is the only sound
// shape here. Exported so the rule has a single home the tests pin directly
// (locate_test.go) rather than only through the many-branched caller path.
func IsEnvFileName(base string) bool {
	return base == ".env" || strings.HasPrefix(base, ".env.") || strings.HasSuffix(base, ".env")
}

// ResolveTarget resolves a --file argument to a contained, canonical Target.
// It performs resolution and containment ONLY — the env-file-name allowlist is
// the caller's to apply (see internal/cli/env.go's resolveEnvTarget, which
// applies it and owns the --any-file confirmation), because a caller that
// intends to bypass the allowlist must first know what path it is bypassing it
// for.
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
// the repo; Exists reports false so callers know they're about to create,
// not overwrite.
func ResolveTarget(fileFlag, cwd string) (Target, error) {
	if fileFlag == "" {
		return Target{}, errors.New("env: file path required")
	}

	absCwd, err := filepath.Abs(cwd)
	if err != nil {
		return Target{}, fmt.Errorf("resolve cwd: %w", err)
	}

	abs := fileFlag
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(absCwd, fileFlag)
	}

	root, err := findRepoRoot(absCwd)
	if err != nil {
		return Target{}, err
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return Target{}, fmt.Errorf("resolve repository root: %w", err)
	}

	var resolved string
	var exists bool
	if r, rerr := filepath.EvalSymlinks(abs); rerr == nil {
		resolved = r
		exists = true
	} else {
		parent := filepath.Dir(abs)
		realParent, perr := filepath.EvalSymlinks(parent)
		if perr != nil {
			return Target{}, fmt.Errorf("resolve parent directory of %s: %w", filepath.Base(abs), perr)
		}
		resolved = filepath.Join(realParent, filepath.Base(abs))
	}

	if !sandbox.WithinWorkspace(realRoot, resolved) {
		// Names what the caller typed, not the resolved path: the resolved
		// form of an escaping argument is by definition outside the repo, so
		// it is the one path that must not be echoed, and the argument is
		// what the operator can act on. filepath.Base alone would render
		// `--file ../outside/.env` as ".env", which names nothing.
		return Target{}, fmt.Errorf("refusing %s: outside the repository", filepath.Clean(fileFlag))
	}

	t := Target{
		path:   resolved,
		base:   filepath.Base(resolved),
		Exists: exists,
		root:   realRoot,
	}

	// Pin the containing directory now, while the resolution just performed is
	// still current. Everything downstream operates relative to this
	// descriptor, so nothing re-walks the path — which is what makes the
	// --any-file confirmation window safe rather than merely narrow.
	dir, derr := pinDir(filepath.Dir(resolved))
	if derr != nil {
		if errors.Is(derr, errIsSymlink) {
			return Target{}, fmt.Errorf("refusing %s: its directory is not the one that was resolved", t.Rel())
		}
		return Target{}, fmt.Errorf("open directory of %s: %w", t.Rel(), derr)
	}
	t.dir = dir

	if exists {
		_, regular, present, serr := dir.lstat(t.base)
		if serr != nil {
			dir.close()
			return Target{}, fmt.Errorf("stat %s: %w", t.Rel(), serr)
		}
		// A regular-file check here is a resolution-time snapshot; the opens
		// downstream re-assert it against the descriptor, because a FIFO or a
		// directory swapped in after this point is exactly the case the pin
		// exists for.
		//
		// present can be false despite exists: EvalSymlinks succeeded a moment
		// ago and the entry is already gone. Treating that as a non-regular
		// file would refuse with the wrong reason.
		if present && !regular {
			dir.close()
			return Target{}, fmt.Errorf("refusing %s: not a regular file", t.Rel())
		}
	}

	return t, nil
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
