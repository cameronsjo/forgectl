package clip

// Test plan for clip.go
//
// Client.Copy / Client.Paste (Classification: ops layer)
//   [x] Happy: Copy shells `pbcopy` with the given string piped in as stdin
//   [x] Happy: Paste shells `pbpaste` with no args and returns its stdout
//   [x] Unhappy: on a non-Darwin GOOS, Copy/Paste fail fast with a clear
//       "macOS only" error and never touch the Runner (guarded via WithGOOS,
//       a test-only hook, since exercising the real runtime.GOOS default
//       would only ever run one branch depending on the CI host's OS)

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
)

// captureSlog installs a scratch slog default logger writing to a buffer,
// restored via t.Cleanup.
func captureSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func TestCopy_ShellsPbcopyWithStdin(t *testing.T) {
	fake := &exec.FakeRunner{}
	c := New(fake, WithGOOS("darwin"))

	if err := c.Copy(context.Background(), "hello clipboard"); err != nil {
		t.Fatalf("Copy: %v", err)
	}

	call := fake.Last()
	if call.Name != "pbcopy" {
		t.Errorf("call.Name = %q, want %q", call.Name, "pbcopy")
	}
	if len(call.Args) != 0 {
		t.Errorf("call.Args = %v, want empty", call.Args)
	}
	if call.Input != "hello clipboard" {
		t.Errorf("call.Input = %q, want %q", call.Input, "hello clipboard")
	}
}

func TestPaste_ShellsPbpaste_ReturnsStdout(t *testing.T) {
	fake := &exec.FakeRunner{
		RunFunc: func(name string, args []string) (string, error) {
			if name == "pbpaste" {
				return "pasted contents", nil
			}
			return "", nil
		},
	}
	c := New(fake, WithGOOS("darwin"))

	got, err := c.Paste(context.Background())
	if err != nil {
		t.Fatalf("Paste: %v", err)
	}
	if got != "pasted contents" {
		t.Errorf("Paste = %q, want %q", got, "pasted contents")
	}

	call := fake.Last()
	if call.Name != "pbpaste" {
		t.Errorf("call.Name = %q, want %q", call.Name, "pbpaste")
	}
	if len(call.Args) != 0 {
		t.Errorf("call.Args = %v, want empty", call.Args)
	}
}

func TestCopy_WithoutSensitive_KeepsByteLength(t *testing.T) {
	fake := &exec.FakeRunner{}
	buf := captureSlog(t)
	c := New(fake, WithGOOS("darwin"))

	if err := c.Copy(context.Background(), "some clipboard value"); err != nil {
		t.Fatalf("Copy: %v", err)
	}
	if !strings.Contains(buf.String(), "bytes=") {
		t.Errorf("log output = %q, want the byte-length field preserved (no option = current logging)", buf.String())
	}
}

func TestCopy_WithSensitive_OmitsByteLength(t *testing.T) {
	fake := &exec.FakeRunner{}
	buf := captureSlog(t)
	c := New(fake, WithGOOS("darwin"), WithSensitive())

	if err := c.Copy(context.Background(), "some clipboard value"); err != nil {
		t.Fatalf("Copy: %v", err)
	}
	if strings.Contains(buf.String(), "bytes=") {
		t.Errorf("log output = %q, want no byte-length field with WithSensitive", buf.String())
	}
}

func TestPaste_WithoutSensitive_KeepsByteLength(t *testing.T) {
	fake := &exec.FakeRunner{
		RunFunc: func(name string, _ []string) (string, error) {
			if name == "pbpaste" {
				return "pasted contents", nil
			}
			return "", nil
		},
	}
	buf := captureSlog(t)
	c := New(fake, WithGOOS("darwin"))

	if _, err := c.Paste(context.Background()); err != nil {
		t.Fatalf("Paste: %v", err)
	}
	if !strings.Contains(buf.String(), "bytes=") {
		t.Errorf("log output = %q, want the byte-length field preserved (no option = current logging)", buf.String())
	}
}

func TestPaste_WithSensitive_OmitsByteLength(t *testing.T) {
	fake := &exec.FakeRunner{
		RunFunc: func(name string, _ []string) (string, error) {
			if name == "pbpaste" {
				return "pasted contents", nil
			}
			return "", nil
		},
	}
	buf := captureSlog(t)
	c := New(fake, WithGOOS("darwin"), WithSensitive())

	if _, err := c.Paste(context.Background()); err != nil {
		t.Fatalf("Paste: %v", err)
	}
	if strings.Contains(buf.String(), "bytes=") {
		t.Errorf("log output = %q, want no byte-length field with WithSensitive", buf.String())
	}
}

func TestCopy_NonDarwin_FailsFastWithoutTouchingRunner(t *testing.T) {
	fake := &exec.FakeRunner{}
	c := New(fake, WithGOOS("linux"))

	err := c.Copy(context.Background(), "hello")
	if err == nil {
		t.Fatal("expected an error on non-Darwin GOOS")
	}
	if !errors.Is(err, errMacOSOnly) {
		t.Errorf("error = %v, want errMacOSOnly", err)
	}
	if len(fake.Calls) != 0 {
		t.Errorf("Runner was called %d times, want 0 (guard must short-circuit)", len(fake.Calls))
	}
}

func TestPaste_NonDarwin_FailsFastWithoutTouchingRunner(t *testing.T) {
	fake := &exec.FakeRunner{}
	c := New(fake, WithGOOS("windows"))

	_, err := c.Paste(context.Background())
	if err == nil {
		t.Fatal("expected an error on non-Darwin GOOS")
	}
	if !errors.Is(err, errMacOSOnly) {
		t.Errorf("error = %v, want errMacOSOnly", err)
	}
	if len(fake.Calls) != 0 {
		t.Errorf("Runner was called %d times, want 0 (guard must short-circuit)", len(fake.Calls))
	}
}
