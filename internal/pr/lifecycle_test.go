//go:build unix

package pr

// Test plan for lifecycle_unix.go (forgectl#299 Task 1)
//
// withLifecycleLock (Classification: cross-process advisory lock, bounded wait)
//   [x] Happy: fn runs, the lock file exists 0600 and carries the holder body
//   [x] Two Clients on one sessions dir: the second waits while the first holds,
//       then proceeds once the first releases (kernel release on close)
//   [x] Unhappy: a held lock times out with an error carrying the lock path,
//       the holder body, and the next step
//   [x] Unhappy: a nested acquisition from inside fn times out — the lock is
//       non-reentrant by construction, and this test documents it
//   [x] Refusal asserts zero mutation: a timed-out caller never rewrites the
//       holder body

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/exec"
)

func lockClient(t *testing.T, dir string, wait time.Duration) *Client {
	t.Helper()
	return New(&exec.FakeRunner{}, WithSessionsDir(dir), WithFindingsDir(t.TempDir()), WithLockWait(wait))
}

func TestLifecycleLock_RunsFnAndWritesHolder(t *testing.T) {
	dir := t.TempDir()
	c := lockClient(t, dir, time.Second)
	ran := false
	err := c.withLifecycleLock(context.Background(), "test-verb", func() error {
		ran = true
		body, err := os.ReadFile(filepath.Join(dir, lifecycleLockName)) //nolint:gosec // test-owned temp dir
		if err != nil {
			t.Fatalf("read lock body while held: %v", err)
		}
		for _, want := range []string{"pid=", "verb=test-verb", "started=", "host="} {
			if !strings.Contains(string(body), want) {
				t.Errorf("lock body %q lacks %q", body, want)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("withLifecycleLock: %v", err)
	}
	if !ran {
		t.Fatal("fn did not run")
	}
	info, err := os.Stat(filepath.Join(dir, lifecycleLockName))
	if err != nil {
		t.Fatalf("lock file after release: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("lock file mode = %o, want 0600", got)
	}
}

func TestLifecycleLock_SecondHolderWaitsThenProceeds(t *testing.T) {
	dir := t.TempDir()
	first := lockClient(t, dir, time.Second)
	second := lockClient(t, dir, 5*time.Second)

	entered := make(chan struct{})
	release := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = first.withLifecycleLock(context.Background(), "first", func() error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered

	done := make(chan error, 1)
	go func() {
		done <- second.withLifecycleLock(context.Background(), "second", func() error { return nil })
	}()
	select {
	case err := <-done:
		t.Fatalf("second holder acquired while first held: err=%v", err)
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	wg.Wait()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("second holder after release: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("second holder never acquired after the first released")
	}
}

func TestLifecycleLock_TimeoutNamesHolderAndPath(t *testing.T) {
	dir := t.TempDir()
	first := lockClient(t, dir, time.Second)
	second := lockClient(t, dir, 250*time.Millisecond)

	entered := make(chan struct{})
	release := make(chan struct{})
	go func() {
		_ = first.withLifecycleLock(context.Background(), "holder-verb", func() error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered
	defer close(release)

	err := second.withLifecycleLock(context.Background(), "second", func() error {
		t.Fatal("fn ran under a lock that should have timed out")
		return nil
	})
	var busy *lockBusyError
	if !errors.As(err, &busy) {
		t.Fatalf("err = %v, want *lockBusyError", err)
	}
	msg := err.Error()
	for _, want := range []string{filepath.Join(dir, lifecycleLockName), "verb=holder-verb", "forgectl pr list"} {
		if !strings.Contains(msg, want) {
			t.Errorf("timeout error %q lacks %q", msg, want)
		}
	}
	// Zero mutation on refusal: the body still names the first holder.
	body, _ := os.ReadFile(filepath.Join(dir, lifecycleLockName)) //nolint:gosec // test-owned temp dir
	if !strings.Contains(string(body), "verb=holder-verb") {
		t.Errorf("lock body rewritten by a refused caller: %q", body)
	}
}

func TestLifecycleLock_RefusesAnOtherWritableSessionsDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o777); err != nil { //nolint:gosec // the point of the test is a world-writable dir
		t.Fatal(err)
	}
	c := lockClient(t, dir, time.Second)
	err := c.withLifecycleLock(context.Background(), "test", func() error {
		t.Fatal("fn ran under a world-writable sessions dir")
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "chmod 0700") {
		t.Fatalf("err = %v, want a refusal naming chmod 0700", err)
	}
	if _, lerr := os.Lstat(filepath.Join(dir, lifecycleLockName)); !errors.Is(lerr, os.ErrNotExist) {
		t.Errorf("a refused acquisition must create no lock file")
	}
}

func TestLifecycleLock_NestedAcquisitionIsRefused(t *testing.T) {
	dir := t.TempDir()
	c := lockClient(t, dir, 200*time.Millisecond)
	var inner error
	err := c.withLifecycleLock(context.Background(), "outer", func() error {
		inner = c.withLifecycleLock(context.Background(), "inner", func() error { return nil })
		return nil
	})
	if err != nil {
		t.Fatalf("outer: %v", err)
	}
	var busy *lockBusyError
	if !errors.As(inner, &busy) {
		t.Fatalf("nested acquisition = %v, want *lockBusyError (the lock is non-reentrant)", inner)
	}
}
