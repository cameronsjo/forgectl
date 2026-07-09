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
	"context"
	"errors"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
)

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
