package resume

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/termsafe"
	"github.com/cameronsjo/forgectl/internal/termsafe/termsafetest"
)

// These tests pin #871 item 2: the errors this package builds around a
// filesystem cause are terminal-safe where they are created, not only where a
// CLI sink renders them, and the render-time termsafe.Error the CLI applies on
// top of them (#867, #870) changes nothing. Each also checks that errors.Is
// and errors.As still reach the filesystem cause through the wrap.

// assertSourceSafe is the shared contract: err is inert text, a second
// termsafe.Error (as a sink applies it) and a %v wrap then termsafe.Error (as
// the resume CLI's stderr lines do) both leave its text unchanged, and the
// cause is still reachable.
func assertSourceSafe(t *testing.T, err error, cause error) {
	t.Helper()
	if err == nil {
		t.Fatal("want an error, got nil")
	}
	text := err.Error()
	termsafetest.AssertInert(t, "source error", text)
	if again := termsafe.Error(err).Error(); again != text {
		t.Errorf("a render-time termsafe.Error changed the text (double escape):\n first: %s\nsecond: %s", text, again)
	}
	wrapped := fmt.Errorf("could not restore tasks: %v", err)
	if again := termsafe.Error(wrapped).Error(); again != "could not restore tasks: "+text {
		t.Errorf("a %%v wrap then termsafe.Error changed the text:\n got: %s\nwant: could not restore tasks: %s", again, text)
	}
	if cause != nil && !errors.Is(err, cause) {
		t.Errorf("errors.Is(err, %v) = false; the wrap lost the cause: %v", cause, err)
	}
	var pathErr *fs.PathError
	var linkErr *os.LinkError
	if cause != nil && !errors.As(err, &pathErr) && !errors.As(err, &linkErr) {
		t.Errorf("errors.As(err, *fs.PathError or *os.LinkError) = false; the wrap lost the cause: %v", err)
	}
}

// procTaskDir returns a task directory whose path carries hostile runes and
// whose file creation fails even for root: a symlink named with them that
// points at /proc/self, where no file can be created. Restore's MkdirAll
// follows the link to an existing directory and succeeds, so the failure is
// the O_EXCL open inside the loop, the site #871 names.
func procTaskDir(t *testing.T, parent string) string {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("needs /proc/self as a directory no one can create files in")
	}
	link := filepath.Join(parent, termsafetest.Hostile("tasks"))
	if err := os.Symlink("/proc/self", link); err != nil {
		t.Skipf("cannot create the fixture symlink: %v", err)
	}
	return link
}

func TestRestore_OpenErrorIsTerminalSafeAtTheSource(t *testing.T) {
	dir := procTaskDir(t, t.TempDir())
	_, err := Restore(dir, []Task{{ID: "1", Raw: []byte(`{"id":"1"}`)}})
	if err == nil || !strings.HasPrefix(err.Error(), "restore task 1: ") {
		t.Fatalf("want the per-task open failure, got %v", err)
	}
	assertSourceSafe(t, err, fs.ErrNotExist)
}

// An over-cap path is cut by the first termsafe.Error. The render-time pass
// must neither re-cut it nor find the native text to replace a second time.
func TestRestore_OpenErrorWithOverCapPathRendersOnce(t *testing.T) {
	parent := t.TempDir()
	for range 3 {
		parent = filepath.Join(parent, strings.Repeat("d", 200))
	}
	if err := os.MkdirAll(parent, 0o700); err != nil {
		t.Skipf("cannot build a long fixture path: %v", err)
	}
	dir := procTaskDir(t, parent)
	if len(dir) <= termsafe.PathEchoMaxRunes {
		t.Fatalf("fixture path is %d runes, want over %d", len(dir), termsafe.PathEchoMaxRunes)
	}
	_, err := Restore(dir, []Task{{ID: "1", Raw: []byte(`{"id":"1"}`)}})
	if err == nil || !strings.HasPrefix(err.Error(), "restore task 1: ") {
		t.Fatalf("want the per-task open failure, got %v", err)
	}
	if strings.Count(err.Error(), strings.Repeat("d", 200)) >= 3 {
		t.Errorf("the over-cap path was echoed whole: %v", err)
	}
	assertSourceSafe(t, err, fs.ErrNotExist)
}

func TestDelete_RemoveErrorIsTerminalSafeAtTheSource(t *testing.T) {
	// A store directory named with hostile runes, and a record path that is a
	// non-empty directory, which os.Remove refuses even for root.
	dir := filepath.Join(t.TempDir(), termsafetest.Hostile("store"))
	id := "abc123"
	if err := os.MkdirAll(filepath.Join(dir, id+".json", "keep"), 0o700); err != nil {
		// Windows refuses control characters in a file name.
		t.Skipf("cannot create the hostile fixture directory: %v", err)
	}
	err := Delete(dir, id)
	if err == nil || !strings.HasPrefix(err.Error(), "delete record abc123: ") {
		t.Fatalf("want the remove failure, got %v", err)
	}
	var pathErr *fs.PathError
	if !errors.As(err, &pathErr) {
		t.Fatalf("errors.As(err, *fs.PathError) = false: %v", err)
	}
	assertSourceSafe(t, err, pathErr.Err)
}

// DefaultPaths' causes are stdlib errors with fixed text today, so there is no
// hostile byte to observe. What the source wrap guarantees is that the error
// is termsafe's wrapper around the cause rather than the raw cause, so a
// future cause that does carry a value is escaped before any sink sees it.
func TestDefaultPaths_ErrorIsWrappedAtTheSource(t *testing.T) {
	switch runtime.GOOS {
	case "windows", "plan9":
		t.Skip("os.UserHomeDir reads a different variable here")
	}
	for _, tc := range []struct {
		name, home, xdg string
	}{
		{name: "home dir", home: "", xdg: ""},
		// $HOME resolves; the config dir does not, which fails
		// config.ResumeStoreDir, the second site.
		{name: "store dir", home: t.TempDir(), xdg: "relative/config"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.xdg != "" && runtime.GOOS == "darwin" {
				t.Skip("os.UserConfigDir ignores $XDG_CONFIG_HOME here")
			}
			t.Setenv("HOME", tc.home)
			t.Setenv("XDG_CONFIG_HOME", tc.xdg)
			_, err := DefaultPaths()
			if err == nil {
				t.Fatal("want an error")
			}
			if errors.Unwrap(err) == nil {
				t.Errorf("DefaultPaths returned the raw cause, not a termsafe.Error wrap: %T %v", err, err)
			}
			assertSourceSafe(t, err, nil)
		})
	}
}

// hostileDir creates a directory under a fresh temp dir whose name carries
// hostile runes, skipping where the OS refuses them (Windows).
func hostileDir(t *testing.T, label string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), termsafetest.Hostile(label))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Skipf("cannot create the hostile fixture directory: %v", err)
	}
	return dir
}

// Save's write, close, and chmod cannot be made to fail against a real file,
// so each is failed through its seam with the *PathError the real operation
// would return: the op and the (hostile) temp-file path.
func TestSave_TempFileErrorsAreTerminalSafeAtTheSource(t *testing.T) {
	injected := func(op, path string) error {
		return &fs.PathError{Op: op, Path: path, Err: fs.ErrInvalid}
	}
	for _, tc := range []struct {
		name  string
		setup func()
	}{
		{name: "write", setup: func() {
			saveWrite = func(f *os.File, _ []byte) (int, error) { return 0, injected("write", f.Name()) }
		}},
		{name: "close", setup: func() {
			saveClose = func(f *os.File) error {
				_ = f.Close()
				return injected("close", f.Name())
			}
		}},
		{name: "chmod", setup: func() {
			saveChmod = func(name string, _ os.FileMode) error { return injected("chmod", name) }
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, c, m := saveWrite, saveClose, saveChmod
			t.Cleanup(func() { saveWrite, saveClose, saveChmod = w, c, m })
			tc.setup()
			err := Save(hostileDir(t, "store"), &Record{ID: "abc123"})
			assertSourceSafe(t, err, fs.ErrInvalid)
		})
	}
}

func TestSave_RenameErrorIsTerminalSafeAtTheSource(t *testing.T) {
	// The final record path is a non-empty directory, which a rename of a
	// regular file over it refuses on every platform, root included.
	dir := hostileDir(t, "store")
	if err := os.MkdirAll(filepath.Join(dir, "abc123.json", "keep"), 0o700); err != nil {
		t.Fatal(err)
	}
	err := Save(dir, &Record{ID: "abc123"})
	var linkErr *os.LinkError
	if !errors.As(err, &linkErr) {
		t.Fatalf("want the rename's *os.LinkError, got %T %v", err, err)
	}
	assertSourceSafe(t, err, linkErr.Err)
}

// A history.jsonl that is a symlink to itself fails to open with ELOOP, not
// ErrNotExist, so Scan returns the error rather than an empty list.
func TestScan_HistoryOpenErrorIsTerminalSafeAtTheSource(t *testing.T) {
	home := hostileDir(t, "claude")
	history := filepath.Join(home, "history.jsonl")
	if err := os.Symlink(history, history); err != nil {
		t.Skipf("cannot create the fixture symlink: %v", err)
	}
	_, err := Scan(Paths{ClaudeHome: home, StoreDir: filepath.Join(t.TempDir(), "store")}, Opts{})
	var pathErr *fs.PathError
	if !errors.As(err, &pathErr) {
		t.Fatalf("want the history open's *fs.PathError, got %T %v", err, err)
	}
	assertSourceSafe(t, err, pathErr.Err)
}

// The projects-dir read error is only tested against nil by today's callers;
// it is escaped where it is built so a caller that prints it gets safe text.
// The cached (non-nil receiver) and uncached forms return the same error.
func TestProjectIndex_ReadErrorIsTerminalSafeAtTheSource(t *testing.T) {
	home := hostileDir(t, "claude")
	// projects as a regular file: ReadDir fails on it on every platform.
	if err := os.WriteFile(filepath.Join(home, "projects"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	p := Paths{ClaudeHome: home}
	for _, idx := range []*projectIndex{nil, {}} {
		_, err := idx.list(p)
		var pathErr *fs.PathError
		if !errors.As(err, &pathErr) {
			t.Fatalf("want the ReadDir *fs.PathError, got %T %v", err, err)
		}
		assertSourceSafe(t, err, pathErr.Err)
		if idx != nil {
			if _, again := idx.list(p); again == nil || again.Error() != err.Error() {
				t.Errorf("the cached error differs: %v vs %v", again, err)
			}
		}
	}
}
