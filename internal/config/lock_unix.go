//go:build unix

package config

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
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
	fd, err := unix.Open(lockPath, unix.O_CREAT|unix.O_RDWR|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return fmt.Errorf("open lock file %s: %w", lockPath, err)
	}
	f := os.NewFile(uintptr(fd), lockPath)
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat lock file %s: %w", lockPath, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("lock file %s is not a regular file", lockPath)
	}

	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		return fmt.Errorf("lock %s: %w", lockPath, err)
	}
	defer func() { _ = unix.Flock(int(f.Fd()), unix.LOCK_UN) }()

	return fn()
}
