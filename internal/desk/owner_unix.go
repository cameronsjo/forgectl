//go:build unix

package desk

import (
	"errors"
	"fmt"
	"os"
	"path"

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

// lockOwner takes the owner lock, waiting for a Skip or Release that holds
// it to finish.
func (d *Desk) lockOwner(name string) (*ownerLock, error) {
	f, err := d.openLock(name)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("desk: take the owner lock for %s: %w", describe(name), err)
	}
	return &ownerLock{d: d, name: name, f: f}, nil
}

// tryLockOwner takes the owner lock without waiting. held is true, with no
// lock returned, when another open file holds it: a live owner.
func (d *Desk) tryLockOwner(name string) (l *ownerLock, held bool, err error) {
	f, err := d.openLock(name)
	if err != nil {
		return nil, false, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, true, nil
		}
		return nil, false, fmt.Errorf("desk: take the owner lock for %s: %w", describe(name), err)
	}
	return &ownerLock{d: d, name: name, f: f}, false, nil
}

// release drops the lock, removing the lock file first (while still holding
// it) when remove is set.
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
