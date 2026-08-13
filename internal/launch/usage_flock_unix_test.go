//go:build unix

package launch

import (
	"os"

	"golang.org/x/sys/unix"
)

// flockExclusive lets a test hold the store lock the same way a peer process
// would, so contention is exercised through the real advisory lock rather than
// a stubbed error.
func flockExclusive(file *os.File) error {
	return unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
}
