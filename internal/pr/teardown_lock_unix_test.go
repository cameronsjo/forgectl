//go:build unix

package pr

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/exec"
)

// This file holds the teardown tests that depend on the Unix lifecycle lock's
// busy error (lockBusyError, lifecycle_unix.go), so the rest of
// teardown_test.go still builds, and vets, off Unix.

// TestTeardown_KillWindowRunsUnderTheLifecycleLock pins the #556 decision to
// keep the kill under the lock rather than release it first: with the lock
// released, a new admission of the same ref could create a same-named window
// that the kill would then take out. From inside the kill, a second client
// must find the lock busy.
func TestTeardown_KillWindowRunsUnderTheLifecycleLock(t *testing.T) {
	ref := Ref{Owner: "o", Repo: "r", Number: 22}
	server := reviewServer(mustWindowName(t, ref))
	inner := server.RunFunc
	dir := t.TempDir()
	var other *Client
	var probeErr error
	probed := false
	fake := &exec.FakeRunner{}
	fake.RunFunc = func(name string, args []string) (string, error) {
		if name == "tmux" && len(args) > 0 && args[0] == "kill-window" {
			probed = true
			probeErr = other.withLifecycleLock(context.Background(), "probe", func() error { return nil })
		}
		return inner(name, args)
	}
	opts := []Option{WithSessionsDir(dir), WithFindingsDir(t.TempDir()),
		WithApprover(func(string) (bool, error) { return false, nil }),
		WithTTYCheck(func() bool { return false })}
	c := New(fake, opts...)
	other = New(&exec.FakeRunner{}, append(opts, WithLockWait(50*time.Millisecond))...)
	path, _ := seedSession(t, c, ref, time.Now().UTC())

	if err := c.Teardown(context.Background(), path); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	if !probed {
		t.Fatal("kill-window never ran; the test did not exercise the lock")
	}
	var busy *lockBusyError
	if !errors.As(probeErr, &busy) {
		t.Errorf("a second client acquired the lock during the kill (err = %v); the kill has left the lock", probeErr)
	}
}
