//go:build unix

package gitenv

import (
	"context"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

// interruptSignals are the terminating signals Bounded passes on to the git
// it runs in a process group of its own: SIGINT (Ctrl-C, which the terminal
// sends to the foreground group only), SIGTERM and SIGHUP (a closed terminal
// or a dropped SSH session).
var interruptSignals = []os.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP}

// interruptible returns ctx cancelled when one of interruptSignals reaches
// the process, and the stop that ends the watch. When a signal fired, stop
// re-raises it after it stops watching, so it meets whatever handles it
// next: the default, which ends the process by it, as it ended forgectl
// when git shared forgectl's group; or a handler someone else registered,
// a concurrent Bounded call or a command's own scope, which then decides.
// It waits up to a second for the re-raised signal to take, then returns.
//
// A signal the process inherited as ignored stays ignored, as in
// internal/sops: nohup ignores SIGHUP, and a non-interactive shell starts a
// background job with SIGINT ignored. signal.Notify on one would re-enable
// it. With every signal ignored, ctx is returned as it is.
func interruptible(ctx context.Context) (context.Context, func()) {
	var sigs []os.Signal
	for _, s := range interruptSignals {
		if !signal.Ignored(s) {
			sigs = append(sigs, s)
		}
	}
	if len(sigs) == 0 {
		return ctx, func() {}
	}
	ctx, cancel := context.WithCancel(ctx)
	// Buffered by one: signal.Notify never blocks on a send.
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, sigs...)
	done := make(chan struct{})
	var (
		wg  sync.WaitGroup
		got os.Signal
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		select {
		case got = <-ch:
			cancel()
		case <-done:
		}
	}()
	var once sync.Once
	return ctx, func() {
		once.Do(func() {
			close(done)
			wg.Wait()
			signal.Stop(ch)
			cancel()
			if got == nil {
				// One that landed after the watch ended but before Stop.
				select {
				case got = <-ch:
				default:
				}
			}
			if s, ok := got.(syscall.Signal); ok {
				_ = syscall.Kill(syscall.Getpid(), s)
				time.Sleep(time.Second)
			}
		})
	}
}
