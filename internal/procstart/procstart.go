// Package procstart reads a process's start time, so a recorded pid can be
// told apart from a later process that reused it.
//
// It is a copy of internal/desk's processAlive with a stricter rule for
// signalling: [Matches] answers true only when a start time was recorded and
// the live process's start time can be read and equals it. desk's check
// treats an unreadable start time as alive, which is right for "is this run
// still going" and wrong for "may I send this pid SIGTERM".
package procstart

import "errors"

var (
	// ErrNoProcess reports a pid with no live process.
	ErrNoProcess = errors.New("procstart: no such process")
	// ErrUnsupported reports a platform with no readable start time.
	ErrUnsupported = errors.New("procstart: process start time is not available on this platform")
	// ErrUnreadable reports a start time the kernel will not show this user.
	ErrUnreadable = errors.New("procstart: process start time is not readable")
	// ErrZombie reports a process that has exited and not been reaped.
	ErrZombie = errors.New("procstart: the process has exited (zombie)")
)

// Of returns pid's start time: microseconds since the epoch on darwin, clock
// ticks since boot on linux. The value is only ever compared with another
// value from the same machine, never shown as a time.
func Of(pid int) (int64, error) {
	if pid <= 0 {
		return 0, ErrNoProcess
	}
	start, zombie, err := procStart(pid)
	if err != nil {
		return 0, err
	}
	if zombie {
		return 0, ErrZombie
	}
	return start, nil
}

// Matches reports whether pid is alive and is the process that recorded
// start. A zero start (none recorded), an unreadable live start time, and a
// different one all answer false: the caller must not signal the pid.
func Matches(pid int, start int64) (bool, error) {
	if start == 0 {
		return false, errors.New("procstart: no start time was recorded")
	}
	if !signalZero(pid) {
		return false, ErrNoProcess
	}
	got, err := Of(pid)
	if err != nil {
		return false, err
	}
	return got == start, nil
}
