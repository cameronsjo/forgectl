//go:build aix

package config

import "errors"

// errLockUnsupportedAIX is returned by both lock entry points on AIX.
// golang.org/x/sys/unix has no Flock there, so lock_unix.go cannot build, and
// unlike lock_other.go's no-op pass-through this fallback fails CLOSED: a
// caller asking for mutual exclusion on a platform that cannot provide it is
// refused rather than told it holds a lock it does not.
var errLockUnsupportedAIX = errors.New("config: advisory file locking is unsupported on aix; refusing to run the critical section unlocked")

// WithFileLock refuses: there is no flock on AIX to serialize writers with.
func WithFileLock(_ string, _ func() error) error {
	return errLockUnsupportedAIX
}

// WithFileLockNotify refuses for the same reason as WithFileLock.
func WithFileLockNotify(_ string, _ func(), _ func() error) error {
	return errLockUnsupportedAIX
}
