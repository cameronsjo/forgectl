package clean

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// failReadDir swaps Scan's walker for filepath.WalkDir with dir's ReadDir
// failing on permission, the way a mode-000 directory fails for a non-root
// user. It hands the error to the callback exactly as WalkDir does: one call
// with the directory's entry and a nil error, then a second call with the
// same entry and the ReadDir error, and nothing below dir is visited. Tests
// use it so the unreadable path runs as root too, which reads mode-000
// directories.
func failReadDir(t *testing.T, dir string) {
	t.Helper()
	prev := walkDir
	t.Cleanup(func() { walkDir = prev })
	walkDir = func(root string, fn fs.WalkDirFunc) error {
		return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || path != dir || !d.IsDir() {
				return fn(path, d, err)
			}
			if r := fn(path, d, nil); r != nil {
				return r
			}
			if r := fn(path, d, &fs.PathError{Op: "open", Path: path, Err: fs.ErrPermission}); r != nil {
				return r
			}
			return fs.SkipDir
		})
	}
}

// TestScan_UnscannableRootFails pins forgectl#915: a root Scan cannot walk
// is an error, never an empty report that reads as "nothing to reclaim".
//
// Mutation that turns it red: drop the `path == opts.Root` return in Scan's
// error branch (missing, unreadable), or the root's !d.IsDir() refusal
// (regular file, dangling symlink).
func TestScan_UnscannableRootFails(t *testing.T) {
	for _, tt := range []struct {
		name    string
		root    func(t *testing.T, dir string) string
		wantErr string
	}{
		{"missing", func(_ *testing.T, dir string) string {
			return filepath.Join(dir, "absent")
		}, "no such file or directory"},
		{"unreadable", func(t *testing.T, dir string) string {
			mustWriteFile(t, filepath.Join(dir, "proj", "node_modules", "a.js"), 10)
			failReadDir(t, dir)
			return dir
		}, "permission denied"},
		{"regular file", func(t *testing.T, dir string) string {
			path := filepath.Join(dir, "file")
			mustWriteFile(t, path, 10)
			return path
		}, "not a directory"},
		{"dangling symlink", func(t *testing.T, dir string) string {
			path := filepath.Join(dir, "link")
			if err := os.Symlink(filepath.Join(dir, "gone"), path); err != nil {
				t.Fatal(err)
			}
			return path
		}, "not a directory"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := tt.root(t, t.TempDir())
			report, err := Scan(ScanOptions{Root: root})
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Scan(%s) error = %v, want one containing %q", root, err, tt.wantErr)
			}
			if len(report.Targets) != 0 || report.TotalSize != 0 {
				t.Errorf("a failed scan returned a report: %+v", report)
			}
		})
	}
}

// TestScan_UnreadableRootFails_RealPermissions is the unreadable case with
// no seam, for a non-root user.
//
// Mutation that turns it red: drop the `path == opts.Root` return in Scan's
// error branch.
func TestScan_UnreadableRootFails_RealPermissions(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode-000 directory; TestScan_UnscannableRootFails covers this through failReadDir")
	}
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "proj", "node_modules", "a.js"), 10)
	if err := os.Chmod(root, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o700) }) //nolint:gosec // G302: a directory needs 0700; 0600 makes it non-traversable
	if _, err := Scan(ScanOptions{Root: root}); !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("Scan error = %v, want permission denied", err)
	}
}

// TestScan_UnreadableSubdirStaysBestEffort pins the other half of the
// split: only the root fails the scan. An unreadable directory below it is
// skipped and every other target is still reported.
//
// Mutation that turns it red: return err for every walk error, not only the
// root's.
func TestScan_UnreadableSubdirStaysBestEffort(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "locked", "node_modules", "a.js"), 10)
	mustWriteFile(t, filepath.Join(root, "open", "node_modules", "b.js"), 10)
	failReadDir(t, filepath.Join(root, "locked"))
	report, err := Scan(ScanOptions{Root: root})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(report.Targets) != 1 || report.Targets[0].Path != filepath.Join(root, "open", "node_modules") {
		t.Errorf("targets = %+v, want only open/node_modules", report.Targets)
	}
}

// TestPreview_UnscannableRootFails carries the scan error through the
// entry point `forgectl status` calls, naming the root.
//
// Mutation that turns it red: drop the `path == opts.Root` return in Scan's
// error branch.
func TestPreview_UnscannableRootFails(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent")
	_, result, err := New(fakeGitRunner(nil), WithRoot(missing)).Preview(context.Background())
	if err == nil || !strings.Contains(err.Error(), "absent") {
		t.Fatalf("Preview error = %v, want one naming the missing root", err)
	}
	if len(result.Items) != 0 {
		t.Errorf("a failed preview returned items: %+v", result.Items)
	}
}
