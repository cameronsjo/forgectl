//go:build unix

package worker

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"

	"github.com/cameronsjo/forgectl/internal/privdir"
)

// Read returns merge-audit.jsonl, or nil when there is none.
func (a MergeAudit) Read() ([]byte, error) {
	dir, err := fileStore{stateBase: a.stateBase}.pin(false)
	if errors.Is(err, privdir.ErrAbsent) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("worker: audit directory: %w", err)
	}
	defer unix.Close(dir) //nolint:errcheck // read-only descriptor
	return readAtMax(dir, mergeAuditName, MaxMergeAuditBytes)
}

// Append holds merge-audit.lock, reads the whole file, and appends what fn
// returns for it (whole lines ending in a newline; nothing when fn returns
// none), synced before it returns. The lock makes the read and the append
// one step, so two writers cannot chain onto the same line.
func (a MergeAudit) Append(fn func(existing []byte) ([]byte, error)) error {
	dir, err := fileStore{stateBase: a.stateBase}.pin(true)
	if err != nil {
		return fmt.Errorf("worker: audit directory: %w", err)
	}
	defer unix.Close(dir) //nolint:errcheck // nothing to flush on a directory fd
	lock, err := openVerified(dir, mergeAuditLockName, unix.O_RDWR|unix.O_CREAT)
	if err != nil {
		return err
	}
	defer lock.Close() //nolint:errcheck // closing releases the flock
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		return fmt.Errorf("worker: lock %s: %w", mergeAuditLockName, err)
	}
	existing, err := readAtMax(dir, mergeAuditName, MaxMergeAuditBytes)
	if err != nil {
		return err
	}
	data, err := fn(existing)
	if err != nil || len(data) == 0 {
		return err
	}
	if data[len(data)-1] != '\n' {
		return errors.New("worker: audit lines must end in a newline")
	}
	if len(existing)+len(data) > MaxMergeAuditBytes {
		return ErrMergeAuditFull
	}
	return appendAt(dir, mergeAuditName, data)
}
