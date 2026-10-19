//go:build unix

package goal

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/cameronsjo/forgectl/internal/config"
)

func TestResolveNativeBinaryHonorsConfiguredPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pi")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil { //nolint:gosec // Test helper must be executable.
		t.Fatal(err)
	}
	got, err := resolveNativeBinary("pi", config.LaunchDefaults{PiBinaryPath: path})
	if err != nil {
		t.Fatalf("resolveNativeBinary: %v", err)
	}
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Path != canonical {
		t.Fatalf("resolved path = %q, want %q", got.Path, canonical)
	}
}

func TestResolveNativeBinaryHonorsEnvironmentOverride(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil { //nolint:gosec // Test helper must be executable.
		t.Fatal(err)
	}
	t.Setenv("FORGECTL_CLAUDE_BIN", path)
	got, err := resolveNativeBinary("claude", config.LaunchDefaults{})
	if err != nil {
		t.Fatalf("resolveNativeBinary: %v", err)
	}
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Path != canonical {
		t.Fatalf("resolved path = %q, want %q", got.Path, canonical)
	}
}
