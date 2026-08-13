//go:build unix

package cli

import (
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestConfigWriterLock_FIFOConfigRefusesWithoutBlockingOrRendering(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := unix.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}

	var renders atomic.Int32
	done := make(chan error, 1)
	go func() {
		_, err := updateConfigLocked(path, nativeConfigWriterOps(), func(raw []byte) ([]byte, error) {
			renders.Add(1)
			return raw, nil
		})
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("updateConfigLocked accepted a FIFO config")
		}
	case <-time.After(time.Second):
		t.Fatal("updateConfigLocked blocked on a FIFO config")
	}
	if got := renders.Load(); got != 0 {
		t.Fatalf("render callback count = %d, want 0", got)
	}
}
