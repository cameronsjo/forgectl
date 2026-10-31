//go:build unix

package workerwatch

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// A worker can swap report.md for a FIFO between the directory read and the
// open. The open must refuse it at once, not wait for a writer.
func TestBriefDirFSRefusesFIFOWithoutBlocking(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "report.md")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
	done := make(chan bool, 1)
	go func() { done <- endsComplete(briefDirFS{dir: dir}, "report.md", 0) }()
	select {
	case complete := <-done:
		if complete {
			t.Error("a FIFO counted as a complete report")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("opening a FIFO report blocked the scan")
	}
}

func TestBriefDirFSRefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.md")
	if err := os.WriteFile(outside, []byte("REPORT-COMPLETE\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "report.md")); err != nil {
		t.Fatal(err)
	}
	if endsComplete(briefDirFS{dir: dir}, "report.md", 16) {
		t.Error("a symlinked report was followed")
	}
	if r := ScanReportDir(dir); len(r.Files) != 0 {
		t.Errorf("a symlinked report was listed: %+v", r.Files)
	}
	if err := os.WriteFile(filepath.Join(dir, "report-real.md"), []byte("ok\nREPORT-COMPLETE\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := ScanReportDir(dir); len(r.Files) != 1 || !r.Files[0].Complete {
		t.Errorf("a real report on disk was not read as complete: %+v", r)
	}
}
