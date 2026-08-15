//go:build unix

package history

import (
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestRead_RefusesNonRegularFiles pins the kind check. A fifo is the case
// that matters: without the check, os.Open blocks forever waiting for a
// writer, so the failure mode is a hang rather than a wrong answer — hence the
// deadline.
func TestRead_RefusesNonRegularFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fifo")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		// Skipped rather than failed only when the platform cannot make a
		// fifo at all; on every platform this suite actually runs on it can,
		// so the check is exercised rather than quietly waived.
		t.Skipf("mkfifo unavailable: %v", err)
	}

	type result struct{ err error }
	done := make(chan result, 1)
	go func() {
		_, err := Read(path)
		done <- result{err}
	}()

	select {
	case got := <-done:
		if got.err == nil {
			t.Fatal("Read accepted a fifo as a history file")
		}
		if !strings.Contains(got.err.Error(), "not a regular file") {
			t.Errorf("Read error = %v, want it to name the file kind", got.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Read blocked on a fifo — the kind check must run before the open")
	}
}
