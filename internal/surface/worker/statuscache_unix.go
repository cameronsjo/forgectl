//go:build unix

package worker

import (
	"errors"
	"fmt"

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
