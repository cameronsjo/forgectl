//go:build unix

package worker

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/cameronsjo/forgectl/internal/privdir"
)

func (d DrainFiles) pin(create bool) (int, error) {
	return fileStore{stateBase: d.stateBase}.pin(create)
}

// DrainLock is a held drain.lock. Closing it releases the lock; so does the
// process ending, however it ends.
type DrainLock struct {
	f *os.File
}

// Close releases the lock.
func (l *DrainLock) Close() error {
	if l == nil || l.f == nil {
		return nil
	}
	return l.f.Close()
}

// Lock takes drain.lock with LOCK_EX|LOCK_NB. A lock another process holds
// is ErrDrainLocked at once; nothing waits.
func (d DrainFiles) Lock() (*DrainLock, error) {
	dir, err := d.pin(true)
	if err != nil {
		return nil, fmt.Errorf("worker: drain directory: %w", err)
	}
	defer unix.Close(dir) //nolint:errcheck // nothing to flush on a directory fd
	f, err := openVerified(dir, drainLockName, unix.O_RDWR|unix.O_CREAT)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close() //nolint:errcheck,gosec // not held; nothing written
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, ErrDrainLocked
		}
		return nil, fmt.Errorf("worker: lock %s: %w", drainLockName, err)
	}
	return &DrainLock{f: f}, nil
}

// ReadStatus returns drain.json, or nil when there is none.
func (d DrainFiles) ReadStatus() ([]byte, error) {
	return d.read(drainStatusName)
}

// WriteStatus replaces drain.json atomically. The caller holds the lock.
func (d DrainFiles) WriteStatus(_ *DrainLock, data []byte) error {
	if len(data) > maxLedgerBytes {
		return fmt.Errorf("worker: drain status is %d bytes, over the %d-byte cap", len(data), maxLedgerBytes)
	}
	dir, err := d.pin(true)
	if err != nil {
		return fmt.Errorf("worker: drain directory: %w", err)
	}
	defer unix.Close(dir) //nolint:errcheck // nothing to flush on a directory fd
	return writeAt(dir, drainStatusTmpName, drainStatusName, data)
}

// AppendEvent appends one line to drain-events.jsonl with O_APPEND. When the
// line would take the file past MaxDrainEventsBytes the file is first
// renamed to drain-events.jsonl.1, replacing the previous one. The caller
// holds the lock, so only one process appends and rotates.
func (d DrainFiles) AppendEvent(_ *DrainLock, line []byte) error {
	if len(line) == 0 || line[len(line)-1] != '\n' {
		return errors.New("worker: a drain event must be one line ending in a newline")
	}
	if len(line) > MaxDrainEventsBytes {
		return fmt.Errorf("worker: a drain event of %d bytes is over the %d-byte file cap", len(line), MaxDrainEventsBytes)
	}
	dir, err := d.pin(true)
	if err != nil {
		return fmt.Errorf("worker: drain directory: %w", err)
	}
	defer unix.Close(dir) //nolint:errcheck // nothing to flush on a directory fd
	// The size is read from the verified file, so a symlink or another
	// user's file is refused before anything is rotated.
	f, err := openVerified(dir, drainEventsName, unix.O_WRONLY|unix.O_CREAT|unix.O_APPEND)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	f.Close() //nolint:errcheck,gosec // only stat was read; appendAt reopens it
	if err != nil {
		return fmt.Errorf("worker: stat %s: %w", drainEventsName, err)
	}
	if st.Size()+int64(len(line)) > MaxDrainEventsBytes {
		if err := unix.Renameat(dir, drainEventsName, dir, drainEventsOldName); err != nil {
			return fmt.Errorf("worker: rotate %s: %w", drainEventsName, err)
		}
	}
	return appendAt(dir, drainEventsName, line)
}

// ReadEvents returns drain-events.jsonl.1 and drain-events.jsonl, each nil
// when absent.
func (d DrainFiles) ReadEvents() (older, current []byte, err error) {
	if older, err = d.read(drainEventsOldName); err != nil {
		return nil, nil, err
	}
	if current, err = d.read(drainEventsName); err != nil {
		return nil, nil, err
	}
	return older, current, nil
}

// AppendUsage appends data, whole lines ending in a newline, to
// usage-daily.jsonl with O_APPEND. The file is never rotated or truncated.
func (d DrainFiles) AppendUsage(data []byte) error {
	if len(data) == 0 || data[len(data)-1] != '\n' {
		return errors.New("worker: usage lines must end in a newline")
	}
	dir, err := d.pin(true)
	if err != nil {
		return fmt.Errorf("worker: drain directory: %w", err)
	}
	defer unix.Close(dir) //nolint:errcheck // nothing to flush on a directory fd
	return appendAt(dir, usageDailyName, data)
}

// appendAt appends data to name under dir with O_APPEND and syncs it before
// returning, through openVerified. drain-events.jsonl and usage-daily.jsonl
// both go through it, so both sync: a line the caller was told is written is
// on disk, which the usage file needs (it is the only record of a pruned
// row's cost), and events are written once per condition, rarely enough that
// the sync costs nothing that matters.
func appendAt(dir int, name string, data []byte) error {
	f, err := openVerified(dir, name, unix.O_WRONLY|unix.O_CREAT|unix.O_APPEND)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close() //nolint:errcheck,gosec // already failing
		return fmt.Errorf("worker: append %s: %w", name, err)
	}
	if err := f.Sync(); err != nil {
		f.Close() //nolint:errcheck,gosec // already failing
		return fmt.Errorf("worker: sync %s: %w", name, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("worker: close %s: %w", name, err)
	}
	return nil
}

// ReadUsage returns usage-daily.jsonl, or nil when there is none.
func (d DrainFiles) ReadUsage() ([]byte, error) {
	return d.read(usageDailyName)
}

// ReadPruneDay returns the UTC day the drain last ran its daily prune, or
// "" when it never has.
func (d DrainFiles) ReadPruneDay() (string, error) {
	data, err := d.read(drainPruneDayName)
	if err != nil || data == nil {
		return "", err
	}
	day := strings.TrimSuffix(string(data), "\n")
	if err := checkDay(day); err != nil {
		return "", fmt.Errorf("%w: %s: %w", ErrLedgerUnreadable, drainPruneDayName, err)
	}
	return day, nil
}

// WritePruneDay records day as the last daily prune, atomically.
func (d DrainFiles) WritePruneDay(day string) error {
	if err := checkDay(day); err != nil {
		return err
	}
	dir, err := d.pin(true)
	if err != nil {
		return fmt.Errorf("worker: drain directory: %w", err)
	}
	defer unix.Close(dir) //nolint:errcheck // nothing to flush on a directory fd
	return writeAt(dir, drainPruneDayTmpName, drainPruneDayName, []byte(day+"\n"))
}

func (d DrainFiles) read(name string) ([]byte, error) {
	dir, err := d.pin(false)
	if errors.Is(err, privdir.ErrAbsent) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("worker: drain directory: %w", err)
	}
	defer unix.Close(dir) //nolint:errcheck // read-only descriptor
	return readAt(dir, name)
}
