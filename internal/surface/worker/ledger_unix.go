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
	// Listed through a duplicate of the pinned descriptor, so no path is
	// resolved again.
	dup, err := unix.Dup(dir)
	if err != nil {
		return nil, nil, fmt.Errorf("worker: ledger directory: %w", err)
	}
	d := os.NewFile(uintptr(dup), "surface")
	names, err := d.Readdirnames(-1)
	d.Close() //nolint:errcheck,gosec // read-only directory descriptor
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

// openVerified opens name under dir without following a symlink, then refuses
// it unless verifyLedgerFile passes.
func openVerified(dir int, name string, flags int) (*os.File, error) {
	fd, err := unix.Openat(dir, name, flags|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, ledgerFileMode)
	switch {
	case errors.Is(err, unix.ENOENT):
		return nil, os.ErrNotExist
	case errors.Is(err, unix.ELOOP):
		return nil, fmt.Errorf("%w: %s is a symlink", ErrLedgerUnreadable, name)
	case errors.Is(err, unix.ENXIO), errors.Is(err, unix.EISDIR), errors.Is(err, unix.ENOTDIR):
		return nil, fmt.Errorf("%w: %s is not a regular file", ErrLedgerUnreadable, name)
	case err != nil:
		return nil, fmt.Errorf("%w: open %s: %w", ErrLedgerUnreadable, name, err)
	}
	f := os.NewFile(uintptr(fd), name)
	if err := verifyLedgerFile(dir, name, fd); err != nil {
		f.Close() //nolint:errcheck,gosec // refusing; nothing was written
		return nil, err
	}
	return f, nil
}

// verifyLedgerFile proves the open file is a regular, single-link file owned
// by this user and is the same object the directory entry names, then narrows
// its mode and checks nothing changed while it did.
func verifyLedgerFile(dir int, name string, fd int) error {
	var pinned unix.Stat_t
	if err := unix.Fstat(fd, &pinned); err != nil {
		return fmt.Errorf("%w: stat %s: %w", ErrLedgerUnreadable, name, err)
	}
	switch {
	case pinned.Mode&unix.S_IFMT != unix.S_IFREG:
		return fmt.Errorf("%w: %s is not a regular file", ErrLedgerUnreadable, name)
	case int(pinned.Uid) != os.Geteuid():
		return fmt.Errorf("%w: %s is owned by another user", ErrLedgerUnreadable, name)
	case pinned.Nlink != 1:
		return fmt.Errorf("%w: %s is hardlinked", ErrLedgerUnreadable, name)
	}
	var named unix.Stat_t
	if err := unix.Fstatat(dir, name, &named, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return fmt.Errorf("%w: stat %s entry: %w", ErrLedgerUnreadable, name, err)
	}
	if named.Dev != pinned.Dev || named.Ino != pinned.Ino {
		return fmt.Errorf("%w: %s entry does not match the opened file", ErrLedgerUnreadable, name)
	}
	if pinned.Mode&0o7777 == ledgerFileMode {
		return nil
	}
	if err := unix.Fchmod(fd, ledgerFileMode); err != nil {
		return fmt.Errorf("%w: restrict %s: %w", ErrLedgerUnreadable, name, err)
	}
	var rechecked unix.Stat_t
	if err := unix.Fstat(fd, &rechecked); err != nil {
		return fmt.Errorf("%w: re-stat %s: %w", ErrLedgerUnreadable, name, err)
	}
	if rechecked.Dev != pinned.Dev || rechecked.Ino != pinned.Ino || rechecked.Nlink != 1 ||
		rechecked.Mode&0o7777 != ledgerFileMode || int(rechecked.Uid) != os.Geteuid() {
		return fmt.Errorf("%w: %s changed identity while being restricted", ErrLedgerUnreadable, name)
	}
	return nil
}
