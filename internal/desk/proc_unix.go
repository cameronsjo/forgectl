//go:build unix

package desk

import (
	"errors"

	"golang.org/x/sys/unix"
)

var (
	errNoProcess            = errors.New("desk: no such process")
	errProcStartUnsupported = errors.New("desk: process start time is not available on this platform")
	errProcUnreadable       = errors.New("desk: process start time is not readable")
)

// processAlive reports whether pid is still the process that recorded start.
// A pid that now belongs to a different process (its start time differs) or
// is a zombie reads as dead. start 0 means none was recorded, so only the pid
// is checked.
func processAlive(pid int, start int64) bool {
	if pid <= 0 {
		return false
	}
	if err := unix.Kill(pid, 0); errors.Is(err, unix.ESRCH) {
		return false
	}
	got, zombie, err := procStart(pid)
	switch {
	case errors.Is(err, errProcStartUnsupported), errors.Is(err, errProcUnreadable):
		// Signal 0 found the pid and nothing more can be learned: alive. A
		// live run read as dead could be skipped while it runs.
		return true
	case err != nil:
		return false
	case zombie:
		return false
	}
	return start == 0 || got == start
}

// ownStart is this process's start time for meta.PIDStart, or 0 when the
// platform cannot report one.
func ownStart(pid int) int64 {
	start, _, err := procStart(pid)
	if err != nil {
		return 0
	}
	return start
}
