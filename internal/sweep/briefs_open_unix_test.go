//go:build unix

package sweep

import (
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// A FIFO in place of a brief file must be refused, not waited on.
func TestReadBriefsDoesNotHangOnAFifo(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "cos-fifo")
	put(t, dir, "brief.md", briefBody, t0)
	if err := syscall.Mkfifo(filepath.Join(dir, "report.md"), 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, _, err := ReadBriefs(root, "cos-", t0.Add(-time.Hour)); err != nil {
			t.Error(err)
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("ReadBriefs blocked on a FIFO")
	}
}
