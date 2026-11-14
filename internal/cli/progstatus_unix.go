// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

//go:build unix

package cli

import (
	"os"
	"os/signal"
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
// report for name, then re-raises the signal, so forgectl still dies by it
// exactly as before. It deliberately does not touch any context: forgectl#788
// declined a root signal context, and children still get the terminal's Ctrl-C
// from the process group. With status off, or with both signals ignored, it
// installs nothing.
//
// The handler owns the exit. Children get the same Ctrl-C and usually return
// first, so the command's own return path would reach os.Exit(1) before the
// re-raise; the returned stop func therefore waits for a handler that has
// taken a signal rather than letting the command return normally.
//
// The re-raise stops this handler's channel instead of resetting the signal, so
// an in-process guard that also subscribed (sops's plaintext cleanup) still
// receives it and owns the death. Do not enclose such a guard casually: it
// would then decide when forgectl exits.
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
	exited := make(chan struct{}) // closed only when the goroutine ends by done
	reraise := func(sig os.Signal) {
		signal.Stop(ch)
		_ = syscall.Kill(os.Getpid(), sig.(syscall.Signal))
	}
	go func() {
		select {
		case sig := <-ch:
			// If the write blocks, re-raise anyway once the deadline passes.
			timer := time.AfterFunc(signalReportDeadline, func() { reraise(sig) })
			r.Error(name + " interrupted")
			timer.Stop()
			reraise(sig)
		case <-done:
			close(exited)
		}
	}()
	return func() {
		signal.Stop(ch)
		close(done)
		select {
		case <-exited:
			// The goroutine saw done. A signal queued before Stop is still ours.
			select {
			case sig := <-ch:
				reraise(sig)
			default:
			}
		case <-time.After(4 * signalReportDeadline):
			// The goroutine took a signal and is ending the process; it never
			// reaches exited. Do not let the command's exit path win the race.
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
