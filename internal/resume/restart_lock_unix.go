//go:build unix

package resume

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// lockRestart takes the run lock: an exclusive, non-blocking flock on
// <storeDir>/restart.lock. The kernel drops a flock when its holder exits,
// however it exits, so a crashed run never leaves the lock stuck.
func lockRestart(storeDir string) (func(), error) {
	if storeDir == "" {
		return nil, errors.New("no forgectl state directory for the restart lock")
	}
	if err := os.MkdirAll(storeDir, 0o700); err != nil {
		return nil, fmt.Errorf("create the restart lock directory: %w", err)
	}
	path := filepath.Join(storeDir, restartLockName)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600) // #nosec G304 -- fixed name under forgectl's own state dir
	if err != nil {
		return nil, fmt.Errorf("open the restart lock: %w", err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = f.Close() // the lock was never taken; nothing to release
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, ErrRestartBusy
		}
		return nil, fmt.Errorf("take the restart lock: %w", err)
	}
	return func() {
		// Closing the descriptor releases the flock; an error here leaves
		// nothing to act on, and the kernel releases it at exit regardless.
		_ = f.Close()
	}, nil
}
