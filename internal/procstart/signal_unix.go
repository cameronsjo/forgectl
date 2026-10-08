//go:build unix

package procstart

import (
	"errors"

	"golang.org/x/sys/unix"
)

// signalZero reports whether pid names a live process this user may signal.
// EPERM means the pid now belongs to another user, so it was reused.
func signalZero(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := unix.Kill(pid, 0)
	return !errors.Is(err, unix.ESRCH) && !errors.Is(err, unix.EPERM)
}
