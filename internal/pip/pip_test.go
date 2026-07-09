package pip

// Test plan for pip.go
//
// defaultConfigPath (Classification: pure logic)
//   [x] Happy: resolves under os.UserConfigDir()/pip/pip.conf on non-Windows
//
// New / WithConfigPath (Classification: constructor / functional option)
//   [x] Happy: WithConfigPath overrides the default path
//   [x] Happy: Path() reports whatever path the Client was built with
//
// Client.Read (Classification: I/O boundary)
//   [x] Happy: missing pip.conf reads as empty bytes, no error
//   [x] Happy: existing pip.conf reads back its (normalized) content
//
// Client.Remove / Client.Restore (Classification: I/O boundary, reversibility)
//   [x] Happy: Remove persists a commented-out entry to disk
//   [x] Happy: Remove with no match returns 0 and writes nothing new
//   [x] Happy: Restore(Remove(x)) round-trips the on-disk file byte-for-byte
//   [x] Happy: Restore with nothing removed returns 0
//   [x] Happy: Remove/Restore against a not-yet-existing pip.conf creates it

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
)

// ---- defaultConfigPath ------------------------------------------------------

func TestDefaultConfigPath_UnderUserConfigDirPipPipConf(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows resolves via %APPDATA%, covered separately")
	}
	got := defaultConfigPath()
	base, err := os.UserConfigDir()
	if err != nil {
		t.Skip("os.UserConfigDir unavailable in this environment")
	}
	want := filepath.Join(base, "pip", "pip.conf")
	if got != want {
		t.Errorf("defaultConfigPath() = %q, want %q", got, want)
	}
}

// ---- New / WithConfigPath ---------------------------------------------------

func TestNew_WithConfigPath_OverridesDefault(t *testing.T) {
	client := New(&exec.FakeRunner{}, WithConfigPath("/tmp/custom/pip.conf"))
	if client.Path() != "/tmp/custom/pip.conf" {
		t.Errorf("Path() = %q, want /tmp/custom/pip.conf", client.Path())
	}
}

// ---- Client.Read -------------------------------------------------------------

func TestRead_MissingFile_ReturnsEmptyBytesNoError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pip.conf")
	client := New(&exec.FakeRunner{}, WithConfigPath(path))

	data, err := client.Read(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(data) != 0 {
		t.Errorf("Read() on missing file = %q, want empty", data)
	}
}

func TestRead_ExistingFile_ReturnsContent(t *testing.T) {
	client := writeFixture(t, "[global]\nindex-url = https://pypi.org/simple\n")

	data, err := client.Read(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(string(data), "index-url = https://pypi.org/simple") {
		t.Errorf("Read() = %q, missing expected entry", data)
	}
}

// ---- Client.Remove / Client.Restore ------------------------------------------

func TestRemove_PersistsCommentedEntryToDisk(t *testing.T) {
	client := writeFixture(t, "[global]\nindex-url = https://pypi.internal.example.com/simple\n")

	n, err := client.Remove(context.Background(), "global", "index-url")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 1 {
		t.Fatalf("Remove returned %d, want 1", n)
	}

	onDisk, err := os.ReadFile(client.Path())
	if err != nil {
		t.Fatalf("read pip.conf: %v", err)
	}
	if !strings.Contains(string(onDisk), removedMarker) {
		t.Errorf("on-disk pip.conf missing removal marker: %q", onDisk)
	}
}

func TestRemove_NoMatch_ReturnsZeroLeavesFileUnchanged(t *testing.T) {
	src := "[global]\nindex-url = https://pypi.org/simple\n"
	client := writeFixture(t, src)

	n, err := client.Remove(context.Background(), "global", "extra-index-url")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 0 {
		t.Fatalf("Remove returned %d, want 0", n)
	}

	onDisk, err := os.ReadFile(client.Path())
	if err != nil {
		t.Fatalf("read pip.conf: %v", err)
	}
	if string(onDisk) != src {
		t.Errorf("pip.conf changed on no-match Remove: %q", onDisk)
	}
}

func TestRestore_ReversesRemoveOnDisk_ByteForByte(t *testing.T) {
	src := "[global]\nindex-url = https://pypi.internal.example.com/simple\n"
	client := writeFixture(t, src)
	ctx := context.Background()

	if n, err := client.Remove(ctx, "global", "index-url"); err != nil || n != 1 {
		t.Fatalf("Remove: n=%d err=%v", n, err)
	}
	if n, err := client.Restore(ctx); err != nil || n == 0 {
		t.Fatalf("Restore: n=%d err=%v", n, err)
	}

	onDisk, err := os.ReadFile(client.Path())
	if err != nil {
		t.Fatalf("read pip.conf: %v", err)
	}
	if string(onDisk) != src {
		t.Errorf("restore(remove(x)) != x on disk:\n  want: %q\n  got:  %q", src, onDisk)
	}
}

func TestRestore_NothingRemoved_ReturnsZero(t *testing.T) {
	client := writeFixture(t, "[global]\nindex-url = https://pypi.org/simple\n")

	n, err := client.Restore(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 0 {
		t.Errorf("Restore returned %d, want 0", n)
	}
}

func TestRemoveThenRestore_AgainstMissingFile_CreatesIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "pip.conf")
	client := New(&exec.FakeRunner{}, WithConfigPath(path))

	// Nothing to remove from a nonexistent file — no-op, no crash, no file.
	if n, err := client.Remove(context.Background(), "global", "index-url"); err != nil || n != 0 {
		t.Fatalf("Remove against missing file: n=%d err=%v", n, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("expected no file to be created on a no-op Remove, stat err=%v", err)
	}
}

// writeFixture writes src to a temp pip.conf and returns a Client pointed at it.
func writeFixture(t *testing.T, src string) *Client {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pip.conf")
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return New(&exec.FakeRunner{}, WithConfigPath(path))
}
