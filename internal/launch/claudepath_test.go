package launch

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/cameronsjo/forgectl/internal/config"
)

// TestClaudePath_ExpandsTilde guards the fix that tilde-expands an explicit
// claude path (env or config) before stat, consistent with how Match/AddDir are
// handled. Without it, `binary_path = "~/bin/claude"` fails to resolve.
func TestClaudePath_ExpandsTilde(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("FORGECTL_CLAUDE_BIN", "") // isolate from any ambient override

	binDir := filepath.Join(home, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	stub := filepath.Join(binDir, "claude")
	// #nosec G306 -- a claude stub must be executable for the resolution test.
	if err := os.WriteFile(stub, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Run("config binary_path with tilde", func(t *testing.T) {
		got, err := ClaudePath(config.LaunchDefaults{BinaryPath: "~/bin/claude"})
		if err != nil {
			t.Fatalf("ClaudePath: %v", err)
		}
		if got != stub {
			t.Errorf("ClaudePath = %q, want %q (tilde expanded)", got, stub)
		}
	})

	t.Run("env FORGECTL_CLAUDE_BIN with tilde wins over config", func(t *testing.T) {
		t.Setenv("FORGECTL_CLAUDE_BIN", "~/bin/claude")
		got, err := ClaudePath(config.LaunchDefaults{BinaryPath: "/nonexistent/claude"})
		if err != nil {
			t.Fatalf("ClaudePath: %v", err)
		}
		if got != stub {
			t.Errorf("ClaudePath = %q, want %q (env tilde expanded, wins over config)", got, stub)
		}
	})
}
