//go:build unix

package cli

import (
	"path/filepath"
	"syscall"
	"testing"
)

// A FIFO named as a brief file is refused before it is opened, because the
// open itself would block until a writer appeared.
func TestReadBriefArgRefusesAFIFO(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	if _, err := readBriefArg("@" + fifo); err == nil {
		t.Fatal("a FIFO was read as a brief")
	}
}
