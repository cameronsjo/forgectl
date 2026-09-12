//go:build unix

package env

import (
	"errors"
	"fmt"
	"syscall"
)

// withFileLock runs fn while holding an exclusive lock on the target's
// name + ".lock" inside the pinned directory — closing the concurrent-set
// window commitSet's writeAtomic leaves open: writeAtomic's tmp+rename
// prevents a TORN read, but not a LOST update, since two concurrent sets both
// parse the same starting Document, each mutate their own key, and whichever
// rename lands second silently discards the first's write. Holding the lock
// across parse→write (see commitSet) forces the second caller's parse to
// observe the first caller's already-written file, so both mutations survive.
//
// The lock file is a sibling, never the target itself — flock locks an open
// file description, not a name, so locking the target directly would still
// race with writeAtomic's rename swapping the directory entry out from under
// it. The kernel releases the flock automatically when the descriptor closes
// (including on process death), so there is no stale-lock file to detect or
// clean up — only the (harmless, non-secret) lock file itself persists.
//
// # Why the lock name gets its own checks
//
// The lock name is derived, not resolved, so it does not inherit the target's.
// It needs them: a repo shipping `.env.lock` as a SYMLINK made `env set`
// create a file wherever it pointed while reporting an ordinary success. Git
// stores a symlink as mode 120000, so a hostile repo delivers that by being
// cloned — no local step required.
//
// Two checks, and which one fires is worth stating precisely because the
// intuitive guess is wrong. The Lstat is what refuses a non-regular entry,
// including a FIFO — and the reason is NOT that the open would block: an
// O_RDWR open on a FIFO returns immediately (measured). It is that locking and
// then writing through a FIFO is meaningless. O_NOFOLLOW refuses a symlink,
// including a dangling one, and closes the window between the Lstat and the
// open.
//
// Containment needs no separate check here, and the one that used to be here
// was worse than nothing: sandbox.WithinWorkspace falls back to the
// UNRESOLVED path string when EvalSymlinks fails, and a lock file that does
// not exist yet always fails that way — so it returned true for a symlink
// pointing anywhere, while reading like a containment guard. The descriptor is
// the real control: the lock is created relative to a directory already proven
// to be inside the repository, so it cannot land outside one.
func withFileLock(t Target, fn func() error) error {
	lockName := t.base + ".lock"

	if _, regular, exists, err := t.dir.lstat(lockName); err != nil {
		return fmt.Errorf("stat the lock file for %s: %w", t.Rel(), err)
	} else if exists && !regular {
		// Names neither path: the offending entry is repo-controlled content,
		// and the actionable fact is its role, not its name.
		return errors.New("refusing to lock: the lock file exists and is not a regular file")
	}

	f, err := t.dir.openLock(lockName)
	if err != nil {
		if errors.Is(err, errIsSymlink) {
			return errors.New("refusing to lock: the lock file is a symlink")
		}
		return fmt.Errorf("open the lock file for %s: %w", t.Rel(), err)
	}
	defer func() { _ = f.Close() }()

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("lock %s: %w", t.Rel(), err)
	}
	defer func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }()

	return fn()
}
