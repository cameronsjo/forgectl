//go:build unix

package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/resume"
)

func TestResumeCommand_RefusedCapturedFIFOIsPrompt(t *testing.T) {
	base := t.TempDir()
	legacyDir := filepath.Join(base, "claunch")
	if err := os.MkdirAll(legacyDir, 0o700); err != nil {
		t.Fatal(err)
	}
	legacyPath := filepath.Join(legacyDir, "claunch.conf")
	if err := unix.Mkfifo(legacyPath, 0o600); err != nil {
		t.Fatal(err)
	}

	boundary, err := config.PrepareLegacyMigrationBoundary(config.EnvSnapshot{
		Home:           filepath.Join(base, "home"),
		XDGConfigHome:  base,
		UserConfigHome: base,
	}, config.NativeMigrationFS())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := boundary.Close(); err != nil {
			t.Errorf("close migration boundary: %v", err)
		}
	})
	if boundary.Status != config.BoundaryRefused || !errors.Is(boundary.Refusal, config.ErrLegacyNonRegular) {
		t.Fatalf("boundary status/refusal = %v/%v, want refused/nonregular", boundary.Status, boundary.Refusal)
	}
	t.Setenv("XDG_CONFIG_HOME", base)

	fakeClaudeBin(t)
	s := resume.Session{ID: "aaaaaaaa-0000-0000-0000-000000000263", Cwd: t.TempDir()}
	pinScan(t, s)
	cmd := newResumeCmd(module.Deps{Cfg: config.Config{}, LegacyBoundary: boundary})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--dry-run", s.ID})

	done := make(chan error, 1)
	go func() { done <- cmd.Execute() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("resume --dry-run returned %v, want prompt built-in fallback", err)
		}
		if !strings.Contains(out.String(), "--model opus") {
			t.Errorf("resume output = %q, want built-in model after FIFO refusal", out.String())
		}
	case <-time.After(500 * time.Millisecond):
		// Release an unsafe implementation without risking a second blocking
		// open: O_NONBLOCK reports ENXIO if no reader is actually waiting.
		fd, releaseErr := unix.Open(legacyPath, unix.O_WRONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
		if releaseErr == nil {
			_, writeErr := unix.Write(fd, []byte("[defaults]\nmodel = \"released\"\n"))
			closeErr := unix.Close(fd)
			releaseErr = errors.Join(writeErr, closeErr)
		}
		select {
		case <-done:
		case <-time.After(time.Second):
		}
		t.Fatalf("resume --dry-run blocked opening the refused legacy FIFO (release: %v)", releaseErr)
	}
}

func TestResumeCommand_UsesCapturedLegacyPathAfterEnvironmentChanges(t *testing.T) {
	capturedBase := t.TempDir()
	capturedLegacyDir := filepath.Join(capturedBase, "claunch")
	if err := os.MkdirAll(capturedLegacyDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(capturedLegacyDir, "claunch.conf"),
		[]byte("[defaults]\nmodel = \"captured-boundary\"\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	boundary, err := config.PrepareLegacyMigrationBoundary(config.EnvSnapshot{
		Home:           filepath.Join(capturedBase, "home"),
		XDGConfigHome:  capturedBase,
		UserConfigHome: capturedBase,
	}, config.NativeMigrationFS())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := boundary.Close(); err != nil {
			t.Errorf("close migration boundary: %v", err)
		}
	})
	if boundary.Status != config.BoundaryMigratable {
		t.Fatalf("boundary status/refusal = %v/%v, want migratable", boundary.Status, boundary.Refusal)
	}

	redirectedBase := t.TempDir()
	redirectedLegacyDir := filepath.Join(redirectedBase, "claunch")
	if err := os.MkdirAll(redirectedLegacyDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(redirectedLegacyDir, "claunch.conf"),
		[]byte("[defaults]\nmodel = \"live-environment\"\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", redirectedBase)

	fakeClaudeBin(t)
	s := resume.Session{ID: "aaaaaaaa-0000-0000-0000-000000000264", Cwd: t.TempDir()}
	pinScan(t, s)
	cmd := newResumeCmd(module.Deps{Cfg: config.Config{}, LegacyBoundary: boundary})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--dry-run", s.ID})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("resume --dry-run returned %v", err)
	}

	if !strings.Contains(out.String(), "--model captured-boundary") {
		t.Errorf("resume output = %q, want model from captured legacy path", out.String())
	}
	if strings.Contains(out.String(), "live-environment") {
		t.Errorf("resume output = %q, used post-capture XDG_CONFIG_HOME", out.String())
	}
}
