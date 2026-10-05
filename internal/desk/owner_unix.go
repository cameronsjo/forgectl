//go:build unix

package desk

import (
	"errors"
	"fmt"
	"os"
	"path"
	"time"

	"golang.org/x/sys/unix"
)

// The owner lock: running/<name>.lock, flock'ed exclusively by the process
// that owns a run (the supervisor, or the desk for a TTY item) from BeginRun
// to Finish. The kernel drops it when that process dies, so a held lock is a
// live owner whatever the clock or the meta says. Skip and Release take it
// without waiting before they move a running item, and refuse while it is
// held; BeginRun waits for it, so a Skip or Release already moving the item
// finishes first and BeginRun then finds the item gone.
//
// Scans ignore the file: it is not an item name. Finish, Skip and Release
// remove it.

const extLock = ".lock"

func lockName(name string) string { return path.Join(DirRunning, name+extLock) }

// ownerLock is a held owner lock.
type ownerLock struct {
	d    *Desk
	name string
	f    *os.File
}

// openLock opens (creating) the lock file, which must be a regular file.
func (d *Desk) openLock(name string) (*os.File, error) {
	f, err := d.root.OpenFile(lockName(name), os.O_RDWR|os.O_CREATE|unix.O_NONBLOCK, fileMode)
	if err != nil {
		return nil, fmt.Errorf("desk: open the owner lock for %s: %w", describe(name), err)
	}
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		_ = f.Close()
		return nil, fmt.Errorf("%w: the owner lock for %s is not a regular file", ErrRefused, describe(name))
	}
	return f, nil
}

// lockStale bounds how often a lock is retaken because the file it locked was
// unlinked or replaced while the taker waited.
const lockStale = 10

// lockOwner takes the owner lock, waiting for a Skip or Release that holds
// it to finish.
func (d *Desk) lockOwner(name string) (*ownerLock, error) {
	return d.takeLock(name, unix.LOCK_EX)
}

// tryLockOwner takes the owner lock without waiting. held is true, with no
// lock returned, when another open file holds it: a live owner.
func (d *Desk) tryLockOwner(name string) (l *ownerLock, held bool, err error) {
	l, err = d.takeLock(name, unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) {
		return nil, true, nil
	}
	return l, false, err
}

// takeLock opens the lock file and flocks it with how. A lock is only a lock
// while the path still names the file it was taken on: a holder that
// unlinked the file while this taker waited leaves it locking an inode no
// other process can find. So after the flock the open file must be the file
// at the path (os.SameFile); if not, it is closed and taken again.
func (d *Desk) takeLock(name string, how int) (*ownerLock, error) {
	for range lockStale {
		f, err := d.openLock(name)
		if err != nil {
			return nil, err
		}
		if err := unix.Flock(int(f.Fd()), how); err != nil {
			_ = f.Close()
			if errors.Is(err, unix.EWOULDBLOCK) {
				return nil, err
			}
			return nil, fmt.Errorf("desk: take the owner lock for %s: %w", describe(name), err)
		}
		locked, ferr := f.Stat()
		named, lerr := d.root.Lstat(lockName(name))
		if ferr == nil && lerr == nil && os.SameFile(locked, named) {
			return &ownerLock{d: d, name: name, f: f}, nil
		}
		_ = f.Close() // the file was unlinked or replaced while we waited
	}
	return nil, fmt.Errorf("desk: the owner lock for %s kept changing; try again", describe(name))
}

// Retry for tryLockOwnerSettled: a liveness probe (ownerAlive) holds the
// lock shared for a moment, so one failed try is not a live owner.
const (
	ownerLockTries = 5
	ownerLockWait  = 20 * time.Millisecond
)

// tryLockOwnerSettled is tryLockOwner retried over a short window: only a
// lock held on every try counts as held. Skip and Release use it, so a scan's
// probe landing at the same instant does not refuse them.
func (d *Desk) tryLockOwnerSettled(name string) (l *ownerLock, held bool, err error) {
	for try := 1; ; try++ {
		l, held, err = d.tryLockOwner(name)
		if err != nil || !held || try == ownerLockTries {
			return l, held, err
		}
		time.Sleep(ownerLockWait)
	}
}

// release drops the lock, removing the lock file first (while still holding
// it) when remove is set. Remove only once the item has left running/ (Finish,
// or a Skip or Release that moved it): a waiter may be blocked on this very
// file, and takeLock makes it retake the lock on whatever the path names.
func (l *ownerLock) release(remove bool) {
	if remove {
		_ = l.d.root.Remove(lockName(l.name)) // already gone is fine
	}
	_ = l.f.Close() // closing the descriptor drops the flock
}

// ownerAlive reports whether another open file holds name's owner lock. It
// never creates the file; a missing lock file is no owner.
func (d *Desk) ownerAlive(name string) bool {
	f, err := d.root.OpenFile(lockName(name), os.O_RDONLY|unix.O_NONBLOCK, 0)
	if err != nil {
		return false
	}
	defer f.Close() //nolint:errcheck // read-only probe
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return false
	}
	err = unix.Flock(int(f.Fd()), unix.LOCK_SH|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) {
		return true
	}
	if err == nil {
		_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
	}
	return false
}
