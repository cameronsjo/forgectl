//go:build unix

package pr

import (
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
