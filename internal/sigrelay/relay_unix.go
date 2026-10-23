//go:build unix

package sigrelay

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

// settleWait is how long a re-raise waits for the default action to take.
var settleWait = time.Second

// interruptSignals are the terminating signals Group passes on to the child
// it runs in a process group of its own: SIGINT (Ctrl-C, which the terminal
// sends to the foreground group only), SIGTERM and SIGHUP (a closed terminal
// or a dropped SSH session).
var interruptSignals = []os.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP}

// interruptible returns ctx cancelled when one of interruptSignals reaches
// the process, and the stop that ends the watch. When a signal fired, stop
// re-raises it after it stops watching, so it meets whatever handles it
// next: the default, which ends the process by it, as it ended forgectl
// when the child shared forgectl's group; or a handler someone else registered,
// a concurrent Group call or a command's own scope, which then decides.
// It waits up to a second for the re-raised signal to take, then returns.
// It skips the wait when ctx was cancelled from above: whoever cancelled it
// is already unwinding and lives on by design, so the second would be lost
// on every later call (#1020: 8 sequential stops took 8s).
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
	parent := ctx
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
				if !errors.Is(parent.Err(), context.Canceled) {
					time.Sleep(settleWait)
				}
			}
		})
	}
}
