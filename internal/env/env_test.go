package env

// Test plan for env.go
//
// Client.CopyValue (Classification: domain, clipboard-touching)
//   [x] Happy: copies KEY's value to the clipboard — fake pbcopy sees
//       Input == the value
//   [x] Unhappy: a missing key errors, never touches the clipboard
//   [x] Unhappy: a clipboard (pbcopy) failure is surfaced with no value in
//       the error
//
// Client.SetFromClipboard (Classification: domain, clipboard-touching)
//   [x] Happy: pastes from the clipboard and writes the file
//   [x] Unhappy: a clipboard (pbpaste) failure is surfaced with no value in
//       the error
//   [x] Unhappy: an invalid key is refused before the clipboard is ever
//       touched ("ValidKey first, refuse before … reading input" applies
//       to the clipboard source exactly as it does to stdin)

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/clip"
	"github.com/cameronsjo/forgectl/internal/exec"
)

func TestCopyValue_CopiesToClipboard(t *testing.T) {
	repo := t.TempDir()
	initGitRepo(t, repo)
	const sentinel = "s3ntinel-VALUE-77x"
	if err := os.WriteFile(filepath.Join(repo, ".env"), []byte("KEY="+sentinel+"\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	fake := &exec.FakeRunner{}
	client := NewClient(clip.New(fake, clip.WithGOOS("darwin")))

	if err := client.CopyValue(context.Background(), repo, ".env", "KEY"); err != nil {
		t.Fatalf("CopyValue: %v", err)
	}

	call := fake.Last()
	if call.Name != "pbcopy" {
		t.Errorf("call.Name = %q, want %q", call.Name, "pbcopy")
	}
	if call.Input != sentinel {
		t.Errorf("call.Input = %q, want %q", call.Input, sentinel)
	}
}

func TestCopyValue_MissingKey_Errors(t *testing.T) {
	repo := t.TempDir()
	initGitRepo(t, repo)
	if err := os.WriteFile(filepath.Join(repo, ".env"), []byte("OTHER=1\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	fake := &exec.FakeRunner{}
	client := NewClient(clip.New(fake, clip.WithGOOS("darwin")))

	err := client.CopyValue(context.Background(), repo, ".env", "MISSING")
	if err == nil {
		t.Fatal("CopyValue with a missing key returned nil error, want a refusal")
	}
	if len(fake.Calls) != 0 {
		t.Errorf("clipboard was touched %d times, want 0 (missing key must fail before pbcopy)", len(fake.Calls))
	}
	if strings.Contains(err.Error(), "MISSING") {
		t.Errorf("error %q echoes the missing key; a not-found token may be a secret pasted into the key slot", err.Error())
	}
}

func TestCopyValue_ClipboardFailure_Surfaced(t *testing.T) {
	repo := t.TempDir()
	initGitRepo(t, repo)
	const sentinel = "s3ntinel-VALUE-77x"
	if err := os.WriteFile(filepath.Join(repo, ".env"), []byte("KEY="+sentinel+"\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	fake := &exec.FakeRunner{
		RunFunc: func(name string, _ []string) (string, error) {
			if name == "pbcopy" {
				return "", errors.New("pbcopy: no such device")
			}
			return "", nil
		},
	}
	client := NewClient(clip.New(fake, clip.WithGOOS("darwin")))

	err := client.CopyValue(context.Background(), repo, ".env", "KEY")
	if err == nil {
		t.Fatal("CopyValue with a failing pbcopy returned nil error, want it surfaced")
	}
	assertNoSecretInOutput(t, sentinel, "", err.Error())
}

func TestSetFromClipboard_PastesAndWrites(t *testing.T) {
	repo := t.TempDir()
	initGitRepo(t, repo)
	const sentinel = "s3ntinel-VALUE-77x"

	fake := &exec.FakeRunner{
		RunFunc: func(name string, _ []string) (string, error) {
			if name == "pbpaste" {
				return sentinel, nil
			}
			return "", nil
		},
	}
	client := NewClient(clip.New(fake, clip.WithGOOS("darwin")))

	tightened, err := client.SetFromClipboard(context.Background(), repo, ".env", "KEY")
	if err != nil {
		t.Fatalf("SetFromClipboard: %v", err)
	}
	if tightened {
		t.Error("tightened = true, want false for a brand-new file")
	}

	got, err := os.ReadFile(filepath.Join(repo, ".env"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if want := "KEY=" + sentinel + "\n"; string(got) != want {
		t.Errorf("file content = %q, want %q", got, want)
	}
}

func TestSetFromClipboard_ClipboardFailure_Surfaced(t *testing.T) {
	repo := t.TempDir()
	initGitRepo(t, repo)

	fake := &exec.FakeRunner{
		RunFunc: func(name string, _ []string) (string, error) {
			if name == "pbpaste" {
				return "", errors.New("pbpaste: no such device")
			}
			return "", nil
		},
	}
	client := NewClient(clip.New(fake, clip.WithGOOS("darwin")))

	_, err := client.SetFromClipboard(context.Background(), repo, ".env", "KEY")
	if err == nil {
		t.Fatal("SetFromClipboard with a failing pbpaste returned nil error, want it surfaced")
	}
	if _, statErr := os.Stat(filepath.Join(repo, ".env")); !os.IsNotExist(statErr) {
		t.Error("file was written despite the clipboard paste failing")
	}
}

func TestSetFromClipboard_InvalidKey_NeverTouchesClipboard(t *testing.T) {
	repo := t.TempDir()
	initGitRepo(t, repo)

	fake := &exec.FakeRunner{}
	client := NewClient(clip.New(fake, clip.WithGOOS("darwin")))

	_, err := client.SetFromClipboard(context.Background(), repo, ".env", "not a valid key")
	if err == nil {
		t.Fatal("SetFromClipboard with an invalid key returned nil error, want a refusal")
	}
	if len(fake.Calls) != 0 {
		t.Errorf("clipboard was touched %d times, want 0 (invalid key must fail before pbpaste)", len(fake.Calls))
	}
}
