//go:build unix

package config

import (
	"fmt"
	"os"
	"syscall"
)

// WithFileLock runs fn while holding a blocking exclusive advisory lock on
// path+".lock" — closing the concurrent-writer window a read-decide-write
// critical section otherwise leaves open: two racing forgectl processes can
// both observe the same "safe to write" precondition, both perform the
// write, and whichever lands second silently corrupts or discards the
// first's. Holding the lock across the whole critical section forces the
// second caller to observe the first caller's already-written result before
// it decides anything.
//
// The lock file is a sibling, never path itself — flock locks an open file
// description, not the path, so locking path directly would still race with
// an atomic rename-based write swapping the directory entry out from under
// it. The kernel releases the flock automatically when the file descriptor
// closes (including on process death), so there is no stale-lock file to
// detect or clean up — only the (harmless, non-secret) lock file itself
// persists on disk. Mirrors internal/env's withFileLock (same pattern,
// closing the same class of lost-update race for a different config file);
// exported here because config.toml's writers live in internal/cli, a
// different package from the lock.
func WithFileLock(path string, fn func() error) error {
	lockPath := path + ".lock"
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open lock file %s: %w", lockPath, err)
	}
	defer func() { _ = f.Close() }()

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("lock %s: %w", lockPath, err)
	}
	defer func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }()

	return fn()
}
