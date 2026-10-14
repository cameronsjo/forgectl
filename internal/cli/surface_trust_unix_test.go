//go:build unix

package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/cameronsjo/forgectl/internal/config"
)

// TestSurfaceLaunchDryRun_RefusesAnUntrustedFolder is forgectl#1240 through
// the command: --dry-run refuses a claude launch into a folder the account
// has not trusted, and passes once it is.
func TestSurfaceLaunchDryRun_RefusesAnUntrustedFolder(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "tmux"), []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil { //nolint:gosec // G306: a backend stub must be executable
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfgDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfgDir)
	target, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	deps := dryRunDeps(t)
	deps.Cfg.Launch.Defaults = config.LaunchDefaults{Harness: "claude", BinaryPath: stubHarness(t)}
	run := func() error {
		cmd := newSurfaceLaunchCmd(deps)
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs([]string{target, "--surface", "tmux", "--dry-run", "--here"})
		return cmd.ExecuteContext(t.Context())
	}

	writeClaudeConfig(t, cfgDir, `{"projects":{"/nowhere":{"hasTrustDialogAccepted":true}}}`)
	if err := run(); !errors.Is(err, errUntrustedFolder) {
		t.Fatalf("untrusted: err = %v, want errUntrustedFolder", err)
	}
	writeClaudeConfig(t, cfgDir, `{"projects":{"`+filepath.ToSlash(target)+`":{"hasTrustDialogAccepted":true}}}`)
	if err := run(); err != nil {
		t.Fatalf("trusted: %v", err)
	}
}
