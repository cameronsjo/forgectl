//go:build unix

package pr

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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
//     admission decision. It is never held across network work (a clone, a gh
//     call) or a tmux new-window; the phase record is what bridges those.
//     ONE CARVE-OUT: an audited destructive verb may remove a local directory
//     under the hold — teardown's sandbox removal and `pr findings cleanup`
//     both do — because its intent row, the removal, and its completion row
//     must be atomic with respect to `pr repair --prune`, which compacts the
//     audit log by rename under this lock. The carve-out covers local removal
//     only; it never licenses the network or dispatch work above.
//     SECOND CARVE-OUT: tmux, bounded rather than excluded. Every tmux call
//     made under the hold draws on a lockedTmuxBudget (tmuxbudget.go), so a
//     hung tmux server holds the lock for the budget plus exec's
//     pipeWaitDelay (500 ms) per site, not indefinitely. The sites are
//     teardown's window kill (killReviewWindow, through resolveReviewWindow),
//     reached from `pr teardown`, from `pr cleanup`, and from `pr repair
//     --rollback` and `--forget-if-absent`; the occupancy read (reviewWindowSnapshot) in
//     admit, reserve, PrepareMany's batch reserve, and drain's claim; the
//     liveness read (WindowsLive, WindowLive) in `pr repair`'s inspect,
//     undecodable set-aside, rollback and forget arms and in `pr repair
//     --prune`'s screenLiveWindows; and `pr repair --adopt-window`'s
//     resolveReviewWindow.
//     Each fails closed on a timeout: an unreadable window list is "a window
//     may exist", so admission refuses, repair and prune refuse, and teardown
//     parks the record in needs-repair and removes nothing. The kill cannot
//     move out from under the lock: the window is found by the review's name,
//     so after release a new admission of the same ref could create a
//     same-named window and the kill would hit it. The local removal that
//     follows a kill (restore renames, os.RemoveAll) is os work with no
//     subprocess and no context, so it is bounded by neither; that is why it
//     is a carve-out rather than a budget. A tmux new-window is still never
//     issued under the hold.
//   - BOUNDED WAIT. flock has no timeout, so acquisition polls LOCK_NB every
//     lockPollInterval up to c.lockWait and then returns *lockBusyError.
//   - KERNEL RELEASE. Closing the descriptor releases the lock, including on
//     process death, so a crashed holder never wedges the next caller. The
//     holder body left in the file is diagnostic text, never a liveness claim.
//   - ADVISORY, SAME-UID. flock binds only processes that ask for it. The audit
//     log's append (last-byte separator check, write, and a short-write
//     rollback Truncate) is atomic only against holders of this lock; a
//     same-uid writer that skips it can interleave with the check, and a
//     rollback can then truncate that writer's bytes. That is an accepted
//     residual, not a gap to close: the writer already owns the 0700 dir and
//     every record and log in it, so it can forge or delete any row directly
//     and the lock was never a defense against it. The log's file-type check
//     (a FIFO or symlink is refused) covers the cases that could hang or
//     redirect the appender. (forgectl#570)
//
// Two hosts sharing one $HOME (NFS, a synced volume) share this directory,
// and flock over NFS is advisory at best. Out of scope; stated so it is on the
// record rather than implied.
func (c *Client) withLifecycleLock(ctx context.Context, verb string, fn func() error) error {
	if err := os.MkdirAll(c.sessionsDir, 0o700); err != nil {
		return fmt.Errorf("create pr sessions dir: %w", err)
	}
	// The lock's exclusivity and every 0600 record under it rest on the
	// directory itself being private. MkdirAll only sets the mode on a dir it
	// creates, so a pre-existing dir is asserted here rather than assumed.
	dirInfo, err := os.Stat(c.sessionsDir)
	if err != nil {
		return fmt.Errorf("stat pr sessions dir: %w", err)
	}
	if !dirInfo.IsDir() {
		return fmt.Errorf("pr sessions dir %s is not a directory", termsafe.QuotePath(c.sessionsDir))
	}
	// Group- or other-WRITABLE is the refusal, not merely accessible: a writer
	// is what can plant a record or a symlink under the lock, and a readable
	// dir gives away nothing the 0600 records do not already withhold. (A
	// stricter 0o077 test also refuses every t.TempDir, whose leaf is 0777
	// under umask.)
	if perm := dirInfo.Mode().Perm(); perm&0o022 != 0 {
		return fmt.Errorf("pr sessions dir %s is mode %04o and writable by others; run chmod 0700 on it",
			termsafe.QuotePath(c.sessionsDir), perm)
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
	if c.onLock != nil {
		c.onLock(verb, "acquire")
		defer c.onLock(verb, "release")
	}

	// The holder body is written only AFTER acquisition, so a refused caller
	// never overwrites the real holder's identity.
	host, _ := os.Hostname()
	body := fmt.Sprintf("pid=%d verb=%s started=%s host=%s\n",
		os.Getpid(), verb, time.Now().UTC().Format(time.RFC3339), host)
	// A failure here leaves the lock HELD and fn running — correct — but the
	// diagnostic body the next contender's timeout reads would be stale, so
	// the failure is logged rather than swallowed.
	if err := f.Truncate(0); err != nil {
		slog.Warn("Failed to truncate the lifecycle lock body; the holder text a later timeout reports may be stale.",
			"path", lockPath, "error", err)
	} else if _, err := f.WriteAt([]byte(body), 0); err != nil {
		slog.Warn("Failed to write the lifecycle lock holder body; a later timeout will report an unknown holder.",
			"path", lockPath, "error", err)
	}

	return fn()
}
