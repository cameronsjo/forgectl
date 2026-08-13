package pr

// Test plan for workspace_state.go
//
// classifyWorkspace (Classification: fail-closed delete authority)
//   [x] A real sandbox directory is LIVE
//   [x] A symlink resolving to a prefixed sandbox is LIVE (pins the existing
//       validate-resolved/act-unresolved split)
//   [x] A lexically absent final component under a real parent is MISSING
//   [x] MISSING is a *workspaceMissingError and unwraps to fs.ErrNotExist
//   [x] A DANGLING final symlink is INVALID, never MISSING — the load-bearing
//       case that makes errors.As mandatory and errors.Is insufficient
//   [x] Missing parent, non-directory parent, and dangling parent link are INVALID
//   [x] Non-directory, bad resolved prefix are INVALID
//   [x] EACCES, generic I/O, and post-Lstat disappearance (seams) are INVALID
//   [x] Relative and unclean pathnames are INVALID
//   [x] Only LIVE and MISSING ever return their state; every refusal is INVALID
//       with a non-nil error

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// swapFSSeams installs classifier filesystem seams for one test and restores
// them afterwards. Tests using it must not call t.Parallel: the seams are
// package-level.
func swapFSSeams(t *testing.T, lstat, stat func(string) (fs.FileInfo, error), eval func(string) (string, error)) {
	t.Helper()
	oldLstat, oldStat, oldEval := fsLstat, fsStat, fsEvalSymlinks
	if lstat != nil {
		fsLstat = lstat
	}
	if stat != nil {
		fsStat = stat
	}
	if eval != nil {
		fsEvalSymlinks = eval
	}
	t.Cleanup(func() { fsLstat, fsStat, fsEvalSymlinks = oldLstat, oldStat, oldEval })
}

func TestClassifyWorkspace_Live(t *testing.T) {
	ws := fakeWorkspace(t)
	avail, err := classifyWorkspace(ws)
	if err != nil {
		t.Fatalf("classifyWorkspace(%q) error = %v, want nil", ws, err)
	}
	if avail != workspaceAvailabilityLive {
		t.Errorf("availability = %d, want live", avail)
	}
}

// TestClassifyWorkspace_LiveThroughSymlink pins that a link NAMED without the
// sandbox prefix but RESOLVING to a prefixed sandbox stays live — the shape
// validateWorkspace has always accepted, unchanged by the classifier.
func TestClassifyWorkspace_LiveThroughSymlink(t *testing.T) {
	ws := fakeWorkspace(t)
	link := filepath.Join(t.TempDir(), "plain-name")
	if err := os.Symlink(ws, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	avail, err := classifyWorkspace(link)
	if err != nil {
		t.Fatalf("classifyWorkspace(link) error = %v, want nil", err)
	}
	if avail != workspaceAvailabilityLive {
		t.Errorf("availability = %d, want live", avail)
	}
}

func TestClassifyWorkspace_MissingIsTypedAndUnwraps(t *testing.T) {
	absent := filepath.Join(t.TempDir(), "forgectl-workflow-gone")
	avail, err := classifyWorkspace(absent)
	if avail != workspaceAvailabilityMissing {
		t.Fatalf("availability = %d, want missing (err = %v)", avail, err)
	}
	var missing *workspaceMissingError
	if !errors.As(err, &missing) {
		t.Fatalf("error %v is not a *workspaceMissingError; remediation and stale unlink both gate on this type", err)
	}
	if missing.path != absent {
		t.Errorf("missing.path = %q, want %q", missing.path, absent)
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Error("a missing workspace error must still unwrap to fs.ErrNotExist")
	}
}

// TestClassifyWorkspace_DanglingFinalSymlinkIsInvalid is the case that forces
// errors.As over errors.Is. A dangling link surfaces ENOENT from Stat just as
// a real absence does — but Lstat sees an entry, so the path is INVALID and
// must never authorize a stale unlink.
func TestClassifyWorkspace_DanglingFinalSymlinkIsInvalid(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "forgectl-workflow-dangling")
	if err := os.Symlink(filepath.Join(dir, "no-such-target"), link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	avail, err := classifyWorkspace(link)
	if avail != workspaceAvailabilityInvalid {
		t.Fatalf("availability = %d, want invalid — a dangling link is not a clean absence", avail)
	}
	if err == nil {
		t.Fatal("invalid classification must carry an error")
	}
	var missing *workspaceMissingError
	if errors.As(err, &missing) {
		t.Error("a dangling final symlink must NOT produce a workspaceMissingError")
	}
}

func TestClassifyWorkspace_InvalidStates(t *testing.T) {
	dir := t.TempDir()

	file := filepath.Join(dir, "forgectl-workflow-file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	unprefixed := filepath.Join(dir, "not-a-sandbox")
	if err := os.MkdirAll(unprefixed, 0o700); err != nil {
		t.Fatalf("seed unprefixed dir: %v", err)
	}
	parentIsFile := filepath.Join(file, "forgectl-workflow-child")

	danglingParent := filepath.Join(dir, "gone-parent")
	if err := os.Symlink(filepath.Join(dir, "no-such-dir"), danglingParent); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	cases := []struct {
		name string
		path string
	}{
		{"relative path", "relative/forgectl-workflow-x"},
		// Built by concatenation, NOT filepath.Join — Join cleans its own
		// result, so it can never produce the form under test.
		{"unclean path", dir + "/sub/../forgectl-workflow-x"},
		{"unclean trailing slash", dir + "/forgectl-workflow-x/"},
		{"non-directory", file},
		{"bad resolved prefix", unprefixed},
		{"missing parent", filepath.Join(dir, "no-such-parent", "forgectl-workflow-x")},
		{"parent is a file", parentIsFile},
		{"dangling parent link", filepath.Join(danglingParent, "forgectl-workflow-x")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			avail, err := classifyWorkspace(tc.path)
			if avail != workspaceAvailabilityInvalid {
				t.Fatalf("availability = %d, want invalid", avail)
			}
			if err == nil {
				t.Fatal("invalid classification must carry an error")
			}
			var missing *workspaceMissingError
			if errors.As(err, &missing) {
				t.Error("an invalid workspace must never produce a workspaceMissingError")
			}
		})
	}
}

// TestClassifyWorkspace_SeamedFailures covers the states that cannot be staged
// portably: a permission-denied parent, a generic I/O error, and a workspace
// that disappears between Lstat and the checks that follow.
func TestClassifyWorkspace_SeamedFailures(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "forgectl-workflow-seamed")

	cases := []struct {
		name  string
		lstat func(string) (fs.FileInfo, error)
		stat  func(string) (fs.FileInfo, error)
		eval  func(string) (string, error)
	}{
		{
			name: "EACCES on Lstat",
			lstat: func(string) (fs.FileInfo, error) {
				return nil, &fs.PathError{Op: "lstat", Path: target, Err: syscall.EACCES}
			},
		},
		{
			name: "generic I/O on Lstat",
			lstat: func(string) (fs.FileInfo, error) {
				return nil, &fs.PathError{Op: "lstat", Path: target, Err: syscall.EIO}
			},
		},
		{
			name: "EACCES on the parent",
			lstat: func(string) (fs.FileInfo, error) {
				return nil, &fs.PathError{Op: "lstat", Path: target, Err: syscall.ENOENT}
			},
			stat: func(string) (fs.FileInfo, error) {
				return nil, &fs.PathError{Op: "stat", Path: dir, Err: syscall.EACCES}
			},
		},
		{
			name: "unresolvable parent",
			lstat: func(string) (fs.FileInfo, error) {
				return nil, &fs.PathError{Op: "lstat", Path: target, Err: syscall.ENOENT}
			},
			eval: func(string) (string, error) {
				return "", &fs.PathError{Op: "readlink", Path: dir, Err: syscall.ELOOP}
			},
		},
		{
			// The entry is present at Lstat and gone by the time
			// validateWorkspace stats it — a live race, never an absence.
			name: "disappears after Lstat",
			stat: func(string) (fs.FileInfo, error) {
				return nil, &fs.PathError{Op: "stat", Path: target, Err: syscall.ENOENT}
			},
			lstat: func(string) (fs.FileInfo, error) { return os.Lstat(dir) },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			swapFSSeams(t, tc.lstat, tc.stat, tc.eval)
			avail, err := classifyWorkspace(target)
			if avail != workspaceAvailabilityInvalid {
				t.Fatalf("availability = %d, want invalid", avail)
			}
			if err == nil {
				t.Fatal("invalid classification must carry an error")
			}
			var missing *workspaceMissingError
			if errors.As(err, &missing) {
				t.Error("a seamed failure must never manufacture delete authority")
			}
		})
	}
}

// TestWorkspaceAvailabilityZeroValueIsInvalid pins the fail-closed enum: a
// forgotten assignment must not read as an actionable state.
func TestWorkspaceAvailabilityZeroValueIsInvalid(t *testing.T) {
	var zero workspaceAvailability
	if zero != workspaceAvailabilityInvalid {
		t.Errorf("zero value = %d, want workspaceAvailabilityInvalid", zero)
	}
}
