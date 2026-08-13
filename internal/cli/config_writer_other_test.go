//go:build !unix

package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/module"
)

func TestConfigWriterOther_VisibleReplacementReportsUnsupportedDurability(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "config.toml")
	action, err := updateConfigLocked(path, nativeConfigWriterOps(), func(raw []byte) ([]byte, error) {
		return append(bytes.Clone(raw), []byte("no_icons = true\n")...), nil
	})
	if action != configWritten || !errors.Is(err, errDirectoryDurabilityUnsupported) {
		t.Fatalf("action=%v error=%v, want visible write with explicit unsupported durability", action, err)
	}
	got, readErr := os.ReadFile(path)
	if readErr != nil || string(got) != "no_icons = true\n" {
		t.Fatalf("visible config=%q error=%v", got, readErr)
	}
}

func TestConfigWriterOther_PreparedUnsupportedBoundaryAllowsNormalInitCommands(t *testing.T) {
	for _, tt := range []struct {
		name string
		cmd  func(*config.LegacyMigrationBoundary) *cobra.Command
	}{
		{name: "launch init", cmd: func(b *config.LegacyMigrationBoundary) *cobra.Command { return newLaunchInitCmd(b) }},
		{name: "top-level init", cmd: func(b *config.LegacyMigrationBoundary) *cobra.Command {
			return newInitCmd(module.Deps{Cfg: config.Config{}, Runner: exec.OSRunner{}, LegacyBoundary: b})
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			base := t.TempDir()
			legacyPath := filepath.Join(base, "claunch", "claunch.conf")
			if err := os.MkdirAll(filepath.Dir(legacyPath), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(legacyPath, []byte("[defaults]\nmodel = \"sonnet\"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			boundary, err := config.PrepareLegacyMigrationBoundary(
				config.EnvSnapshot{Home: base, XDGConfigHome: base, UserConfigHome: base},
				config.NativeMigrationFS(),
			)
			if err != nil {
				t.Fatal(err)
			}
			defer boundary.Close() //nolint:errcheck
			if boundary.Status != config.BoundaryRefused || !errors.Is(boundary.Refusal, config.ErrLegacyMigrationUnsupported) {
				t.Fatalf("boundary status=%v refusal=%v", boundary.Status, boundary.Refusal)
			}
			cmd := tt.cmd(boundary)
			var out, stderr strings.Builder
			cmd.SetOut(&out)
			cmd.SetErr(&stderr)
			if err := cmd.Execute(); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(stderr.String(), "directory durability and cross-process locking are unavailable") {
				t.Fatalf("stderr=%q, want unsupported durability warning", stderr.String())
			}
			data, err := os.ReadFile(boundary.ConfigPath)
			if err != nil || len(data) == 0 {
				t.Fatalf("visible scaffold bytes=%q error=%v", data, err)
			}
			if out.Len() == 0 {
				t.Fatal("normal init produced no success output")
			}
		})
	}
}
