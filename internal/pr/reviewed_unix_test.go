//go:build unix

package pr

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// Test plan for LoadReviewed's read (forgectl#765)
//
//   [x] A FIFO at the store path loads as an empty store, fast, instead of
//       blocking the open
//   [x] A store symlinked in (a dotfile manager's layout) still loads
//   [x] A store over the 8 KiB record bound still loads in full

// writeReviewedStore writes a store holding n marks and returns the path.
func writeReviewedStore(t *testing.T, dir string, n int) string {
	t.Helper()
	at := make(map[string]time.Time, n)
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < n; i++ {
		at[fmt.Sprintf("github.com/owner/repo#%d", i+1)] = base.Add(time.Duration(i) * time.Minute)
	}
	data, err := json.Marshal(at)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "pr-reviewed.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// A writer holds the FIFO open without writing, so the pipe never reaches EOF:
// without that, a reader that got past the open would read nothing and the
// test could not tell a skipped FIFO from an empty one.
//
// Mutations that turn it red: in LoadReviewed, read with os.ReadFile(path)
// (the pre-#765 form), or drop readReviewedFile's IsRegular check. Either way
// the read blocks on the idle writer, and mustFailFast fails the test.
func TestLoadReviewed_AFIFOLoadsEmptyAndFast(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pr-reviewed.json")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	// O_RDWR opens a FIFO without waiting for a reader (Linux and macOS).
	writer, err := os.OpenFile(filepath.Clean(path), os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("hold the FIFO open: %v", err)
	}
	t.Cleanup(func() { _ = writer.Close() })
	var s *ReviewedStore
	_ = mustFailFast(t, "LoadReviewed on a FIFO", func() error {
		s = LoadReviewed(path)
		return nil
	})
	if len(s.at) != 0 {
		t.Errorf("a FIFO store loaded %d marks, want an empty store", len(s.at))
	}
	if _, err := os.Lstat(path); err != nil {
		t.Errorf("the FIFO was disturbed: %v", err)
	}
}

// Mutation that turns it red: open with openNoFollowNonblock (readRecordFile's
// opener) instead of openNonblock. The symlink is refused with ELOOP, and the
// store loads empty.
func TestLoadReviewed_ASymlinkedStoreStillLoads(t *testing.T) {
	target := writeReviewedStore(t, t.TempDir(), 3)
	link := filepath.Join(t.TempDir(), "pr-reviewed.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if got := len(LoadReviewed(link).at); got != 3 {
		t.Errorf("a symlinked store loaded %d marks, want 3", got)
	}
}

// Mutation that turns it red: read through readBreadcrumbBytes (or
// readRecordFile) instead of io.ReadAll. The 8 KiB record cap refuses the
// store, and it loads empty.
func TestLoadReviewed_AStoreOverTheRecordBoundLoadsInFull(t *testing.T) {
	const n = 400
	path := writeReviewedStore(t, t.TempDir(), n)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() <= maxBreadcrumbRecordBytes {
		t.Fatalf("fixture is %d bytes; it must exceed the %d-byte record bound to test anything",
			info.Size(), maxBreadcrumbRecordBytes)
	}
	if got := len(LoadReviewed(path).at); got != n {
		t.Errorf("a %d-byte store loaded %d marks, want %d", info.Size(), got, n)
	}
}
