package env

// Test plan for write.go
//
// writeAtomic (Classification: filesystem effect, real temp-dir fixtures)
//   [x] Happy: a new file is created at exactly 0600, content matches, no
//       leftover temp file in the directory, tightened=false
//   [x] Happy: a pre-existing looser-permission (0644) file lands at 0600
//       and reports tightened=true, with the new content in place
//   [x] An already-0600 file is not reported as tightened
//   [x] A write into a read-only directory fails with a path-only error —
//       the offending VALUE never appears in it

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteAtomic_CreatesFileAt0600(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, ".env")
	data := []byte("KEY=1\n")

	tightened, err := writeAtomic(target, data)
	if err != nil {
		t.Fatalf("writeAtomic: %v", err)
	}
	if tightened {
		t.Error("tightened = true, want false for a brand-new file")
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != string(data) {
		t.Errorf("content = %q, want %q", got, data)
	}

	fi, err := os.Stat(target)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if fi.Mode().Perm() != secureMode {
		t.Errorf("mode = %o, want %o", fi.Mode().Perm(), secureMode)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("dir has %d entries after write, want 1 (just the target): %v", len(entries), names)
	}
}

func TestWriteAtomic_Tightens0644To0600AndReports(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, ".env")
	if err := os.WriteFile(target, []byte("KEY=old\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	tightened, err := writeAtomic(target, []byte("KEY=new\n"))
	if err != nil {
		t.Fatalf("writeAtomic: %v", err)
	}
	if !tightened {
		t.Error("tightened = false, want true for a pre-existing 0644 file")
	}

	fi, err := os.Stat(target)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if fi.Mode().Perm() != secureMode {
		t.Errorf("mode = %o, want %o", fi.Mode().Perm(), secureMode)
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "KEY=new\n" {
		t.Errorf("content = %q, want %q", got, "KEY=new\n")
	}
}

func TestWriteAtomic_AlreadySecure_NotTightened(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, ".env")
	if err := os.WriteFile(target, []byte("KEY=old\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	tightened, err := writeAtomic(target, []byte("KEY=new\n"))
	if err != nil {
		t.Fatalf("writeAtomic: %v", err)
	}
	if tightened {
		t.Error("tightened = true, want false for an already-0600 file")
	}
}

func TestWriteAtomic_ReadOnlyDirErrorsPathOnly(t *testing.T) {
	dir := t.TempDir()
	roDir := filepath.Join(dir, "locked")
	if err := os.Mkdir(roDir, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if err := os.Chmod(roDir, 0o500); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	// t.TempDir()'s own cleanup needs write permission on every directory
	// it removes.
	t.Cleanup(func() { _ = os.Chmod(roDir, 0o755) })

	const sentinel = "s3ntinel-VALUE-77x"
	target := filepath.Join(roDir, ".env")

	_, err := writeAtomic(target, []byte("KEY="+sentinel+"\n"))
	if err == nil {
		t.Skip("writeAtomic into a read-only dir succeeded — likely running as root; skipping (permission checks don't apply)")
	}

	assertNoSecretInOutput(t, sentinel, "", err.Error())
	if !strings.Contains(err.Error(), roDir) {
		t.Errorf("error %q does not name the directory %q", err.Error(), roDir)
	}
}
