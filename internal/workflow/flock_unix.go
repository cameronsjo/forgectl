//go:build unix

package workflow

import (
	"errors"
	"os"
	"syscall"
)

// flockTryExclusive attempts a non-blocking exclusive advisory lock on f.
// It returns (true, nil) when the lock was taken, (false, nil) when another
// holder has it (EWOULDBLOCK), and a non-nil error for anything else. flock
// locks are keyed to the open file description, so two independent opens of the
// same path — even within one process — contend, which is exactly the
// concurrent-run case we guard. Stdlib syscall keeps this off the x/sys direct
// dependency (it is only transitively present).
func flockTryExclusive(f *os.File) (bool, error) {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return false, nil
	}
	return false, err
}

// flockUnlock releases the advisory lock on f.
func flockUnlock(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
