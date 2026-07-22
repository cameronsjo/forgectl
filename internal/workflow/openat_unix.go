//go:build unix

package workflow

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/cameronsjo/forgectl/internal/config"
)

// stateDir is an open handle to the workflow state directory, pinned by an
// O_DIRECTORY|O_NOFOLLOW file descriptor. Every create / rename / unlink /
// lock-open / dir-sync runs *at (openat-relative) that fd rather than
// re-resolving the .state path, so a same-user adversary who swaps .state for a
// symlink AFTER the handle is opened cannot redirect any subsequent I/O — the fd
// already names the real inode. This structurally closes the TOCTOU window
// between the Lstat guard and the path-based reopens the old code left open
// (forgectl #128). The path field is display/error text only; it is never
// re-resolved for an operation.
type stateDir struct {
	fd   int
	path string
}

// openStateDir resolves the workflow state directory, keeps the Lstat
// refuse-symlink/file guard and the 0o700 MkdirAll, then opens a directory fd
// with O_DIRECTORY|O_NOFOLLOW|O_CLOEXEC. O_NOFOLLOW refuses the final component
// if it has become a symlink and O_DIRECTORY refuses a non-directory (defence in
// depth behind the Lstat check); O_CLOEXEC keeps the dir fd from leaking into
// the step subprocesses forgectl spawns while a run is in flight. Callers MUST
// close the returned handle.
func openStateDir() (*stateDir, error) {
	dir, err := config.WorkflowStateDir()
	if err != nil {
		return nil, err
	}
	// Refuse a pre-planted symlink/file at .state and MkdirAll it (shared helper,
	// so the refuse-symlink message stays defined once; the message is asserted by
	// TestEnsureStateDir_RefusesSymlinkedStateDir and must stay stable).
	if err := guardAndMakeStateDir(dir); err != nil {
		return nil, err
	}
	fd, err := unix.Openat(unix.AT_FDCWD, dir, unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open workflow state dir %s: %w", dir, err)
	}
	return &stateDir{fd: fd, path: dir}, nil
}

// randRead is the crypto-random source for temp-name suffixes; a package var so
// tests can force guaranteed collisions to exercise the exhaustion branch.
var randRead = rand.Read

// createTemp creates a fresh temp file *at the dir fd* and returns the open file
// plus its dir-relative name. pattern follows os.CreateTemp's convention (the
// last '*' is where the random string lands). O_EXCL+O_NOFOLLOW makes the open
// both refuse an existing name and refuse to follow a symlink planted at that
// name; O_CLOEXEC keeps the temp fd out of spawned subprocesses. The EEXIST
// retry is bounded — a collision on a 64-bit crypto-random suffix is effectively
// impossible, so exhausting the cap signals something pathological rather than
// bad luck.
func (d *stateDir) createTemp(pattern string) (*os.File, string, error) {
	prefix, suffix := prefixAndSuffix(pattern)
	const maxAttempts = 100
	for range maxAttempts {
		var rnd [8]byte
		if _, err := randRead(rnd[:]); err != nil {
			return nil, "", fmt.Errorf("generate random temp suffix: %w", err)
		}
		name := prefix + hex.EncodeToString(rnd[:]) + suffix
		fd, err := unix.Openat(d.fd, name, unix.O_CREAT|unix.O_EXCL|unix.O_WRONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
		if err != nil {
			if errors.Is(err, unix.EEXIST) {
				continue
			}
			return nil, "", fmt.Errorf("openat %s: %w", name, err)
		}
		return os.NewFile(uintptr(fd), filepath.Join(d.path, name)), name, nil
	}
	return nil, "", fmt.Errorf("exhausted %d attempts finding an unused temp name", maxAttempts)
}

// rename atomically renames oldName to newName, both dir-relative, through the
// pinned fd (renameat) so neither side re-resolves the .state path.
func (d *stateDir) rename(oldName, newName string) error {
	return unix.Renameat(d.fd, oldName, d.fd, newName)
}

// remove unlinks a dir-relative name through the pinned fd. A missing entry is
// success — the temp file may already have been renamed away, so ENOENT is the
// expected outcome on the happy path's deferred cleanup.
func (d *stateDir) remove(name string) error {
	if err := unix.Unlinkat(d.fd, name, 0); err != nil && !errors.Is(err, unix.ENOENT) {
		return err
	}
	return nil
}

// openLock opens (creating if absent) a dir-relative lock file through the
// pinned fd. O_NOFOLLOW refuses a pre-planted symlink at the lock name;
// O_CLOEXEC is REQUIRED here (raw Openat does NOT set it implicitly the way
// os.OpenFile does) because the lock fd is held for the whole run while step
// subprocesses are spawned — without it the lock would leak into every child.
func (d *stateDir) openLock(name string) (*os.File, error) {
	fd, err := unix.Openat(d.fd, name, unix.O_CREAT|unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), filepath.Join(d.path, name)), nil
}

// syncDir fsyncs the pinned directory fd so a rename into it survives a crash —
// the durability half of the atomic-rename write, done on the handle already
// open rather than reopening the path.
func (d *stateDir) syncDir() error {
	if err := unix.Fsync(d.fd); err != nil {
		return fmt.Errorf("sync state dir %s: %w", d.path, err)
	}
	return nil
}

// close releases the pinned directory fd.
func (d *stateDir) close() error {
	return unix.Close(d.fd)
}

// prefixAndSuffix splits an os.CreateTemp-style pattern at its last '*' — the
// point the random string replaces — so the unix and non-unix paths mint the
// same "<prefix><random><suffix>" filename shape. A pattern with no '*' takes
// the random string as a suffix, matching os.CreateTemp.
func prefixAndSuffix(pattern string) (prefix, suffix string) {
	if i := strings.LastIndex(pattern, "*"); i >= 0 {
		return pattern[:i], pattern[i+1:]
	}
	return pattern, ""
}
