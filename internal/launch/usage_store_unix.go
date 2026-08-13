//go:build unix

package launch

import (
	"errors"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

func init() {
	appendUsageRow = appendUsageRowUnix
	openUsageReader = openUsageReaderUnix
	inspectUsageStore = inspectUsageStoreUnix
}

// usageWrite is a seam so tests can force a short or failing write without a
// full filesystem. Production is a plain write.
var usageWrite = func(f *os.File, b []byte) (int, error) { return f.Write(b) }

// appendUsageRowUnix appends one already-encoded, already-bounded row.
//
// The lock is exclusive and NONBLOCKING: this runs microseconds before
// syscall.Exec, and a launch that waited on a peer's lock would turn a
// statistics feature into a latency bug. A contended write is dropped, and the
// count is a count of recorded attempts, not of attempts made.
func appendUsageRowUnix(base string, row []byte) error {
	stateFD, err := pinUsageLeaf(base, true)
	if err != nil {
		return err
	}
	defer unix.Close(stateFD) //nolint:errcheck // read-only directory descriptor

	lock, err := openUsageFileAt(stateFD, usageLockName, unix.O_RDWR, true)
	if err != nil {
		return err
	}
	defer lock.Close() //nolint:errcheck // lock file carries no buffered data
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) {
			return ErrUsageBusy
		}
		return unsafeStore("lock the usage store: %s", err)
	}
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN) //nolint:errcheck // closing releases it anyway

	data, err := openUsageFileAt(stateFD, usageDataName, unix.O_RDWR|unix.O_APPEND, true)
	if err != nil {
		return err
	}
	defer data.Close() //nolint:errcheck // the single write below is checked directly

	if err := repairUsageTail(data); err != nil {
		return err
	}
	// One write for the whole row: a reader then sees either a complete row or
	// an unterminated fragment it can isolate, never a row spliced into
	// another.
	n, err := usageWrite(data, row)
	if err != nil {
		return err
	}
	if n != len(row) {
		return io.ErrShortWrite
	}
	// No fsync. Durability of a statistics row is not worth stalling an exec,
	// and a lost tail is already a recoverable case by construction.
	return nil
}

// repairUsageTail inspects only the final byte and, if a previous writer died
// mid-row, terminates that fragment with a newline so this row starts on a
// clean boundary. It never scans or rewrites the log.
//
// If the repair itself is short or fails, the new row is NOT attempted: two
// fragments joined into one line would read as a single corrupt row instead of
// two isolated ones, and a later writer can still repair the first.
func repairUsageTail(data *os.File) error {
	info, err := data.Stat()
	if err != nil {
		return unsafeStore("stat the usage log: %s", err)
	}
	if info.Size() == 0 {
		return nil
	}
	last := make([]byte, 1)
	if _, err := data.ReadAt(last, info.Size()-1); err != nil {
		return err
	}
	if last[0] == '\n' {
		return nil
	}
	n, err := usageWrite(data, []byte("\n"))
	if err != nil {
		return err
	}
	if n != 1 {
		return io.ErrShortWrite
	}
	return nil
}
