package pr

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateWorkspace_RejectsEvalSymlinksFailure(t *testing.T) {
	if _, err := os.Stat("/proc/self/fd"); err != nil {
		t.Skipf("procfs file-descriptor links unavailable: %v", err)
	}

	// Keeping the directory descriptor open after unlinking makes the procfs
	// magic link stattable while component-by-component resolution follows the
	// textual " (deleted)" target and fails.
	root := t.TempDir()
	heldDir := filepath.Join(root, "held-dir")
	if err := os.Mkdir(heldDir, 0o700); err != nil {
		t.Fatalf("mkdir held directory: %v", err)
	}
	held, err := os.Open(heldDir)
	if err != nil {
		t.Fatalf("open held directory: %v", err)
	}
	t.Cleanup(func() {
		if err := held.Close(); err != nil {
			t.Errorf("close held directory: %v", err)
		}
	})
	if err := os.Remove(heldDir); err != nil {
		t.Fatalf("remove held directory: %v", err)
	}

	workspace := filepath.Join(root, sandboxPrefix+"magic")
	if err := os.Symlink(fmt.Sprintf("/proc/self/fd/%d", held.Fd()), workspace); err != nil {
		t.Fatalf("symlink workspace to held directory: %v", err)
	}

	info, err := os.Stat(workspace)
	if err != nil {
		t.Fatalf("stat magic-link workspace: %v", err)
	}
	if !info.IsDir() {
		t.Fatal("magic-link workspace must stat as a directory")
	}
	if !strings.HasPrefix(filepath.Base(workspace), sandboxPrefix) {
		t.Fatalf("literal workspace base %q must carry %q", filepath.Base(workspace), sandboxPrefix)
	}
	if _, evalErr := filepath.EvalSymlinks(workspace); evalErr == nil {
		t.Fatal("EvalSymlinks of magic-link workspace must fail")
	} else if !errors.Is(evalErr, fs.ErrNotExist) {
		t.Fatalf("EvalSymlinks error = %v, want fs.ErrNotExist", evalErr)
	}
	if _, err := os.Lstat(workspace); err != nil {
		t.Fatalf("lstat magic-link workspace before validation: %v", err)
	}

	err = validateWorkspace(workspace)
	if err == nil {
		t.Fatal("validateWorkspace must reject an unresolvable workspace")
	}
	wantPrefix := fmt.Sprintf("workspace %q could not be resolved: ", workspace)
	if !strings.HasPrefix(err.Error(), wantPrefix) {
		t.Fatalf("validateWorkspace error = %q, want prefix %q", err, wantPrefix)
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("validateWorkspace error = %v, want wrapped fs.ErrNotExist", err)
	}
	if _, err := os.Lstat(workspace); err != nil {
		t.Fatalf("validation must not mutate magic-link workspace: %v", err)
	}
}
