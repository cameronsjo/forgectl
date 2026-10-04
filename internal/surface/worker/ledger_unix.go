//go:build unix

package worker

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

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

	lock, err := openVerified(dir, s.lockName(), unix.O_RDWR|unix.O_CREAT)
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

// readAt returns the file's contents, or nil when it does not exist.
func readAt(dir int, name string) ([]byte, error) {
	f, err := openVerified(dir, name, unix.O_RDONLY)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck // read-only
	data, err := io.ReadAll(io.LimitReader(f, maxLedgerBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrLedgerUnreadable, err)
	}
	if len(data) > maxLedgerBytes {
		return nil, fmt.Errorf("%w: larger than %d bytes", ErrLedgerUnreadable, maxLedgerBytes)
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

// openVerified opens name under dir without following a symlink and refuses
// anything that is not a regular, single-link file owned by this user. A file
// found with a broader mode is narrowed only after those checks pass.
func openVerified(dir int, name string, flags int) (*os.File, error) {
	fd, err := unix.Openat(dir, name, flags|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, ledgerFileMode)
	switch {
	case errors.Is(err, unix.ENOENT):
		return nil, os.ErrNotExist
	case errors.Is(err, unix.ELOOP):
		return nil, fmt.Errorf("%w: %s is a symlink", ErrLedgerUnreadable, name)
	case err != nil:
		return nil, fmt.Errorf("%w: open %s: %w", ErrLedgerUnreadable, name, err)
	}
	f := os.NewFile(uintptr(fd), name)
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		f.Close() //nolint:errcheck,gosec // refusing
		return nil, fmt.Errorf("%w: stat %s: %w", ErrLedgerUnreadable, name, err)
	}
	switch {
	case st.Mode&unix.S_IFMT != unix.S_IFREG:
		f.Close() //nolint:errcheck,gosec // refusing
		return nil, fmt.Errorf("%w: %s is not a regular file", ErrLedgerUnreadable, name)
	case int(st.Uid) != os.Geteuid():
		f.Close() //nolint:errcheck,gosec // refusing
		return nil, fmt.Errorf("%w: %s is owned by another user", ErrLedgerUnreadable, name)
	case st.Nlink != 1:
		f.Close() //nolint:errcheck,gosec // refusing
		return nil, fmt.Errorf("%w: %s is hardlinked", ErrLedgerUnreadable, name)
	}
	if st.Mode&0o7777 != ledgerFileMode {
		if err := unix.Fchmod(fd, ledgerFileMode); err != nil {
			f.Close() //nolint:errcheck,gosec // refusing
			return nil, fmt.Errorf("%w: restrict %s: %w", ErrLedgerUnreadable, name, err)
		}
	}
	return f, nil
}
