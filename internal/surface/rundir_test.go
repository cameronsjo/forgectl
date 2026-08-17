package surface_test

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

// TestNewRunDir_RefusesAWorldWritableNonStickyBase covers the check the whole
// directory guarantee silently rested on.
//
// privdir proves the *leaf* is ours but hands back a path — there is no
// bindat — so the window between that proof and the bind is closed only by the
// base being one another user cannot rename entries in. On stock Linux /tmp is
// 1777 and the sticky bit carries that; strip it, as a `chmod 777 /tmp` in a
// container image does, and an attacker can rename our proven directory away
// and drop a symlink at the same name before the socket lands.
func TestNewRunDir_RefusesAWorldWritableNonStickyBase(t *testing.T) {
	root := shortBase(t)

	// World-writable WITHOUT the sticky bit: the dangerous combination.
	unsticky := filepath.Join(root, "u")
	//nolint:gosec // G301: the permissive mode is the condition under test
	if err := os.Mkdir(unsticky, 0o777); err != nil {
		t.Fatal(err)
	}
	//nolint:gosec // G302: the permissive mode is the condition under test
	if err := os.Chmod(unsticky, 0o777); err != nil {
		t.Fatal(err)
	}

	if _, err := surface.NewRunDir(unsticky); !errors.Is(err, surface.ErrRunDir) {
		t.Fatalf("NewRunDir on a world-writable non-sticky base = %v, want ErrRunDir", err)
	}
	if entries, err := os.ReadDir(unsticky); err != nil || len(entries) != 0 {
		t.Errorf("the refusal created something: %d entries, %v", len(entries), err)
	}

	// The control: the same mode WITH the sticky bit is accepted, which is
	// stock /tmp. Otherwise this test would pass for a policy that refuses
	// every shared base, and Linux would be unusable.
	sticky := filepath.Join(root, "s")
	//nolint:gosec // G301: 1777 is stock /tmp, the case that must keep working
	if err := os.Mkdir(sticky, 0o777); err != nil {
		t.Fatal(err)
	}
	//nolint:gosec // G302: 1777 is stock /tmp, the case that must keep working
	if err := os.Chmod(sticky, 0o777|os.ModeSticky); err != nil {
		t.Fatal(err)
	}
	dir, err := surface.NewRunDir(sticky)
	if err != nil {
		t.Fatalf("NewRunDir on a sticky world-writable base = %v, want success", err)
	}
	_ = dir.Close()

	// And a private base is fine, which is macOS's per-user temp dir.
	private := filepath.Join(root, "p")
	if err := os.Mkdir(private, 0o700); err != nil {
		t.Fatal(err)
	}
	dir, err = surface.NewRunDir(private)
	if err != nil {
		t.Fatalf("NewRunDir on a 0700 base = %v, want success", err)
	}
	_ = dir.Close()
}

// TestRunDir_ClosesConcurrentlyWithoutDoubleClosing covers the shape the doc
// comment invites: a defer racing an error-path cleanup. Without the guard both
// would read a live descriptor before either cleared it, and the second close
// would land on whatever number the runtime had since reused.
func TestRunDir_ClosesConcurrentlyWithoutDoubleClosing(t *testing.T) {
	dir, err := surface.NewRunDir(shortBase(t))
	if err != nil {
		t.Fatalf("NewRunDir: %v", err)
	}
	path := dir.Path()

	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = dir.Close()
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("concurrent Close %d = %v, want nil", i, err)
		}
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Errorf("the directory survived: %v", err)
	}
}

// TestRunDir_ZeroValueDoesNotCloseStdin pins the sentinel. RunDir is exported,
// so `var d surface.RunDir; defer d.Close()` is a shape a caller can write, and
// a `fd >= 0` test would read the zero value's fd 0 as live.
func TestRunDir_ZeroValueDoesNotCloseStdin(t *testing.T) {
	var d surface.RunDir
	if err := d.Close(); err != nil {
		t.Fatalf("Close on the zero value = %v, want nil", err)
	}

	// stdin must still be open: reading its fd stats successfully.
	if _, err := os.Stdin.Stat(); err != nil {
		t.Errorf("stdin was closed by the zero value's Close: %v", err)
	}

	if got := d.SocketPath(); got != "" {
		t.Errorf("zero-value SocketPath() = %q, want empty", got)
	}
}

// TestRunDir_SocketPathIsEmptyAfterClose keeps a use-after-close from yielding
// a *relative* path, which would be refused far away by the bootstrap
// classifier's absoluteness check with a message about neither.
func TestRunDir_SocketPathIsEmptyAfterClose(t *testing.T) {
	dir, err := surface.NewRunDir(shortBase(t))
	if err != nil {
		t.Fatalf("NewRunDir: %v", err)
	}
	if dir.SocketPath() == "" {
		t.Fatal("SocketPath is empty before Close")
	}

	_ = dir.Close()

	if got := dir.SocketPath(); got != "" {
		t.Errorf("SocketPath() after Close = %q, want empty", got)
	}
	if got := dir.Path(); got != "" {
		t.Errorf("Path() after Close = %q, want empty", got)
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
