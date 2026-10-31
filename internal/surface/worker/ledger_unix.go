//go:build unix

package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"

	"github.com/cameronsjo/forgectl/internal/privdir"
)

// ledgerFileMode is the mode of every file in the ledger directory.
const ledgerFileMode = 0o600

// fileStore keeps one ledger in <state>/forgectl/surface/<key>.json.
//
// The directory is pinned by descriptor (privdir), and every file below it is
// opened with openat and O_NOFOLLOW, then checked for owner, type and link
// count — the same pattern the launch usage store uses. No later step
// rebuilds a path string, so swapping a name for a symlink after the pin
// redirects nothing.
type fileStore struct {
	stateBase string
	key       string
}

func newFileStore(stateBase, key string) store {
	return fileStore{stateBase: stateBase, key: key}
}

func (s fileStore) dataName() string { return s.key + ".json" }
func (s fileStore) tmpName() string  { return s.key + ".json.tmp" }
func (s fileStore) lockName() string { return s.key + ".lock" }

func (s fileStore) pin(create bool) (int, error) {
	return privdir.Pin(privdir.Spec{
		Base:         filepath.Join(s.stateBase, "forgectl"),
		Leaf:         "surface",
		Mode:         0o700,
		AncestorMode: 0o755,
		Create:       create,
	})
}

func (s fileStore) read() ([]byte, error) {
	dir, err := s.pin(false)
	if errors.Is(err, privdir.ErrAbsent) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("worker: ledger directory: %w", err)
	}
	defer unix.Close(dir) //nolint:errcheck // read-only descriptor
	return readAt(dir, s.dataName())
}

func (s fileStore) update(fn func([]byte) ([]byte, error)) error {
	dir, err := s.pin(true)
	if err != nil {
		return fmt.Errorf("worker: ledger directory: %w", err)
	}
	defer unix.Close(dir) //nolint:errcheck // nothing to flush on a directory fd

	lock, err := openLockFile(dir, s.lockName())
	if err != nil {
		return err
	}
	defer lock.Close() //nolint:errcheck // closing releases the flock
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		return fmt.Errorf("worker: lock ledger: %w", err)
	}

	current, err := readAt(dir, s.dataName())
	if err != nil {
		return err
	}
	next, err := fn(current)
	if err != nil {
		return err
	}
	return writeAt(dir, s.tmpName(), s.dataName(), next)
}

// lockPoll is how often lockNamed retries a lock another process holds.
const lockPoll = 50 * time.Millisecond

func (s fileStore) lockNamed(ctx context.Context, suffix string) (func(), error) {
	lock, err := s.openLock(suffix)
	if err != nil {
		return nil, err
	}
	for {
		err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return func() { _ = lock.Close() }, nil // closing releases the flock
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EINTR) {
			_ = lock.Close()
			return nil, fmt.Errorf("worker: lock %s: %w", suffix, err)
		}
		select {
		case <-ctx.Done():
			_ = lock.Close()
			return nil, fmt.Errorf("%w: %w", ErrBriefBusy, ctx.Err())
		case <-time.After(lockPoll):
		}
	}
}

// lockOpenAttempts bounds the retries of a lock file's create-or-open. On
// macOS, two processes that openat the same new name with O_CREAT at once
// can get ENOENT: a two-goroutine probe on APFS saw it on about 20% of
// opens, with or without O_NOFOLLOW. By the time the loser retries, the
// winner's file exists, so a retry opens it.
const lockOpenAttempts = 3

// openLockFile opens (creating) the lock file name in the pinned directory
// dir, retrying the create race lockOpenAttempts describes.
func openLockFile(dir int, name string) (*os.File, error) {
	var last error
	for range lockOpenAttempts {
		lock, err := openVerified(dir, name, unix.O_RDWR|unix.O_CREAT)
		if !errors.Is(err, os.ErrNotExist) {
			return lock, err
		}
		last = err
	}
	return nil, fmt.Errorf("worker: open the lock %s: %w", name, last)
}

// openLock pins the ledger directory and opens (creating) the named lock
// file in it.
func (s fileStore) openLock(suffix string) (*os.File, error) {
	dir, err := s.pin(true)
	if err != nil {
		return nil, fmt.Errorf("worker: ledger directory: %w", err)
	}
	defer unix.Close(dir) //nolint:errcheck // the lock file keeps its own descriptor
	return openLockFile(dir, s.key+"."+suffix+".lock")
}

// listLedgersAt lists the ledger files in the pinned surface directory under
// stateBase. A directory that does not exist yet holds none.
func listLedgersAt(stateBase string) ([]LedgerID, []string, error) {
	dir, err := fileStore{stateBase: stateBase}.pin(false)
	if errors.Is(err, privdir.ErrAbsent) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("worker: ledger directory: %w", err)
	}
	defer unix.Close(dir) //nolint:errcheck // read-only descriptor
	names, err := listNames(dir)
	if err != nil {
		return nil, nil, fmt.Errorf("worker: list the ledger directory: %w", err)
	}
	var ids []LedgerID
	var bad []string
	for _, name := range names {
		if !isLedgerFileName(name) {
			continue
		}
		data, err := readAt(dir, name)
		if err != nil || data == nil {
			bad = append(bad, name)
			continue
		}
		id, err := ledgerIDOf(name, data)
		if err != nil {
			bad = append(bad, name)
			continue
		}
		ids = append(ids, id)
	}
	return ids, bad, nil
}

// listNames lists the entries of the pinned directory dir through a
// duplicate of its descriptor, so no path is resolved again and dir stays
// open for the caller.
func listNames(dir int) ([]string, error) {
	dup, err := unix.Dup(dir)
	if err != nil {
		return nil, err
	}
	d := os.NewFile(uintptr(dup), "surface")
	names, err := d.Readdirnames(-1)
	d.Close() //nolint:errcheck,gosec // read-only directory descriptor
	return names, err
}

// readAt returns the file's contents, or nil when it does not exist.
func readAt(dir int, name string) ([]byte, error) {
	return readAtMax(dir, name, maxLedgerBytes)
}

// readAtMax is readAt with its own size cap: a file larger than limit is
// ErrLedgerUnreadable, never read in part.
func readAtMax(dir int, name string, limit int) ([]byte, error) {
	f, err := openVerified(dir, name, unix.O_RDONLY)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck // read-only
	data, err := io.ReadAll(io.LimitReader(f, int64(limit)+1))
	if err != nil {
		// The reason alone, so the text reads as it did before the check
		// moved to privdir: "<sentinel>: <name> is a symlink".
		return nil, fmt.Errorf("%w: %s", ErrLedgerUnreadable, privdir.Reason(err))
	}
	if len(data) > limit {
		return nil, fmt.Errorf("%w: %s is larger than %d bytes", ErrLedgerUnreadable, name, limit)
	}
	return data, nil
}

// writeAt replaces name atomically: write a temp file in the same directory,
// sync it, rename it over name, sync the directory. The caller holds the lock,
// so a fixed temp name cannot collide with another writer.
func writeAt(dir int, tmp, name string, data []byte) error {
	if err := unix.Unlinkat(dir, tmp, 0); err != nil && !errors.Is(err, unix.ENOENT) {
		return fmt.Errorf("worker: clear stale ledger temp: %w", err)
	}
	f, err := openVerified(dir, tmp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close() //nolint:errcheck,gosec // already failing
		return fmt.Errorf("worker: write ledger: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close() //nolint:errcheck,gosec // already failing
		return fmt.Errorf("worker: sync ledger: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("worker: close ledger: %w", err)
	}
	if err := unix.Renameat(dir, tmp, dir, name); err != nil {
		return fmt.Errorf("worker: replace ledger: %w", err)
	}
	if err := unix.Fsync(dir); err != nil {
		return fmt.Errorf("worker: sync ledger directory: %w", err)
	}
	return nil
}

// openVerified opens name under dir through privdir.OpenFile, which refuses
// anything but a regular, single-link file this user owns that is the object
// the name resolves to, and narrows its mode to ledgerFileMode. An absent
// name is os.ErrNotExist; every refusal is ErrLedgerUnreadable.
func openVerified(dir int, name string, flags int) (*os.File, error) {
	f, err := privdir.OpenFile(dir, name, flags, ledgerFileMode)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil, os.ErrNotExist
	case err != nil:
		// The reason alone, so the text reads as it did before the check
		// moved to privdir: "<sentinel>: <name> is a symlink".
		return nil, fmt.Errorf("%w: %s", ErrLedgerUnreadable, privdir.Reason(err))
	}
	return f, nil
}
