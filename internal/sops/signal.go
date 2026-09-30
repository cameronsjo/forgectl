package sops

import (
	"errors"
	"os"
	"os/signal"
	"sync"
	"syscall"
)

// # Why a scoped handler, and not a cancelled context
//
// Nothing upstream catches the terminating signals: fang installs
// signal.NotifyContext only when given WithNotifySignal, and forgectl's root
// never passes it, so each keeps Go's default disposition and kills the process
// with every defer unrun. That is the defect this closes: a signal while the work directory holds
// a plaintext value skips `defer work.cleanup()` and leaves the secret beside
// the target, INSIDE the repository, where `git add -A` will commit it.
//
// A context-based cleanup was the other shape considered, and rejected. It
// needs either a NotifyContext at the root, which changes the signal semantics
// of every command in the binary to cover one span in one, or a scoped one
// here, which turns a Ctrl-C into "keep running until the next step that reads
// ctx" — and stage, ReadTarget and the work-dir writes read none, while the two
// sops calls would run on to their 60s and 30s deadlines with the plaintext
// still on disk and a second Ctrl-C swallowed. The operator asked the process
// to stop; this handler removes the secret and then stops it.
//
// The handler deliberately does NOT restore the target. A signal can land
// while the sops child is still running (it is not in its own process group,
// so a terminal Ctrl-C reaches it too, but a SIGTERM aimed at forgectl does
// not), and a restore racing a writer is worse than none. Removing the work
// directory also takes the editor's value file away, so a sops child that
// outlives forgectl finds nothing to write and refuses.
//
// # Keeping the backup when the target may have changed
//
// Not restoring used to mean losing the way back. The handler removed the
// whole work directory, ciphertext backup included, so a signal after sops
// wrote the target and before forgectl verified it left an unverified value
// with nothing but git to return to (cameronsjo/forgectl#560). Now, inside the
// span where the target may differ from the backup (from the moment the sops
// edit is launched until the run settles, meaning verified or restore proven),
// the handler removes the plaintext files first, then renames the backup out to
// `.forgectl-sops-<tag>.backup` beside the target, and then removes the rest.
// The rename stays inside one directory, so it cannot fail with EXDEV. The
// next write to that target finds the backup through the leftover scan and
// refuses, pointing at it. Ciphertext on disk exposes nothing the target
// itself does not. Outside that span the target is untouched, or is proven,
// so keeping a backup would only cause a false refusal.
//
// Something may already sit at that name. The leftover scan refuses on one
// under the lock, so it was put there during this run, outside the lock. The
// guard never moves the backup over it and never deletes it. It keeps the
// work directory instead, with the backup and its .gitignore as the only
// entries, rather than deleting the one copy of the pre-run ciphertext
// (cameronsjo/forgectl#692). The cost is a directory that a sops child
// outliving forgectl can still write into: a read-back still running can
// leave the decrypted value there. The directory is gitignored, and the next
// write's leftover scan refuses on it, names the backup inside, and warns
// that it may hold plaintext.
//
// # What it cannot cover
//
// SIGKILL and SIGSTOP cannot be caught, a power loss runs no code, and a
// terminating signal missing from guardedSignals is not seen; any of them can
// still leave the directory behind. The directory's name is scoped to the
// target, so the next write to that target finds it under the lock and
// refuses, naming it (internal/env's scanLeftovers). Nothing deletes it
// automatically: a sops child that outlived its parent may still be using it.

// plaintextGuard runs the work directory's cleanup when one of guardedSignals
// arrives inside the span it is armed for, then terminates the process with
// that signal.
//
// Its lifetime is exactly the span: armed before the work directory exists,
// released after the directory is removed, on every path including a panic.
type plaintextGuard struct {
	ch   chan os.Signal
	stop func(chan<- os.Signal)
	// die terminates the process for sig. It never returns in production;
	// tests inject one that records and returns.
	die func(os.Signal)

	// mu orders the handler against track. A signal that arrives while the
	// work directory is being created waits for it to exist, so it is always
	// removed; a signal that fires first marks the guard so no directory is
	// ever created after it.
	mu    sync.Mutex
	work  *workDir
	fired bool
	// mutating is true while the target may differ from the backup: from just
	// before the sops edit is launched until settle. A signal inside that span
	// keeps the ciphertext backup.
	mutating bool

	once sync.Once
	// kept is where the ciphertext backup ended up when finish kept it, or
	// "" when it did not. Written inside once, read after it.
	kept string

	done   chan struct{}
	exited chan struct{}
}

// errInterrupted is returned by track when a signal has already fired. In
// production the process is gone before anyone reads it.
var errInterrupted = errors.New("interrupted before the work directory was created")

// armPlaintextGuard registers for guardedSignals and starts the handler.
//
// A signal the process inherited as IGNORED stays ignored. signal.Notify on
// an ignored signal re-enables it, and a job that was told to survive one
// must not become killable by it for the length of this span: nohup ignores
// SIGHUP, and a non-interactive shell starts a background job with SIGINT and
// SIGQUIT ignored.
func armPlaintextGuard() *plaintextGuard {
	var sigs []os.Signal
	for _, s := range guardedSignals {
		if !signal.Ignored(s) {
			sigs = append(sigs, s)
		}
	}
	// Buffered by one: signal.Notify never blocks on a send, so an
	// unbuffered channel drops a signal that lands while nobody is receiving.
	ch := make(chan os.Signal, 1)
	if len(sigs) > 0 {
		signal.Notify(ch, sigs...)
	}
	return startPlaintextGuard(ch, signal.Stop, terminateBySignal)
}

// startPlaintextGuard is the seam the unit tests drive with an injected
// channel, stop, and die.
func startPlaintextGuard(ch chan os.Signal, stop func(chan<- os.Signal), die func(os.Signal)) *plaintextGuard {
	g := &plaintextGuard{
		ch:     ch,
		stop:   stop,
		die:    die,
		done:   make(chan struct{}),
		exited: make(chan struct{}),
	}
	go g.watch()
	return g
}

func (g *plaintextGuard) watch() {
	defer close(g.exited)
	select {
	case sig := <-g.ch:
		g.fire(sig)
	case <-g.done:
	}
}

// track creates the work directory under the guard's lock and records it, so
// there is no instant at which the directory exists and the handler does not
// know about it.
func (g *plaintextGuard) track(create func() (*workDir, error)) (*workDir, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.fired {
		return nil, errInterrupted
	}
	w, err := create()
	if err != nil {
		return nil, err
	}
	g.work = w
	return w, nil
}

// beginMutation opens the span in which the target may differ from the
// backup. Call it immediately before launching anything that can write the
// target. It takes the lock, so a signal is handled wholly before it or wholly
// after it.
func (g *plaintextGuard) beginMutation() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.mutating = true
}

// settle closes that span once the target is proven: verified, or restored
// and the restore proven. A signal after this point has nothing to preserve.
func (g *plaintextGuard) settle() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.mutating = false
}

// cleanup removes the work directory exactly once, whoever gets there first.
// sync.Once also makes the loser WAIT for the winner: the deferred cleanup
// cannot return while a handler-driven removal is half done, and the handler
// cannot terminate the process while a deferred removal is half done.
//
// It retries until the directory is confirmed gone. On the signal path the
// main goroutine (or the sops child) is still running, and os.RemoveAll lists
// the entries, unlinks them, then removes the directory: a file created after
// the last listing fails that final rmdir with ENOTEMPTY and the directory
// survives. Once the directory itself is gone, every later write into it fails
// with ENOENT — nothing but track creates it — so a bounded retry converges.
//
// This is the path a normal return takes. It keeps the ciphertext backup when
// the return came from inside the mutation span, meaning the target was never
// verified and no restore was proven: a failed restore, or a panic. Before
// cameronsjo/forgectl#652 it kept nothing, so a restore that failed returned
// "could NOT be restored" and then deleted the one copy that could.
func (g *plaintextGuard) cleanup() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.mutating {
		g.finish(keepCiphertext)
		return
	}
	g.finish(discardAll)
}

// keepBackup is for a failure path that returns while the target is
// unproven. It keeps the ciphertext backup, removes every plaintext file, and
// reports where the backup now is so the error can name it, or "" if it could
// not be kept. The deferred cleanup that follows finds the work already done.
func (g *plaintextGuard) keepBackup() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.finish(keepCiphertext)
}

// finishMode is what finish does with the ciphertext backup.
type finishMode int

const (
	// discardAll removes the whole work directory: the target is untouched,
	// or it is proven.
	discardAll finishMode = iota
	// keepCiphertext removes the plaintext, then moves the backup out beside the
	// target and removes the directory. When the backup cannot be moved out,
	// because something already sits at that name, the directory stays with
	// the backup and its .gitignore as its only entries, on a signal or on a
	// normal return. Two edge cases still lose it, because a kept directory
	// must never also keep plaintext: a directory that will not prune down to
	// the backup is removed whole, and one holding an entry that cannot be
	// deleted loses everything else, the backup included, and stays behind
	// (docs/commands/env.md).
	//
	// On a normal return the runner has waited for every sops child, so
	// nothing writes into a kept directory afterwards; a panic inside the
	// runner is the exception. On a signal a sops child may still be running,
	// and a read-back that outlives forgectl can write the decrypted value
	// into it. Either way the directory is gitignored and the next run's
	// leftover scan refuses on it, naming the backup inside and warning that
	// it may hold plaintext (cameronsjo/forgectl#692).
	keepCiphertext
)

// finish is cleanup, with the choice of keeping the ciphertext backup. The
// plaintext goes first, the backup is moved out second, and the directory is
// removed last. Once the plaintext files are gone, nothing that follows can
// expose them. It returns where the backup was kept, or "".
func (g *plaintextGuard) finish(mode finishMode) string {
	g.once.Do(func() {
		if g.work == nil {
			return
		}
		if mode == keepCiphertext {
			g.work.discardStagedValue()
			g.work.discardLandedValue()
			if g.work.preserveBackup() {
				g.kept = g.work.keep
			} else if g.pruneToBackup() {
				g.kept = g.work.backup
				return
			}
		}
		for range cleanupAttempts {
			g.work.cleanup()
			if _, err := os.Lstat(g.work.dir); errors.Is(err, os.ErrNotExist) {
				return
			}
		}
	})
	return g.kept
}

// pruneToBackup prunes the work directory down to its backup, retrying on the
// same bound as the removal: on the signal path a sops child may create an
// entry between the prune's removal and its check. A directory that will not
// come down to the backup is removed whole, backup included, because a kept
// directory must never also keep plaintext.
func (g *plaintextGuard) pruneToBackup() bool {
	for range cleanupAttempts {
		if pruneWorkDir(g.work) {
			return true
		}
	}
	return false
}

// pruneWorkDir is one prune attempt. It is a variable only so a test can make
// the first attempts lose the race a sops child would cause, which nothing
// else can do deterministically, and so prove the retry above is there.
var pruneWorkDir = (*workDir).pruneToBackup

// cleanupAttempts bounds the retry in cleanup. Each attempt that fails lost a
// race with a single concurrent create, so a handful is ample; the bound only
// keeps a directory that cannot be removed at all (permissions changed under
// us) from spinning forever.
const cleanupAttempts = 16

// fire runs on a caught signal: remove the plaintext, keep the ciphertext
// backup if the target may have changed, then die.
func (g *plaintextGuard) fire(sig os.Signal) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.fired = true
	mode := discardAll
	if g.mutating {
		mode = keepCiphertext
	}
	g.finish(mode)
	g.die(sig)
}

// release ends the span. It must run AFTER cleanup (defer it first, so it
// runs last), on every path.
//
// The order is what keeps a late signal from being swallowed. The handler is
// stopped first and waited for, so it can neither leak nor race what follows;
// then signal delivery is stopped; then any signal that reached the channel in
// between is acted on here, since after this function returns nobody reads
// the channel and the default disposition would never see it.
func (g *plaintextGuard) release() {
	close(g.done)
	<-g.exited
	g.stop(g.ch)
	select {
	case sig := <-g.ch:
		g.fire(sig)
	default:
	}
}

// exitStatusFor is the conventional 128+N status for a caught signal (130 for
// SIGINT, 143 for SIGTERM, 129 for SIGHUP, 131 for SIGQUIT, 134 for SIGABRT). It is the
// fallback where re-raising is unavailable or did not take. Derived from the
// signal number rather than tabled, so a signal added to guardedSignals cannot
// ship without a status.
func exitStatusFor(sig os.Signal) int {
	if s, ok := sig.(syscall.Signal); ok {
		return 128 + int(s)
	}
	return 1
}
