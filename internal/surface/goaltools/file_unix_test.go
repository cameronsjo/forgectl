//go:build unix

package goaltools

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunFileWritesAndReadsThroughPinnedParents(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "src"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := RunFile(root, "src/new.txt", true, "approved"); err != nil {
		t.Fatalf("RunFile write: %v", err)
	}
	content, err := RunFile(root, "src/new.txt", false, "")
	if err != nil || content != "approved" {
		t.Fatalf("RunFile read = %q, %v", content, err)
	}
	info, err := os.Stat(filepath.Join(root, "src", "new.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("new file mode = %o, want 600", info.Mode().Perm())
	}
}

func TestRunFileRefusesSymlinkAndHardlinkParentsAndFinals(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "outside.txt"), []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link-parent")); err != nil {
		t.Fatal(err)
	}
	if _, err := RunFile(root, "link-parent/outside.txt", false, ""); err == nil {
		t.Fatal("symlink parent read succeeded")
	}
	if _, err := RunFile(root, "link-parent/new.txt", true, "overwrite"); err == nil {
		t.Fatal("symlink parent write succeeded")
	}

	if err := os.Symlink(filepath.Join(outside, "outside.txt"), filepath.Join(root, "final-link")); err != nil {
		t.Fatal(err)
	}
	if _, err := RunFile(root, "final-link", true, "overwrite"); err == nil {
		t.Fatal("symlink final write succeeded")
	}
	if err := os.Link(filepath.Join(outside, "outside.txt"), filepath.Join(root, "final-hardlink")); err != nil {
		t.Fatal(err)
	}
	if _, err := RunFile(root, "final-hardlink", true, "overwrite"); err == nil {
		t.Fatal("hardlink final write succeeded")
	}
	content, err := os.ReadFile(filepath.Join(outside, "outside.txt")) //nolint:gosec // Test-owned negative-control target.
	if err != nil || string(content) != "outside" {
		t.Fatalf("outside target changed: %q, %v", content, err)
	}
}

func TestRunFilePreservesExistingExecutableMode(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "check.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil { //nolint:gosec // Executable source fixture verifies mode preservation.
		t.Fatal(err)
	}
	if _, err := RunFile(root, "check.sh", true, "#!/bin/sh\nexit 0\n"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("existing executable mode changed: %v, %v", info, err)
	}
	content, err := RunFile(root, "check.sh", false, "")
	if err != nil || content != "#!/bin/sh\nexit 0\n" {
		t.Fatalf("replacement source = %q, %v", content, err)
	}
}

func TestRunFileRejectsTraversalMissingParentsAndOversizedReads(t *testing.T) {
	root := t.TempDir()
	for _, requested := range []string{"../outside", "src/../outside", "/tmp/outside", ".", "src//file", "src/./file"} {
		t.Run(strings.ReplaceAll(requested, "/", "_"), func(t *testing.T) {
			if _, err := RunFile(root, requested, false, ""); err == nil {
				t.Fatalf("RunFile(%q) accepted unsafe path", requested)
			}
		})
	}
	if _, err := RunFile(root, "missing/file", true, "content"); err == nil {
		t.Fatal("write created a missing parent")
	}
	large := bytes.Repeat([]byte("x"), maxGoalFileBytes+1)
	if err := os.WriteFile(filepath.Join(root, "large"), large, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := RunFile(root, "large", false, ""); err == nil {
		t.Fatal("oversized read succeeded")
	}
}
