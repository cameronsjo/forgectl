package config

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

const binaryTrustBody = "[launch.defaults]\nbinary_path = \"/x/claude\"\ncodex_binary_path = \"/x/codex\"\npi_binary_path = \"/x/pi\"\n"

func TestLoadPath_RecordsTheBinaryVerdictForEachHarness(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the owner and mode check needs a Unix stat")
	}
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(binaryTrustBody), 0o600); err != nil {
		t.Fatal(err)
	}
	d := LoadPath(path).Launch.Defaults
	for _, h := range []string{"claude", "codex", "pi"} {
		if err := d.BinaryFileError(h); err != nil {
			t.Errorf("%s: 0600 file refused: %v", h, err)
		}
	}
	if err := os.Chmod(path, 0o666); err != nil { //nolint:gosec // G302: the test needs a loose mode to prove it is refused
		t.Fatal(err)
	}
	d = LoadPath(path).Launch.Defaults
	for _, h := range []string{"claude", "codex", "pi"} {
		if err := d.BinaryFileError(h); err == nil {
			t.Errorf("%s: a world-writable file was trusted", h)
		}
	}
	if d.BinaryFileError("bogus") != nil {
		t.Error("an unknown harness got a verdict")
	}
	if d.BinaryPath != "/x/claude" || d.CodexBinaryPath != "/x/codex" || d.PiBinaryPath != "/x/pi" {
		t.Errorf("the verdict must not stop the decode: %+v", d)
	}
}

func TestLegacyBinaryPathsAreRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claunch.conf")
	if err := os.WriteFile(path, []byte("[defaults]\nbinary_path = \"/x/claude\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var fs nativeMigrationFS
	lc, err := fs.loadReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{"claude", "codex", "pi"} {
		if !errors.Is(lc.Defaults.BinaryFileError(h), ErrLegacyBinaryPath) {
			t.Errorf("%s: legacy read-only load was not marked", h)
		}
	}
}

// TestMergeLegacyIntoLaunch_BinaryVerdictIsPerKey: a path legacy filled carries
// the legacy refusal; a path config.toml set keeps its own verdict.
func TestMergeLegacyIntoLaunch_BinaryVerdictIsPerKey(t *testing.T) {
	cfg := Config{Launch: LaunchConfig{Defaults: LaunchDefaults{BinaryPath: "/own/claude"}}}
	legacy := LaunchConfig{Defaults: LaunchDefaults{BinaryPath: "/legacy/claude", CodexBinaryPath: "/legacy/codex"}}
	merged, _ := MergeLegacyIntoLaunch(cfg, legacy)
	d := merged.Defaults
	if d.BinaryPath != "/own/claude" || d.BinaryFileError("claude") != nil {
		t.Errorf("config's own claude path: %q, verdict %v; want it kept and trusted", d.BinaryPath, d.BinaryFileError("claude"))
	}
	if d.CodexBinaryPath != "/legacy/codex" || !errors.Is(d.BinaryFileError("codex"), ErrLegacyBinaryPath) {
		t.Errorf("legacy codex path: %q, verdict %v; want it filled and refused", d.CodexBinaryPath, d.BinaryFileError("codex"))
	}
}

func TestLoadLegacyLaunch_BinaryPathsAreRefused(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	path, err := LegacyLaunchPath()
	if err != nil {
		t.Skipf("no legacy path on this platform: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("[defaults]\nbinary_path = \"/x/claude\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	lc, _, err := LoadLegacyLaunch()
	if err != nil {
		t.Fatal(err)
	}
	if !errors.Is(lc.Defaults.BinaryFileError("claude"), ErrLegacyBinaryPath) {
		t.Error("LoadLegacyLaunch did not mark the legacy binary path")
	}
}
