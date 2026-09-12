//go:build unix

package pr

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// lifecycleLockName is the advisory lock every session-record read-decide-write
// takes, as a sibling inside the sessions dir. It is never a .json, so List's
// enumeration never sees it.
const lifecycleLockName = ".pr-session-lifecycle.lock"

const (
	defaultLockWait  = 10 * time.Second
	lockPollInterval = 100 * time.Millisecond
)

// lockBusyError is the bounded-wait timeout. It carries the lock path and
// whatever holder body was on disk, so the operator's next question — what is
// holding it — is answered by the error rather than by guessing at processes.
type lockBusyError struct {
	path   string
	holder string
	waited time.Duration
}

func (e *lockBusyError) Error() string {
	holder := "holder unknown"
	if e.holder != "" {
		holder = "held by " + e.holder
	}
	return fmt.Sprintf("lifecycle lock busy after %s: %s (%s) — check with 'forgectl pr list'; the lock releases when that process exits",
		e.waited.Round(time.Millisecond), termsafe.QuotePath(e.path), holder)
}

// withLifecycleLock runs fn while holding the exclusive lifecycle lock for
// this client's sessions dir.
//
// THE CONTRACT, stated once here and assumed everywhere:
//
//   - NON-REENTRANT. flock is per open file description and every call opens
//     its own, so a nested acquisition from inside fn blocks until the bounded
//     wait expires and then fails. Every verb that takes the lock is therefore
//     split into a locked shell and an unlocked *Locked core, and a composite
//     verb (repair, drain, cleanup) takes the lock ONCE and calls the cores.
//   - SHORT HOLDS ONLY. The lock covers record reads and writes and the
//     admission decision. It is never held across a clone, a gh call, or a
//     tmux new-window; the phase record is what bridges those.
//   - BOUNDED WAIT. flock has no timeout, so acquisition polls LOCK_NB every
//     lockPollInterval up to c.lockWait and then returns *lockBusyError.
//   - KERNEL RELEASE. Closing the descriptor releases the lock, including on
//     process death, so a crashed holder never wedges the next caller. The
//     holder body left in the file is diagnostic text, never a liveness claim.
//
// Two hosts sharing one $HOME (NFS, a synced volume) share this directory,
// and flock over NFS is advisory at best. Out of scope; stated so it is on the
// record rather than implied.
func (c *Client) withLifecycleLock(ctx context.Context, verb string, fn func() error) error {
	if err := os.MkdirAll(c.sessionsDir, 0o700); err != nil {
		return fmt.Errorf("create pr sessions dir: %w", err)
	}
	lockPath := filepath.Join(c.sessionsDir, lifecycleLockName)
	fd, err := unix.Open(lockPath, unix.O_CREAT|unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return fmt.Errorf("open lifecycle lock %s: %w", termsafe.QuotePath(lockPath), err)
	}
	f := os.NewFile(uintptr(fd), lockPath)
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat lifecycle lock %s: %w", termsafe.QuotePath(lockPath), err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("lifecycle lock %s is not a regular file; refusing", termsafe.QuotePath(lockPath))
	}

	wait := c.lockWait
	if wait <= 0 {
		wait = defaultLockWait
	}
	deadline := time.Now().Add(wait)
	for {
		err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			return fmt.Errorf("lock %s: %w", termsafe.QuotePath(lockPath), err)
		}
		if time.Now().After(deadline) {
			body, _ := os.ReadFile(lockPath) //nolint:gosec // our own lock file, diagnostic text only
			return &lockBusyError{path: lockPath, holder: strings.TrimSpace(termsafe.SafeLine(string(body))), waited: wait}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(lockPollInterval):
		}
	}
	defer func() { _ = unix.Flock(int(f.Fd()), unix.LOCK_UN) }()

	// The holder body is written only AFTER acquisition, so a refused caller
	// never overwrites the real holder's identity.
	host, _ := os.Hostname()
	body := fmt.Sprintf("pid=%d verb=%s started=%s host=%s\n",
		os.Getpid(), verb, time.Now().UTC().Format(time.RFC3339), host)
	if err := f.Truncate(0); err == nil {
		_, _ = f.WriteAt([]byte(body), 0)
	}

	return fn()
}
