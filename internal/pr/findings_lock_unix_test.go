//go:build unix

package pr

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFindingsRemove_LockBusyRefusesAndKeepsDir(t *testing.T) {
	sessions := t.TempDir()
	dir := t.TempDir()
	holder := lockClient(t, sessions, time.Second)
	c := New(nil, WithFindingsDir(dir), WithSessionsDir(sessions), WithLockWait(200*time.Millisecond))
	target := filepath.Join(dir, findingsDirPrefix+"busy")
	mustMkdir(t, target)

	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = holder.withLifecycleLock(t.Context(), "holder-verb", func() error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered

	removed, err := c.FindingsRemove(t.Context(), []string{target})
	close(release)
	<-done

	var busy *lockBusyError
	if !errors.As(err, &busy) {
		t.Fatalf("err = %v, want *lockBusyError", err)
	}
	if len(removed) != 0 {
		t.Errorf("removed = %v, want nothing under a busy lock", removed)
	}
	if _, serr := os.Stat(target); serr != nil {
		t.Errorf("dir %q was removed under a busy lock: %v", target, serr)
	}
	if rows := auditRows(t, c); len(rows) != 0 {
		t.Errorf("audit rows = %+v, want none when the lock was never taken", rows)
	}
}

// TestFindingsRemove_CtxCancelledWhileLockBusy: a cancelled ctx ends the
// wait for a busy lock. context.Canceled is the evidence: a wait that ignored
// the ctx would run out its 30 s and report the busy lock instead. The
// elapsed check is only a hang bound, well under that wait and far above any
// working run, so host load cannot fail it (forgectl#919; it was 5 s).
//
// Mutation: drop the ctx.Done arm from withLifecycleLock's wait, and the
// call waits 30 s and reports the busy lock.
func TestFindingsRemove_CtxCancelledWhileLockBusy(t *testing.T) {
	sessions := t.TempDir()
	dir := t.TempDir()
	holder := lockClient(t, sessions, time.Second)
	// A long wait, so only the ctx can end the acquisition inside the test.
	c := New(nil, WithFindingsDir(dir), WithSessionsDir(sessions), WithLockWait(30*time.Second))
	target := filepath.Join(dir, findingsDirPrefix+"cancelled")
	mustMkdir(t, target)

	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = holder.withLifecycleLock(t.Context(), "holder-verb", func() error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	start := time.Now()
	removed, err := c.FindingsRemove(ctx, []string{target})
	elapsed := time.Since(start)
	close(release)
	<-done

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if elapsed > 20*time.Second {
		t.Errorf("FindingsRemove took %s under a cancelled ctx, want a return well inside the 30s lock wait", elapsed)
	}
	if len(removed) != 0 {
		t.Errorf("removed = %v, want nothing under a cancelled ctx", removed)
	}
	if _, serr := os.Stat(target); serr != nil {
		t.Errorf("dir %q was removed under a cancelled ctx: %v", target, serr)
	}
	if rows := auditRows(t, c); len(rows) != 0 {
		t.Errorf("audit rows = %+v, want none when the lock was never taken", rows)
	}
}
