//go:build unix

package sweep

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readFile(t *testing.T, p string) []byte {
	t.Helper()
	data, err := os.ReadFile(p) //nolint:gosec // G304: a test path under t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func readMode(t *testing.T, p string) os.FileMode {
	t.Helper()
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode()
}

func TestWriteCreatesThePrivateFile(t *testing.T) {
	base := filepath.Join(t.TempDir(), "state")
	if err := Write(base, []byte("{\"a\":1}\n")); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(base, "cadence", "sweep.json")
	if m := readMode(t, p); m != 0o600 {
		t.Errorf("file mode = %v, want 0600", m)
	}
	if m := readMode(t, filepath.Dir(p)); m.Perm() != 0o700 {
		t.Errorf("created dir mode = %v, want 0700", m.Perm())
	}
	if data := readFile(t, p); string(data) != "{\"a\":1}\n" {
		t.Errorf("content = %q", data)
	}
	entries, _ := os.ReadDir(filepath.Dir(p))
	if len(entries) != 1 {
		t.Errorf("a temporary file was left behind: %v", entries)
	}
}

func TestWriteKeepsASharedParentAsItFound(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "cadence")
	if err := os.Mkdir(dir, 0o755); err != nil { //nolint:gosec // a shared state dir, as the spool leaves it
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil { //nolint:gosec // see above
		t.Fatal(err)
	}
	if err := Write(base, []byte("x")); err != nil {
		t.Fatal(err)
	}
	if m := readMode(t, dir); m.Perm() != 0o755 {
		t.Errorf("the shared parent was narrowed to %v", m.Perm())
	}
}

func TestWriteReplacesAtomicallyAndNeverFollowsALink(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "cadence")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(victim, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "sweep.json")
	if err := os.Symlink(victim, p); err != nil {
		t.Fatal(err)
	}
	if err := Write(base, []byte("new")); err != nil {
		t.Fatal(err)
	}
	if data := readFile(t, victim); string(data) != "keep" {
		t.Errorf("Write followed the symlink and changed its target: %q", data)
	}
	if m := readMode(t, p); !m.IsRegular() {
		t.Errorf("sweep.json is still %v", m)
	}
	if data := readFile(t, p); string(data) != "new" {
		t.Errorf("content = %q", data)
	}
}

func TestWriteRefusesAnUnsafeParent(t *testing.T) {
	t.Run("symlinked parent", func(t *testing.T) {
		base := t.TempDir()
		target := t.TempDir()
		if err := os.Symlink(target, filepath.Join(base, "cadence")); err != nil {
			t.Fatal(err)
		}
		err := Write(base, []byte("x"))
		if err == nil {
			t.Fatal("wrote through a symlinked parent")
		}
		if entries, _ := os.ReadDir(target); len(entries) != 0 {
			t.Errorf("something was written to the link's target: %v", entries)
		}
	})
	t.Run("group-writable parent", func(t *testing.T) {
		base := t.TempDir()
		dir := filepath.Join(base, "cadence")
		if err := os.Mkdir(dir, 0o755); err != nil { //nolint:gosec // the unsafe case under test
			t.Fatal(err)
		}
		if err := os.Chmod(dir, 0o775); err != nil { //nolint:gosec // the unsafe case under test
			t.Fatal(err)
		}
		if err := Write(base, []byte("x")); err == nil || !strings.Contains(err.Error(), "writable") {
			t.Errorf("err = %v, want a refusal naming the mode", err)
		}
	})
	t.Run("relative base", func(t *testing.T) {
		if err := Write("relative/state", []byte("x")); err == nil {
			t.Error("a relative state directory was accepted")
		}
	})
}

func TestWriteRefusesADirectoryAtTheTarget(t *testing.T) {
	base := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, "cadence", "sweep.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := Write(base, []byte("x")); err == nil {
		t.Error("replaced a directory")
	}
	entries, _ := os.ReadDir(filepath.Join(base, "cadence"))
	if len(entries) != 1 {
		t.Errorf("a temporary file was left behind: %v", entries)
	}
}
