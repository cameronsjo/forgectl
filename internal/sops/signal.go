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
// # What it cannot cover
//
// SIGKILL and SIGSTOP cannot be caught, a power loss runs no code, and a
// terminating signal missing from guardedSignals is not seen; any of them can
// still leave the directory behind. That residual is tracked in
// cameronsjo/forgectl#520 (a sweep of stale work directories), not here.

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

	once sync.Once

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
func (g *plaintextGuard) cleanup() {
	g.once.Do(func() {
		if g.work == nil {
			return
		}
		for range cleanupAttempts {
			g.work.cleanup()
			if _, err := os.Lstat(g.work.dir); errors.Is(err, os.ErrNotExist) {
				return
			}
		}
	})
}

// cleanupAttempts bounds the retry in cleanup. Each attempt that fails lost a
// race with a single concurrent create, so a handful is ample; the bound only
// keeps a directory that cannot be removed at all (permissions changed under
// us) from spinning forever.
const cleanupAttempts = 16

// fire runs on a caught signal: remove the plaintext, then die.
func (g *plaintextGuard) fire(sig os.Signal) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.fired = true
	g.cleanup()
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
// SIGINT, 143 for SIGTERM, 129 for SIGHUP, 131 for SIGQUIT). It is the
// fallback where re-raising is unavailable or did not take. Derived from the
// signal number rather than tabled, so a signal added to guardedSignals cannot
// ship without a status.
func exitStatusFor(sig os.Signal) int {
	if s, ok := sig.(syscall.Signal); ok {
		return 128 + int(s)
	}
	return 1
}
