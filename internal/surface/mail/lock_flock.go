//go:build darwin || linux || freebsd || netbsd || openbsd || dragonfly

package mail

import (
	"errors"
	"os"
	"syscall"
	"time"
)

// lockWait bounds how long a sender or flusher waits for another one. A flush
// can hold the lock across several deliveries, each bounded by its adapter's
// timeout, so this is generous.
const lockWait = 30 * time.Second

// lockFile takes an exclusive advisory lock on path, creating it mode 0600.
// The returned func releases it. Locks are per open file, so never nest two
// lockFile calls on the same path in one process.
func lockFile(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600) //nolint:gosec // G304: path is a fixed lock name inside the ledger directory
	if err != nil {
		return nil, err
	}
	fd := int(f.Fd())
	deadline := time.Now().Add(lockWait)
	for {
		err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			_ = f.Close()
			return nil, err
		}
		if time.Now().After(deadline) {
			_ = f.Close()
			return nil, ErrLockTimeout
		}
		time.Sleep(25 * time.Millisecond)
	}
	return func() {
		_ = syscall.Flock(fd, syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}
