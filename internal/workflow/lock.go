package workflow

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/cameronsjo/forgectl/internal/config"
)

// ErrWorkflowRunning is returned by AcquireRunLock when another run of the same
// workflow already holds the advisory lock.
var ErrWorkflowRunning = errors.New("workflow is already running")

// RunLock is the held advisory lock guarding one workflow's run-state sidecar
// against concurrent writers. Release it when the run finishes.
type RunLock struct {
	f *os.File
}

// LockPath returns the advisory-lock file path for a workflow name — a sibling
// of the state sidecar in config.WorkflowStateDir(). The name is validated the
// same way as every other state path.
func LockPath(name string) (string, error) {
	if err := validateWorkflowName(name); err != nil {
		return "", err
	}
	dir, err := config.WorkflowStateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, name+".lock"), nil
}

// AcquireRunLock takes a non-blocking exclusive advisory lock on a workflow's
// lock file, so only one run/resume of that workflow proceeds at a time. A
// second concurrent run fails fast with ErrWorkflowRunning rather than
// interleaving state writes. On a platform without advisory locking the lock is
// a documented no-op (see flock_other.go) — the caller still gets a RunLock to
// Release.
func AcquireRunLock(name string) (*RunLock, error) {
	path, err := LockPath(name)
	if err != nil {
		return nil, err
	}
	if _, err := ensureStateDir(); err != nil {
		return nil, err
	}
	// O_NOFOLLOW (unix) refuses a pre-planted <name>.lock symlink — the one file
	// here opened directly rather than via CreateTemp+Rename. The .state dir
	// itself is guarded against a symlink by ensureStateDir above; together they
	// cover the path. On non-unix lockOpenExtraFlags is 0 (the lock is a
	// documented no-op there anyway).
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|lockOpenExtraFlags, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open workflow lock %s: %w", path, err)
	}
	locked, err := flockTryExclusive(f)
	if err != nil {
		f.Close() //nolint:errcheck
		return nil, fmt.Errorf("lock workflow %q: %w", name, err)
	}
	if !locked {
		f.Close() //nolint:errcheck
		return nil, fmt.Errorf("%w: %q is already running (another 'workflow run %s' holds the lock)", ErrWorkflowRunning, name, name)
	}
	return &RunLock{f: f}, nil
}

// Release unlocks and closes the lock file. It is safe to call on a nil-guarded
// lock and never returns a fatal error to the caller — a failed unlock is
// released by process exit anyway.
func (l *RunLock) Release() {
	if l == nil || l.f == nil {
		return
	}
	_ = flockUnlock(l.f)
	_ = l.f.Close()
	l.f = nil
}
