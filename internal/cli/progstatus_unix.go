// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

//go:build unix

package cli

import (
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/cameronsjo/forgectl/internal/progstatus"
)

// signalReportDeadline bounds the status write on a signal. A terminal that
// has stopped reading stderr (Ctrl-S, a stalled SSH session) would otherwise
// block the write, and with it the re-raise, so forgectl would outlive a
// SIGTERM it used to die from at once.
const signalReportDeadline = 250 * time.Millisecond

// catchableSignals returns the signals of SIGINT and SIGTERM that ignored
// does not report as ignored. Notify would take an ignored signal out of its
// ignored state, and the later Reset puts the ignore back, so the re-raise
// would do nothing while an error status had already gone out.
func catchableSignals(ignored func(os.Signal) bool) []os.Signal {
	var sigs []os.Signal
	for _, s := range []os.Signal{syscall.SIGINT, syscall.SIGTERM} {
		if !ignored(s) {
			sigs = append(sigs, s)
		}
	}
	return sigs
}

// reportOnSignal installs a scoped SIGINT/SIGTERM handler that writes an error
// report for name, then restores the default disposition and re-raises the
// signal, so forgectl still dies by it exactly as before. It deliberately does
// not touch any context: forgectl#788 declined a root signal context, and
// children still get the terminal's Ctrl-C from the process group. With status
// off, or with both signals ignored, it installs nothing.
//
// The handler owns the exit. Children get the same Ctrl-C and usually return
// first, so the command's own return path would reach os.Exit(1) before the
// re-raise; the returned stop func therefore waits for a handler that has
// already taken a signal, and re-raises one still queued, rather than letting
// the command return normally.
func reportOnSignal(r *progstatus.Reporter, name string) (stop func()) {
	if !r.Enabled() {
		return func() {}
	}
	sigs := catchableSignals(signal.Ignored)
	if len(sigs) == 0 {
		return func() {}
	}
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, sigs...)
	done := make(chan struct{})
	var (
		mu       sync.Mutex
		stopped  bool
		handling bool
	)
	reraise := func(sig os.Signal) {
		signal.Reset(sig)
		_ = syscall.Kill(os.Getpid(), sig.(syscall.Signal))
	}
	go func() {
		select {
		case sig := <-ch:
			mu.Lock()
			if stopped {
				// The command already returned: nothing left to report.
				mu.Unlock()
				reraise(sig)
				return
			}
			handling = true
			mu.Unlock()
			// If the write blocks, re-raise anyway once the deadline passes.
			timer := time.AfterFunc(signalReportDeadline, func() { reraise(sig) })
			r.Error(name + " interrupted")
			timer.Stop()
			reraise(sig)
		case <-done:
		}
	}()
	return func() {
		signal.Stop(ch)
		close(done)
		mu.Lock()
		stopped = true
		taken := handling
		mu.Unlock()
		if taken {
			// The handler holds a signal and will end the process; do not let
			// the command's own exit path win the race.
			time.Sleep(4 * signalReportDeadline)
			return
		}
		// A signal that arrived just before Stop may still sit in ch.
		select {
		case sig := <-ch:
			reraise(sig)
		default:
		}
	}
}

// reportBlocked reports blocked for kind and msg, runs fn, then clears the row
// on every return path, a panic included. It also reports an interrupt: a
// Ctrl-C during a presence prompt leaves an error row rather than a stale
// blocked one.
func reportBlocked(r *progstatus.Reporter, kind, msg string, fn func() error) error {
	r.Blocked(kind, msg)
	stop := reportOnSignal(r, msg)
	defer stop()
	defer r.Clear()
	return fn()
}
