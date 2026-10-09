//go:build unix

package worker

import (
	"errors"
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/unix"

	"github.com/cameronsjo/forgectl/internal/privdir"
)

// Read returns the entry for head, or nil when there is none.
func (c StatusCache) Read(head string) ([]byte, error) {
	name, err := statusCacheName(head)
	if err != nil {
		return nil, err
	}
	dir, err := fileStore{stateBase: c.stateBase}.pin(false)
	if errors.Is(err, privdir.ErrAbsent) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("worker: status cache directory: %w", err)
	}
	defer unix.Close(dir) //nolint:errcheck // read-only descriptor
	return readAt(dir, name)
}

// Entries lists the cache's entries with their modification times. A
// directory that does not exist yet holds none.
func (c StatusCache) Entries() ([]StatusCacheEntry, error) {
	dir, err := fileStore{stateBase: c.stateBase}.pin(false)
	if errors.Is(err, privdir.ErrAbsent) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("worker: status cache directory: %w", err)
	}
	defer unix.Close(dir) //nolint:errcheck // read-only descriptor
	dup, err := unix.Dup(dir)
	if err != nil {
		return nil, fmt.Errorf("worker: status cache directory: %w", err)
	}
	d := os.NewFile(uintptr(dup), "surface")
	names, err := d.Readdirnames(-1)
	d.Close() //nolint:errcheck,gosec // read-only directory descriptor
	if err != nil {
		return nil, fmt.Errorf("worker: list the status cache: %w", err)
	}
	var out []StatusCacheEntry
	for _, name := range names {
		head, ok := statusCacheHead(name)
		if !ok {
			continue
		}
		var st unix.Stat_t
		if err := unix.Fstatat(dir, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			continue // removed meanwhile
		}
		out = append(out, StatusCacheEntry{Head: head, ModTime: time.Unix(st.Mtim.Unix())})
	}
	return out, nil
}

// Remove deletes the entry for head when it still has the modification
// time listed, so an entry status rewrote meanwhile stays. A missing entry
// is not an error.
func (c StatusCache) Remove(e StatusCacheEntry) error {
	name, err := statusCacheName(e.Head)
	if err != nil {
		return err
	}
	dir, err := fileStore{stateBase: c.stateBase}.pin(false)
	if errors.Is(err, privdir.ErrAbsent) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("worker: status cache directory: %w", err)
	}
	defer unix.Close(dir) //nolint:errcheck // nothing to flush on a directory fd
	var st unix.Stat_t
	if err := unix.Fstatat(dir, name, &st, unix.AT_SYMLINK_NOFOLLOW); errors.Is(err, unix.ENOENT) {
		return nil
	} else if err != nil {
		return fmt.Errorf("worker: stat %s: %w", name, err)
	}
	if !time.Unix(st.Mtim.Unix()).Equal(e.ModTime) {
		return ErrStatusCacheChanged
	}
	if err := unix.Unlinkat(dir, name, 0); err != nil && !errors.Is(err, unix.ENOENT) {
		return fmt.Errorf("worker: remove %s: %w", name, err)
	}
	return nil
}

// Write replaces the entry for head atomically.
func (c StatusCache) Write(head string, data []byte) error {
	name, err := statusCacheName(head)
	if err != nil {
		return err
	}
	if len(data) > maxLedgerBytes {
		return fmt.Errorf("worker: a status cache entry of %d bytes is over the %d-byte cap", len(data), maxLedgerBytes)
	}
	dir, err := fileStore{stateBase: c.stateBase}.pin(true)
	if err != nil {
		return fmt.Errorf("worker: status cache directory: %w", err)
	}
	defer unix.Close(dir) //nolint:errcheck // nothing to flush on a directory fd
	return writeAt(dir, name+".tmp", name, data)
}
