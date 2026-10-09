//go:build !(darwin || linux || freebsd || netbsd || openbsd || dragonfly)

package mail

import "sync"

// Cross-session messaging runs on macOS and Linux. Where flock is unavailable the
// package still builds, and locking falls back to one process-wide mutex.
var fallbackMu sync.Mutex

func lockFile(path string) (func(), error) {
	_ = path
	fallbackMu.Lock()
	return fallbackMu.Unlock, nil
}
