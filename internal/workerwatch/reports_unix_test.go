//go:build unix

package workerwatch

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/exec"
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

func TestScanReportDirRefusesASymlinkedDir(t *testing.T) {
	realDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(realDir, "report.md"), []byte("REPORT-COMPLETE\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "brief")
	if err := os.Symlink(realDir, link); err != nil {
		t.Fatal(err)
	}
	if r := ScanReportDir(link); r.Error != "the brief dir is a symlink" || len(r.Files) != 0 {
		t.Errorf("a symlinked brief dir was read: %+v", r)
	}
}

// A stub hc first on PATH stands in for `hc events`: it records its pid,
// prints a snapshot and one event, then sleeps like an idle subscription.
// Once Done closes after cancel, that process must be gone.
func TestEventsChildIsReapedAfterCancel(t *testing.T) {
	bin := t.TempDir()
	pidFile := filepath.Join(t.TempDir(), "pid")
	stub := "#!/bin/sh\necho $$ > " + pidFile + "\necho '{\"terminals\":[]}'\necho '{\"params\":{\"type\":\"status\"}}'\nexec sleep 30\n"
	if err := os.WriteFile(filepath.Join(bin, "hc"), []byte(stub), 0o700); err != nil { //nolint:gosec // G306: a test stub that must be executable
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	ctx, cancel := context.WithCancel(context.Background())
	es := CLI{Source: &fakeSource{events: func(ctx context.Context, w io.Writer) error {
		return exec.OSRunner{}.RunStreamingScrubbed(ctx, w, io.Discard, filepath.Join(bin, "hc"), "events", "--json")
	}}}.Events(ctx)
	select {
	case <-es.Wake:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("the stub hc events never woke the watch")
	}
	raw, err := os.ReadFile(pidFile) //nolint:gosec // G304: the test's own temp file
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	cancel()
	select {
	case <-es.Done:
	case <-time.After(5 * time.Second):
		t.Fatal("Done never closed after cancel")
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Errorf("the hc events child (pid %d) is still alive after Done: kill(0) = %v", pid, err)
	}
}
