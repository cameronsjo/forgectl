package pr

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// workspaceAvailability classifies a RECORDED workspace pathname against the
// live filesystem. It is the actionability half of the #212 split: a record can
// be perfectly valid while its workspace is long gone.
//
// The zero value is Invalid ON PURPOSE. Every path that reaches a decision must
// have been set deliberately; a forgotten assignment fails closed rather than
// authorizing an action.
type workspaceAvailability uint8

const (
	// workspaceAvailabilityInvalid is the fail-closed zero value: the recorded
	// workspace could not be shown to be either a live sandbox or a clean
	// lexical absence. It authorizes NOTHING — not an action, not a deletion.
	workspaceAvailabilityInvalid workspaceAvailability = iota
	// workspaceAvailabilityLive means the recorded path is a real sandbox
	// directory. Only this state may construct an actionable Session.
	workspaceAvailabilityLive
	// workspaceAvailabilityMissing means the FINAL path component is
	// lexically absent beneath a parent directory that itself exists and
	// resolves. Only this state may authorize a breadcrumb-only stale unlink.
	workspaceAvailabilityMissing
)

// workspaceMissingError reports the one narrow state that authorizes stale
// removal: the recorded workspace's final component simply is not there.
//
// It is unexported and reached through errors.As, never constructed by
// callers. It wraps the originating fs.ErrNotExist so errors.Is keeps working
// for code that only cares that something was absent — but a bare
// errors.Is(err, fs.ErrNotExist) is NOT a substitute for errors.As here: a
// dangling final symlink, an unresolvable parent, and a mid-resolution race
// all surface the same underlying OS cause while being firmly INVALID. Using
// errors.Is to gate remediation (or worse, deletion) would hand those states
// the authority this type exists to withhold.
type workspaceMissingError struct {
	path string
	err  error
}

func (e *workspaceMissingError) Error() string {
	return fmt.Sprintf("workspace %q no longer exists", e.path)
}

func (e *workspaceMissingError) Unwrap() error { return e.err }

// Filesystem seams. Production wires the real calls; classifier tests inject
// deterministic behavior for states that cannot be staged portably (EACCES on
// a parent, a generic I/O error, a disappearance racing between two syscalls).
// Tests must restore these and must not run in parallel while overriding them.
var (
	fsLstat        = os.Lstat
	fsStat         = os.Stat
	fsEvalSymlinks = filepath.EvalSymlinks
)

// classifyWorkspace decides whether a recorded workspace pathname is live,
// lexically missing, or invalid. It is the ONLY place that distinction is
// made, and it is deliberately biased toward Invalid: Missing is granted only
// when the absence is unambiguous, because Missing is what authorizes deleting
// a breadcrumb.
//
// The order is load-bearing:
//
//  1. Shape. An absolute, already-clean pathname. Record validation checks
//     absoluteness too; repeating it here is defense in depth, because this
//     function is what a deletion decision hangs on. Cleanliness is required
//     so no "/a/../b" form can make the parent check below inspect a
//     different directory than the one the final component actually sits in.
//  2. Lstat, which does NOT follow a final symlink — so a dangling link is
//     seen as a present entry, not as an absence.
//  3. Entry present -> defer entirely to validateWorkspace (Stat, directory
//     check, fail-closed EvalSymlinks, resolved sandbox prefix). Live on
//     success; INVALID on anything else. A dangling final symlink, a
//     non-directory, a bad resolved prefix, a permission or I/O error, a
//     Linux procfs magic link, and a resolution race all land here, and none
//     of them is ever Missing.
//  4. Entry absent -> the parent must exist, be a directory, and resolve.
//     Only then is the absence clean enough to call Missing.
//  5. Anything else is Invalid. The Missing sentinel is built ONLY from the
//     Lstat ErrNotExist above — never from a later Stat or EvalSymlinks
//     failure, which would let an unrelated error manufacture delete
//     authority.
func classifyWorkspace(path string) (workspaceAvailability, error) {
	if !filepath.IsAbs(path) {
		return workspaceAvailabilityInvalid, fmt.Errorf("workspace %q must be an absolute path", path)
	}
	if filepath.Clean(path) != path {
		return workspaceAvailabilityInvalid, fmt.Errorf("workspace %q is not a clean path", path)
	}

	_, lerr := fsLstat(path)
	switch {
	case lerr == nil:
		if err := validateWorkspace(path); err != nil {
			return workspaceAvailabilityInvalid, err
		}
		return workspaceAvailabilityLive, nil
	case !errors.Is(lerr, fs.ErrNotExist):
		return workspaceAvailabilityInvalid, fmt.Errorf("workspace %q could not be examined: %w", path, lerr)
	}

	// The final component is lexically absent. Confirm the absence is a clean
	// one — a real, resolvable parent directory — before calling it Missing.
	parent := filepath.Dir(path)
	info, err := fsStat(parent)
	if err != nil {
		return workspaceAvailabilityInvalid,
			fmt.Errorf("workspace %q is absent and its parent %q could not be examined: %w", path, parent, err)
	}
	if !info.IsDir() {
		return workspaceAvailabilityInvalid,
			fmt.Errorf("workspace %q is absent and its parent %q is not a directory", path, parent)
	}
	if _, err := fsEvalSymlinks(parent); err != nil {
		return workspaceAvailabilityInvalid,
			fmt.Errorf("workspace %q is absent and its parent %q could not be resolved: %w", path, parent, err)
	}
	return workspaceAvailabilityMissing, &workspaceMissingError{path: path, err: lerr}
}
