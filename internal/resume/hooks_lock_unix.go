//go:build unix

package resume

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/unix"

	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// hookLockPoll is how often a waiting run retries the hooks lock.
const hookLockPoll = 500 * time.Millisecond

// Lock implements HookStore: an exclusive flock on <Dir>/hooks.lock. Unlike
// the restart lock it waits (until ctx ends) instead of failing: a second run
// was triggered by a later change, and once the first run records its
// version the second must still compare against it, not give up.
func (s FileHookStore) Lock(ctx context.Context) (func(), error) {
	p, err := s.path(hookLockName)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("create %s: %w", termsafe.QuotePath(s.Dir), termsafe.Error(err))
	}
	// O_NOFOLLOW: a planted hooks.lock symlink is refused rather than followed.
	f, err := os.OpenFile(p, os.O_CREATE|os.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600) // #nosec G304 -- fixed name under forgectl's own state dir
	if err != nil {
		return nil, fmt.Errorf("open the hooks lock: %w", termsafe.Error(err))
	}
	for {
		err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			// Closing the descriptor releases the flock; the kernel also
			// releases it when the process exits, however it exits.
			return func() { _ = f.Close() }, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) {
			_ = f.Close() // the lock was never taken; the flock error is the one to report
			return nil, fmt.Errorf("take the hooks lock: %w", err)
		}
		if err := SleepContext(ctx, hookLockPoll); err != nil {
			_ = f.Close() // gave up waiting; nothing was locked
			return nil, fmt.Errorf("wait for another `forgectl resume hooks run` to finish: %w", err)
		}
	}
}

// openAppendNoFollow opens path for append, refusing a symlink.
func openAppendNoFollow(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600) // #nosec G304 -- fixed name under forgectl's own state dir
}
