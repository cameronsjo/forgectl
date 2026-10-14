// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

//go:build unix

package cli

import (
	"context"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

// deskStreamSignals are the terminating signals that stop a stream `desk
// show` reads: SIGINT (Ctrl-C), SIGTERM, and SIGHUP (a closed terminal).
var deskStreamSignals = []os.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP}

// deskRaise re-raises a signal at forgectl once the stream is stopped, so
// the process still ends by it. A seam for the tests.
var deskRaise = func(s syscall.Signal) {
	_ = syscall.Kill(syscall.Getpid(), s)
	time.Sleep(time.Second) // let the default action take
}

// deskInterruptible returns ctx cancelled when one of deskStreamSignals
// reaches the process, so a --follow-cmd command is stopped (its own
// session gets no terminal signal) rather than left running. fired reports
// whether a signal did it, so the caller can return quietly instead of
// reporting a cancelled context. stop ends the watch and, when a signal
// fired, re-raises it: forgectl then ends by that signal as it would have
// with no watch. Defer stop before the stream's close, so the close runs
// first. This follows internal/gitenv's interruptible.
//
// A signal the process inherited as ignored stays ignored: nohup ignores
// SIGHUP, and a non-interactive shell starts a background job with SIGINT
// ignored. signal.Notify on one would re-enable it.
func deskInterruptible(ctx context.Context) (_ context.Context, fired func() bool, stop func()) {
	var sigs []os.Signal
	for _, s := range deskStreamSignals {
		if !signal.Ignored(s) {
			sigs = append(sigs, s)
		}
	}
	if len(sigs) == 0 {
		return ctx, func() bool { return false }, func() {}
	}
	ctx, cancel := context.WithCancel(ctx)
	ch := make(chan os.Signal, 1) // buffered: Notify never blocks on a send
	signal.Notify(ch, sigs...)
	var (
		mu  sync.Mutex
		got os.Signal
		wg  sync.WaitGroup
	)
	done := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		select {
		case s := <-ch:
			mu.Lock()
			got = s
			mu.Unlock()
			cancel()
		case <-done:
		}
	}()
	fired = func() bool {
		mu.Lock()
		defer mu.Unlock()
		return got != nil
	}
	var once sync.Once
	stop = func() {
		once.Do(func() {
			close(done)
			wg.Wait()
			signal.Stop(ch)
			cancel()
			mu.Lock()
			s := got
			mu.Unlock()
			// A signal can sit in ch unread: the watcher took done when both
			// were ready, or it arrived after the watcher left and before
			// Stop. Drain it so it is re-raised, not swallowed.
			if s == nil {
				select {
				case s = <-ch:
				default:
				}
			}
			if sig, ok := s.(syscall.Signal); ok {
				deskRaise(sig)
			}
		})
	}
	return ctx, fired, stop
}
