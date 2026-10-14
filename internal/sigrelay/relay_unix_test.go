//go:build unix

package sigrelay

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"syscall"
	"testing"
	"time"
)

// These tests change process-wide signal state, so none runs in parallel.

// A terminating signal cancels Group's context, and cancel re-raises it
// once however often it is called. The test's own listener stands in for
// whatever handles the re-raised signal next, so the test binary lives.
// Mutations: drop the watch (the context never ends); drop the once (a
// second cancel re-raises again and the listener sees two).
func TestGroupSignalCancelsAndCancelIsIdempotent(t *testing.T) {
	own := make(chan os.Signal, 4)
	signal.Notify(own, syscall.SIGHUP)
	t.Cleanup(func() { signal.Stop(own) })

	ctx, cancel := Group(context.Background(), time.Minute)
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("SIGHUP did not cancel the context")
	}
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Errorf("ctx.Err() = %v, want context.Canceled", ctx.Err())
	}
	select {
	case <-own: // the original delivery
	case <-time.After(5 * time.Second):
		t.Fatal("the test's own listener never saw SIGHUP")
	}
	cancel()
	cancel()
	got := 0
	for done := false; !done; {
		select {
		case <-own:
			got++
		case <-time.After(200 * time.Millisecond):
			done = true
		}
	}
	if got != 1 {
		t.Errorf("cancel re-raised the signal %d times, want once", got)
	}
}

// A signal the process inherited as ignored stays ignored: signal.Notify on
// it would re-enable it. Mutation: watch every interrupt signal regardless.
func TestGroupLeavesAnIgnoredSignalIgnored(t *testing.T) {
	signal.Ignore(syscall.SIGHUP)
	t.Cleanup(func() { signal.Reset(syscall.SIGHUP) })

	_, cancel := Group(context.Background(), time.Minute)
	defer cancel()
	if !signal.Ignored(syscall.SIGHUP) {
		t.Error("Group re-enabled an ignored SIGHUP")
	}
}

// Without a signal, the context ends at the deadline and cancel neither
// re-raises nor waits. Mutation: re-raise unconditionally (cancel takes a
// second, and the process gets a signal no one sent).
func TestGroupDeadlineWithoutSignal(t *testing.T) {
	ctx, cancel := Group(context.Background(), 10*time.Millisecond)
	<-ctx.Done()
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Errorf("ctx.Err() = %v, want context.DeadlineExceeded", ctx.Err())
	}
	start := time.Now()
	cancel()
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Errorf("cancel took %v with no signal to re-raise", d)
	}
}
