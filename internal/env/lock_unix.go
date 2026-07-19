//go:build unix

package env

import (
	"fmt"
	"os"
	"syscall"
)

// withFileLock runs fn while holding an exclusive lock on realPath+".lock" —
// closing the concurrent-set window commitSet's writeAtomic leaves open:
// writeAtomic's tmp+rename prevents a TORN read, but not a LOST update, since
// two concurrent sets both parse the same starting Document, each mutate
// their own key, and whichever rename lands second silently discards the
// first's write. Holding the lock across parse→write (see commitSet) forces
// the second caller's parse to observe the first caller's already-written
// file, so both mutations survive.
//
// The lock file is a sibling, never realPath itself — flock locks an open
// file description, not the path, so locking realPath directly would still
// race with writeAtomic's rename swapping the directory entry out from under
// it. The kernel releases the flock automatically when the file descriptor
// closes (including on process death), so there is no stale-lock file to
// detect or clean up — only the (harmless, non-secret) lock file itself
// persists on disk.
func withFileLock(realPath string, fn func() error) error {
	lockPath := realPath + ".lock"
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
