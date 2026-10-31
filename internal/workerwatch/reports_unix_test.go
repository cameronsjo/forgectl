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
	go func() { done <- endsComplete(openedBriefDir(t, dir), "report.md", 0) }()
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
	if endsComplete(openedBriefDir(t, dir), "report.md", 16) {
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

// openedBriefDir opens dir the way ScanReportDir does, closed at cleanup.
func openedBriefDir(t *testing.T, dir string) briefDirFS {
	t.Helper()
	d, err := openBriefDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return briefDirFS{dir: d}
}

// TestBriefDirSwappedAfterOpenIsNotFollowed: once the brief dir is open, a
// worker that moves it away and puts a symlink to another directory at its
// path changes nothing the scan reads: the listing and the report opens
// stay on the directory that was opened, and so do the size and mtime
// the listing reports.
func TestBriefDirSwappedAfterOpenIsNotFollowed(t *testing.T) {
	base := t.TempDir()
	brief := filepath.Join(base, "brief")
	if err := os.Mkdir(brief, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(brief, "report.md"), []byte("working\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	when := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	if err := os.Chtimes(filepath.Join(brief, "report.md"), when, when); err != nil {
		t.Fatal(err)
	}
	// The swapped-in dir has no report.md, so a size or mtime read by path
	// (os.File.ReadDir's own Info) would drop the report from the row.
	other := t.TempDir()
	for _, name := range []string{"report-a.md", "report-b.md"} {
		if err := os.WriteFile(filepath.Join(other, name), []byte("REPORT-COMPLETE\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	fsys := openedBriefDir(t, brief)
	if err := os.Rename(brief, filepath.Join(base, "moved")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, brief); err != nil {
		t.Fatal(err)
	}
	r := ScanReports(fsys)
	if len(r.Files) != 1 || r.Files[0].Name != "report.md" || r.Files[0].Complete || !r.Files[0].Modified.Equal(when) {
		t.Errorf("the scan read the swapped-in directory: %+v", r)
	}
}
