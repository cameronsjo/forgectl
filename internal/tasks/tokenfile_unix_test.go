//go:build unix

package tasks

// syscall.Mkfifo is unix-only, so this test lives apart from tokenfile_test.go
// to keep GOOS=windows go vet ./... compiling (#810).

import (
	"errors"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestReadTokenFile_RefusesANonRegularFile: Stat reports size 0 for a FIFO,
// so the size ceiling passes and os.ReadFile then BLOCKS FOREVER waiting for a
// writer — hanging startup before the listener ever opens, which presents as a
// container that never becomes healthy rather than as a bad credential path.
func TestReadTokenFile_RefusesANonRegularFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skipf("cannot create a FIFO here: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := ReadTokenFile(path)
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, ErrTokenNotFound) {
			t.Fatalf("ReadTokenFile(fifo) = %v, want errors.Is(ErrTokenNotFound)", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ReadTokenFile blocked on a FIFO instead of refusing it")
	}
}
