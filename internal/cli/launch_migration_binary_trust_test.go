package cli

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/config"
)

// TestMigration_DoesNotCopyALegacyBinaryPathIntoConfig: a binary path from the
// legacy file was never judged for who could write it, and the config.toml the
// migration writes is trusted at the next load. The path must stay out of it,
// stay in the backup, and be named in the notice. This launch still sees the
// path, refused.
func TestMigration_DoesNotCopyALegacyBinaryPathIntoConfig(t *testing.T) {
	base := t.TempDir()
	legacy := []byte("[defaults]\nmodel = \"sonnet\"\nbinary_path = \"/legacy/claude\"\n")
	b := transactionBoundary(t, base, legacy)

	eff, notice, _ := autoMigrateOrWarnLegacyLaunch(b, config.Config{})
	if eff.Defaults.Model != "sonnet" {
		t.Fatalf("effective model = %q, want sonnet", eff.Defaults.Model)
	}
	if got, err := os.ReadFile(b.ConfigPath); err != nil || strings.Contains(string(got), "/legacy/claude") || !strings.Contains(string(got), "sonnet") {
		t.Fatalf("config.toml = %q, %v; want the model migrated and the legacy path left out", got, err)
	}
	if !strings.Contains(notice, "binary_path") || !strings.Contains(notice, "[launch.defaults]") {
		t.Errorf("notice = %q, want it to name the dropped key and the fix", notice)
	}
	if !errors.Is(eff.Defaults.BinaryFileError("claude"), config.ErrLegacyBinaryPath) {
		t.Errorf("this launch's legacy path is not refused: %v", eff.Defaults.BinaryFileError("claude"))
	}
	if next := config.LoadPath(b.ConfigPath).Launch.Defaults; next.BinaryPath != "" {
		t.Errorf("next load sees binary_path %q; the migration laundered it", next.BinaryPath)
	}
}

// TestMigration_KeepsTheConfigFileVerdictForItsOwnKeys: the migration re-reads
// config.toml under its lock and decodes it with no verdict; the launch it
// returns must carry the verdict LoadPath recorded, or a group-writable
// config.toml's own binary_path would resolve as soon as a legacy file exists.
func TestMigration_KeepsTheConfigFileVerdictForItsOwnKeys(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the owner and mode check needs a Unix stat")
	}
	base := t.TempDir()
	b := transactionBoundary(t, base, []byte("[defaults]\ncodex_binary_path = \"/legacy/codex\"\n"))
	if err := os.MkdirAll(filepath.Dir(b.ConfigPath), 0o700); err != nil {
		t.Fatal(err)
	}
	body := "[launch.defaults]\nmodel = \"opus\"\nbinary_path = \"/own/claude\"\n"
	if err := os.WriteFile(b.ConfigPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(b.ConfigPath, 0o666); err != nil { //nolint:gosec // G302: the test needs a loose mode to prove it is refused
		t.Fatal(err)
	}
	cfg := config.LoadPath(b.ConfigPath)
	if cfg.Launch.Defaults.BinaryFileError("claude") == nil {
		t.Fatal("the fixture is not loose; the test proves nothing")
	}

	eff, _, _ := autoMigrateOrWarnLegacyLaunch(b, cfg)
	if eff.Defaults.BinaryPath != "/own/claude" {
		t.Fatalf("effective binary_path = %q, want config.toml's own", eff.Defaults.BinaryPath)
	}
	if eff.Defaults.BinaryFileError("claude") == nil || errors.Is(eff.Defaults.BinaryFileError("claude"), config.ErrLegacyBinaryPath) {
		t.Errorf("config.toml's own key lost its verdict: %v", eff.Defaults.BinaryFileError("claude"))
	}
	if !errors.Is(eff.Defaults.BinaryFileError("codex"), config.ErrLegacyBinaryPath) {
		t.Errorf("the legacy-filled codex key is not refused: %v", eff.Defaults.BinaryFileError("codex"))
	}
}
