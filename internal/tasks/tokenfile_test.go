package tasks

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// writeTokenFile writes content to a fresh file under t.TempDir with mode and
// returns the path. The parent directory is 0700 so a permissive-mode test is
// asserting the FILE's mode, not inheriting a lax umask from the tree.
func writeTokenFile(t *testing.T, content string, mode os.FileMode) string {
	t.Helper()
	dir := t.TempDir()
	// 0700, not 0600: a DIRECTORY needs the execute bit to be traversable, so
	// the gosec rule's file-shaped advice would make the token file below
	// unreachable rather than more private. Owner-only either way.
	if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // G302: a directory needs 0700; 0600 makes it non-traversable
		t.Fatalf("chmod tempdir: %v", err)
	}
	path := filepath.Join(dir, "token")
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatalf("write token file: %v", err)
	}
	// os.WriteFile applies the umask, so the requested mode is not necessarily
	// the mode on disk. Chmod after the fact is what actually sets it — a test
	// asserting a refusal on 0644 must be sure the file really is 0644.
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("chmod token file: %v", err)
	}
	return path
}

func TestReadTokenFile_ReadsAValidToken(t *testing.T) {
	path := writeTokenFile(t, fakeToken+"\n", 0o400)
	tok, err := ReadTokenFile(path)
	if err != nil {
		t.Fatalf("ReadTokenFile: %v", err)
	}
	if !tok.Present() {
		t.Fatal("ReadTokenFile: token not present")
	}
	if got := tok.Header(); got != "Bearer "+fakeToken {
		t.Fatalf("Header() = %q, want the bearer form of the file's contents", got)
	}
}

func TestReadTokenFile_RejectsMalformedValue(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string
	}{
		{"not a token at all", "hunter2"},
		{"missing the tk_ prefix", strings.Repeat("a", 40)},
		{"hex too short", "tk_" + strings.Repeat("a", 39)},
		{"non-hex body", "tk_" + strings.Repeat("z", 40)},
		{"empty file", ""},
		{"whitespace only", "   \n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeTokenFile(t, tc.content, 0o400)
			_, err := ReadTokenFile(path)
			if err == nil {
				t.Fatal("ReadTokenFile(malformed) = nil error, want a refusal")
			}
			if !errors.Is(err, ErrTokenMalformed) && !errors.Is(err, ErrTokenNotFound) {
				t.Fatalf("ReadTokenFile(malformed) = %v, want ErrTokenMalformed or ErrTokenNotFound", err)
			}
			if strings.Contains(err.Error(), tc.content) && tc.content != "" && strings.TrimSpace(tc.content) != "" {
				t.Fatalf("the rejected value leaked into the error string: %v", err)
			}
		})
	}
}

func TestReadTokenFile_RejectsMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist")
	_, err := ReadTokenFile(path)
	if !errors.Is(err, ErrTokenNotFound) {
		t.Fatalf("ReadTokenFile(missing) = %v, want errors.Is(ErrTokenNotFound)", err)
	}
}

// TestReadTokenFile_RefusesAWorldReadableFile is the mode check. A token file
// any process on the host can read is a disclosure the container transport is
// specifically meant to avoid — refusing loudly at startup is the whole point,
// because a permissive file otherwise works perfectly and nothing ever says so.
func TestReadTokenFile_RefusesAWorldReadableFile(t *testing.T) {
	for _, mode := range []os.FileMode{0o444, 0o644, 0o664, 0o604} {
		path := writeTokenFile(t, fakeToken+"\n", mode)
		_, err := ReadTokenFile(path)
		if !errors.Is(err, ErrTokenFileMode) {
			t.Fatalf("ReadTokenFile(mode %04o) = %v, want errors.Is(ErrTokenFileMode)", mode, err)
		}
	}
}

// TestReadTokenFile_RefusesANonRegularFile: Stat reports size 0 for a FIFO,
// so the size ceiling passes and os.ReadFile then BLOCKS FOREVER waiting for a
// writer — hanging startup before the listener ever opens, which presents as a
// container that never becomes healthy rather than as a bad credential path.
func TestReadTokenFile_RefusesANonRegularFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skipf("cannot create a FIFO here: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := ReadTokenFile(path)
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, ErrTokenNotFound) {
			t.Fatalf("ReadTokenFile(fifo) = %v, want errors.Is(ErrTokenNotFound)", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ReadTokenFile blocked on a FIFO instead of refusing it")
	}
}

func TestReadTokenFile_RefusesAGroupReadableFile(t *testing.T) {
	path := writeTokenFile(t, fakeToken+"\n", 0o440)
	_, err := ReadTokenFile(path)
	if !errors.Is(err, ErrTokenFileMode) {
		t.Fatalf("ReadTokenFile(0440) = %v, want errors.Is(ErrTokenFileMode)", err)
	}
}

func TestReadTokenFile_AcceptsOwnerOnlyModes(t *testing.T) {
	for _, mode := range []os.FileMode{0o400, 0o600} {
		path := writeTokenFile(t, fakeToken+"\n", mode)
		if _, err := ReadTokenFile(path); err != nil {
			t.Fatalf("ReadTokenFile(mode %04o) = %v, want accepted", mode, err)
		}
	}
}

// TestReadTokenFile_TokenNeverReachesALogLine captures slog across the whole
// read-and-use path. A doc comment is not a control; this is.
func TestReadTokenFile_TokenNeverReachesALogLine(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	path := writeTokenFile(t, fakeToken+"\n", 0o400)
	tok, err := ReadTokenFile(path)
	if err != nil {
		t.Fatalf("ReadTokenFile: %v", err)
	}
	slog.Info("token read", "token", tok, "path", path)
	slog.Info("token read", "token", slog.AnyValue(tok))

	if strings.Contains(buf.String(), fakeToken) {
		t.Fatalf("the token literal reached a log line: %s", buf.String())
	}
	if !strings.Contains(buf.String(), Redacted) {
		t.Fatalf("expected the redacted rendering in the log output, got: %s", buf.String())
	}
}
