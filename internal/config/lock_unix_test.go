//go:build unix

package config

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/cameronsjo/forgectl/internal/termsafe"
)

func TestWithFileLockNotify_UncontendedNeverAnnouncesAWait(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x")
	waited, ran := false, false
	err := WithFileLockNotify(path, func() { waited = true }, func() error { ran = true; return nil })
	if err != nil || !ran || waited {
		t.Errorf("err=%v ran=%v waited=%v, want nil, true, false", err, ran, waited)
	}
}

func TestWithFileLockNotify_ContendedAnnouncesThenBlocksUntilReleased(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x")
	held, release := make(chan struct{}), make(chan struct{})
	holderDone := make(chan error, 1)
	go func() {
		holderDone <- WithFileLock(path, func() error {
			close(held)
			<-release
			return nil
		})
	}()
	select {
	case <-held:
	case <-time.After(5 * time.Second):
		t.Fatal("holder never acquired the lock")
	}

	waiting, entered := make(chan struct{}), make(chan struct{})
	waiterDone := make(chan error, 1)
	go func() {
		waiterDone <- WithFileLockNotify(path, func() { close(waiting) }, func() error {
			close(entered)
			return nil
		})
	}()
	select {
	case <-waiting:
	case <-time.After(5 * time.Second):
		t.Fatal("a contended lock never announced the wait")
	}
	select {
	case <-entered:
		t.Fatal("the waiter ran while the holder still held the lock")
	case <-time.After(150 * time.Millisecond):
	}

	close(release)
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the waiter never ran after the holder released")
	}
	if err := <-waiterDone; err != nil {
		t.Errorf("waiter: %v", err)
	}
	if err := <-holderDone; err != nil {
		t.Errorf("holder: %v", err)
	}
}

func TestConfigWriteLock_NonregularLeafRefusesWithoutCallback(t *testing.T) {
	tests := []struct {
		name string
		make func(t *testing.T, path string) func()
	}{
		{
			name: "symlink",
			make: func(t *testing.T, path string) func() {
				t.Helper()
				target := filepath.Join(filepath.Dir(path), "target")
				if err := os.WriteFile(target, nil, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
				return func() {}
			},
		},
		{
			name: "directory",
			make: func(t *testing.T, path string) func() {
				t.Helper()
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
				return func() {}
			},
		},
		{
			name: "fifo",
			make: func(t *testing.T, path string) func() {
				t.Helper()
				if err := unix.Mkfifo(path, 0o600); err != nil {
					t.Fatal(err)
				}
				return func() {}
			},
		},
		{
			name: "unix socket",
			make: func(t *testing.T, path string) func() {
				t.Helper()
				ln, err := net.Listen("unix", path)
				if err != nil {
					if errors.Is(err, unix.EPERM) {
						t.Skip("sandbox does not permit Unix socket creation")
					}
					t.Fatal(err)
				}
				return func() { _ = ln.Close() }
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir, err := os.MkdirTemp("/tmp", "forgectl-lock-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(dir) })
			configPath := filepath.Join(dir, "config.toml")
			cleanup := tt.make(t, configPath+".lock")
			defer cleanup()
			calls := 0
			err = WithFileLock(configPath, func() error {
				calls++
				return nil
			})
			if err == nil {
				t.Fatal("WithFileLock() = nil, want refusal")
			}
			if calls != 0 {
				t.Fatalf("callback calls = %d, want 0", calls)
			}
		})
	}
}

// TestWithFileLock_OpenErrorQuotesAndCapsTheLockPath is #847 item 6: the lock
// errors printed the lock path raw beside the wrapped error. A directory at
// the lock path makes the open fail.
//
// Mutation that turns it red: print lockPath with a bare %s again in the
// "open lock file" error.
func TestWithFileLock_OpenErrorQuotesAndCapsTheLockPath(t *testing.T) {
	dir := filepath.Join(t.TempDir(), strings.Repeat("a", 200), strings.Repeat("b", 200), strings.Repeat("c", 200))
	path := filepath.Join(dir, "rlo\u202econfig.toml")
	lockPath := path + ".lock"
	if err := os.MkdirAll(lockPath, 0o750); err != nil {
		t.Fatal(err)
	}
	err := WithFileLock(path, func() error { t.Fatal("fn ran without the lock"); return nil })
	if err == nil {
		t.Fatal("WithFileLock over a directory lock path returned no error")
	}
	msg := err.Error()
	if strings.Contains(msg, lockPath) || strings.Contains(msg, "\u202e") {
		t.Errorf("the lock path reached the message raw: %q", msg)
	}
	if want := "open lock file " + termsafe.QuotePath(lockPath) + ": "; !strings.HasPrefix(msg, want) {
		t.Errorf("message %q does not lead with the capped, quoted lock path", msg)
	}
}
