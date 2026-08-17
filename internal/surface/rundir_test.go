package surface_test

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/surface"
)

// shortBase returns a base short enough to bind a socket under. It is
// deliberately not t.TempDir(), which embeds the test name and would blow the
// sun_path budget this package exists to respect — the exact trap the code
// under test refuses.
func shortBase(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "fcb")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// TestNewRunDir_IsPrivateAndBindable is the whole contract in one test: the
// directory is 0700 and ours, and a socket actually binds at the path it
// reports. Asserting the mode without binding would leave the length budget —
// the thing most likely to be wrong on a real machine — unproven.
func TestNewRunDir_IsPrivateAndBindable(t *testing.T) {
	dir, err := surface.NewRunDir(shortBase(t))
	if err != nil {
		t.Fatalf("NewRunDir: %v", err)
	}
	t.Cleanup(func() { _ = dir.Close() })

	info, err := os.Lstat(dir.Path())
	if err != nil {
		t.Fatalf("lstat: %v", err)
	}
	if !info.IsDir() {
		t.Fatal("run dir is not a directory")
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Errorf("run dir mode = %o, want 700", got)
	}

	socket := dir.SocketPath()
	if len(socket) > surface.MaxSocketPathLen {
		t.Errorf("socket path is %d bytes, over the %d-byte budget: %s",
			len(socket), surface.MaxSocketPathLen, socket)
	}

	var lc net.ListenConfig
	listener, err := lc.Listen(t.Context(), "unix", socket)
	if err != nil {
		t.Fatalf("the reported socket path does not bind: %v", err)
	}
	_ = listener.Close()
}

// TestNewRunDir_NamesAreUnguessableAndDistinct keeps two concurrent launches
// off the same directory, and keeps the name from being derivable.
func TestNewRunDir_NamesAreUnguessableAndDistinct(t *testing.T) {
	base := shortBase(t)

	seen := make(map[string]bool, 32)
	for range 32 {
		dir, err := surface.NewRunDir(base)
		if err != nil {
			t.Fatalf("NewRunDir: %v", err)
		}
		name := filepath.Base(dir.Path())
		if seen[name] {
			t.Fatalf("NewRunDir repeated the name %q; it is not drawing entropy", name)
		}
		seen[name] = true
		if !strings.HasPrefix(name, "fc-") {
			t.Errorf("name %q is outside forgectl's namespace", name)
		}
		_ = dir.Close()
	}
}

// TestNewRunDir_RefusesAnOverlongPathBeforeCreatingAnything is the check that
// earns its place on a real machine: a long TMPDIR is common, and the failure
// it prevents is a bind error saying "invalid argument", which explains
// nothing. The refusal must also leave no directory behind.
func TestNewRunDir_RefusesAnOverlongPathBeforeCreatingAnything(t *testing.T) {
	root := shortBase(t)

	// Build a base long enough that base + "/fc-xxxxxxxx/s" exceeds the bound.
	deep := filepath.Join(root, strings.Repeat("d", surface.MaxSocketPathLen))
	if err := os.MkdirAll(deep, 0o700); err != nil {
		t.Fatal(err)
	}

	before, err := os.ReadDir(deep)
	if err != nil {
		t.Fatal(err)
	}

	dir, err := surface.NewRunDir(deep)
	if !errors.Is(err, surface.ErrSocketPathTooLong) {
		t.Fatalf("NewRunDir on a long base = %v, want ErrSocketPathTooLong", err)
	}
	if dir != nil {
		t.Error("a refused NewRunDir still returned a directory")
	}
	if !strings.Contains(err.Error(), "TMPDIR") {
		t.Errorf("the refusal does not tell the operator how to fix it: %v", err)
	}

	after, err := os.ReadDir(deep)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Errorf("a refused NewRunDir created %d entries", len(after)-len(before))
	}
}

// TestNewRunDir_RefusesARelativeBase keeps the run directory from landing
// wherever the process happened to be started.
func TestNewRunDir_RefusesARelativeBase(t *testing.T) {
	if _, err := surface.NewRunDir("tmp/relative"); !errors.Is(err, surface.ErrRunDir) {
		t.Errorf("NewRunDir with a relative base = %v, want ErrRunDir", err)
	}
}

// TestRunDir_CloseRemovesEverythingAndRepeats covers the cleanup contract.
// Close runs on failure paths where proving it ran exactly once is harder than
// making a second call harmless, so a second call must not error.
func TestRunDir_CloseRemovesEverythingAndRepeats(t *testing.T) {
	dir, err := surface.NewRunDir(shortBase(t))
	if err != nil {
		t.Fatalf("NewRunDir: %v", err)
	}
	path := dir.Path()

	// Something in the directory, so removal has work to do. A plain file
	// rather than a socket: Go's UnixListener unlinks its own path on Close,
	// so a socket would be gone before Close is even called and the removal
	// would have nothing to prove.
	resident := filepath.Join(path, "resident")
	if err := os.WriteFile(resident, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := dir.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Errorf("Close left the directory behind: %v", err)
	}

	if err := dir.Close(); err != nil {
		t.Errorf("second Close = %v, want nil", err)
	}

	var nilDir *surface.RunDir
	if err := nilDir.Close(); err != nil {
		t.Errorf("Close on a nil RunDir = %v, want nil", err)
	}
}

// TestNewRunDir_DefaultsToTheSystemTemp covers the production call shape,
// which passes no base at all.
func TestNewRunDir_DefaultsToTheSystemTemp(t *testing.T) {
	dir, err := surface.NewRunDir("")
	if err != nil {
		// A machine whose TMPDIR is genuinely too long is a real condition,
		// not a test failure — but it must be reported as the length refusal
		// rather than something vaguer.
		if errors.Is(err, surface.ErrSocketPathTooLong) {
			t.Skipf("this machine's temp dir is too long for a socket: %v", err)
		}
		t.Fatalf("NewRunDir(\"\"): %v", err)
	}
	t.Cleanup(func() { _ = dir.Close() })

	if !strings.HasPrefix(dir.Path(), filepath.Clean(os.TempDir())) {
		t.Errorf("run dir %q is not under the system temp dir %q", dir.Path(), os.TempDir())
	}
}
