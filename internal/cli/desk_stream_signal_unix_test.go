// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

//go:build unix

package cli

import (
	"os/signal"
	"syscall"
	"testing"
	"time"
)

// stubDeskRaise records the signal deskInterruptible re-raises instead of
// raising it at the test process.
func stubDeskRaise(t *testing.T) *[]syscall.Signal {
	t.Helper()
	var raised []syscall.Signal
	prev := deskRaise
	deskRaise = func(s syscall.Signal) { raised = append(raised, s) }
	t.Cleanup(func() { deskRaise = prev })
	return &raised
}

// A signal cancels the context, reads as fired, and is raised again once
// the watch stops, so forgectl still ends by it.
func TestDeskInterruptible_FiresAndReRaises(t *testing.T) {
	raised := stubDeskRaise(t)
	ctx, fired, stop := deskInterruptible(t.Context())
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("SIGTERM did not cancel the context")
	}
	if !fired() {
		t.Error("fired() = false after SIGTERM")
	}
	stop()
	if len(*raised) != 1 || (*raised)[0] != syscall.SIGTERM {
		t.Errorf("re-raised %v, want SIGTERM once", *raised)
	}
}

// With no signal, stop raises nothing.
func TestDeskInterruptible_QuietStopRaisesNothing(t *testing.T) {
	raised := stubDeskRaise(t)
	ctx, fired, stop := deskInterruptible(t.Context())
	stop()
	if fired() || len(*raised) != 0 || ctx.Err() == nil {
		t.Errorf("fired=%v raised=%v ctx=%v; want no signal, and the context ended by stop", fired(), *raised, ctx.Err())
	}
}

// A signal inherited as ignored (nohup's SIGHUP) stays ignored: it neither
// cancels the context nor is re-enabled.
func TestDeskInterruptible_KeepsAnIgnoredSignalIgnored(t *testing.T) {
	raised := stubDeskRaise(t)
	signal.Ignore(syscall.SIGHUP)
	t.Cleanup(func() { signal.Reset(syscall.SIGHUP) })
	ctx, fired, stop := deskInterruptible(t.Context())
	defer stop()
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if ctx.Err() != nil || fired() {
		t.Error("an ignored SIGHUP cancelled the context")
	}
	if !signal.Ignored(syscall.SIGHUP) {
		t.Error("watching re-enabled the ignored SIGHUP")
	}
	stop()
	if len(*raised) != 0 {
		t.Errorf("re-raised %v for an ignored signal", *raised)
	}
}
