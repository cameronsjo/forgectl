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
// the restart lock it waits (until ctx ends) instead of failing: under the
// watcher only a manual run can race it (launchd runs one instance of a job),
// and that manual run must still compare against whatever the watcher's run
// records, not give up. While held, the file names this pid and the start
// time (InFlight), so `launch doctor` can tell a stuck run from a live one.
func (s FileHookStore) Lock(ctx context.Context, onWait func()) (func(), error) {
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
	waited := false
	for {
		err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			// Best effort: the marker only feeds the doctor's stuck check.
			_ = f.Truncate(0)
			_, _ = f.WriteAt(fmt.Appendf(nil, "%d %d\n", os.Getpid(), time.Now().Unix()), 0)
			return func() {
				_ = f.Truncate(0) // an empty marker means no run is in flight
				// Closing the descriptor releases the flock; the kernel also
				// releases it when the process exits, however it exits.
				_ = f.Close()
			}, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) {
			_ = f.Close() // the lock was never taken; the flock error is the one to report
			return nil, fmt.Errorf("take the hooks lock: %w", err)
		}
		if !waited && onWait != nil {
			onWait()
		}
		waited = true
		if err := SleepContext(ctx, hookLockPoll); err != nil {
			_ = f.Close() // gave up waiting; nothing was locked
			return nil, fmt.Errorf("wait for another `forgectl resume hooks run` to finish: %w", err)
		}
	}
}

// InFlight reads the lock's marker: the pid and start time of the run that
// holds it, or ok=false when none is recorded. A run killed with SIGKILL
// leaves its marker behind, so a caller pairs this with launchd's own
// "running" state before calling a run stuck.
func (s FileHookStore) InFlight() (pid int, since time.Time, ok bool) {
	p, err := s.path(hookLockName)
	if err != nil {
		return 0, time.Time{}, false
	}
	data, err := os.ReadFile(p) // #nosec G304 -- fixed name under forgectl's own state dir
	if err != nil {
		return 0, time.Time{}, false
	}
	var unixSec int64
	if n, err := fmt.Sscanf(string(data), "%d %d", &pid, &unixSec); err != nil || n != 2 {
		return 0, time.Time{}, false
	}
	return pid, time.Unix(unixSec, 0), true
}

// openAppendNoFollow opens path for append, refusing a symlink.
func openAppendNoFollow(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600) // #nosec G304 -- fixed name under forgectl's own state dir
}
