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

func TestWithBinaryTrustFrom(t *testing.T) {
	refusal := errors.New("loose")
	base := LaunchConfig{Defaults: LaunchDefaults{BinaryPath: "/a", CodexBinaryPath: "/c", binaryTrust: binaryTrust{claude: refusal, codex: nil}}}
	rebuilt := LaunchConfig{Defaults: LaunchDefaults{BinaryPath: "/a", CodexBinaryPath: "/c", PiBinaryPath: "/p"}}
	got := rebuilt.WithBinaryTrustFrom(base).Defaults
	if got.BinaryFileError("claude") != refusal {
		t.Errorf("claude verdict = %v, want the base's", got.BinaryFileError("claude"))
	}
	if got.BinaryFileError("codex") != nil {
		t.Errorf("codex verdict = %v, want nil: the base trusted it", got.BinaryFileError("codex"))
	}
	if !errors.Is(got.BinaryFileError("pi"), ErrLegacyBinaryPath) {
		t.Errorf("pi verdict = %v, want a path the base does not hold refused", got.BinaryFileError("pi"))
	}
	changed := LaunchConfig{Defaults: LaunchDefaults{BinaryPath: "/other"}}.WithBinaryTrustFrom(base).Defaults
	if !errors.Is(changed.BinaryFileError("claude"), ErrLegacyBinaryPath) {
		t.Errorf("a path that differs from the base's was trusted: %v", changed.BinaryFileError("claude"))
	}
	own := LaunchConfig{Defaults: LaunchDefaults{BinaryPath: "/a", binaryTrust: binaryTrust{claude: ErrLegacyBinaryPath}}}.WithBinaryTrustFrom(base).Defaults
	if !errors.Is(own.BinaryFileError("claude"), ErrLegacyBinaryPath) {
		t.Errorf("a verdict the key already had was overwritten: %v", own.BinaryFileError("claude"))
	}
}

func TestWithoutLegacyBinaryPaths(t *testing.T) {
	lc := LaunchConfig{Defaults: LaunchDefaults{
		BinaryPath: "/own", CodexBinaryPath: "/legacy-c", PiBinaryPath: "/legacy-p", Model: "m",
		binaryTrust: binaryTrust{claude: nil, codex: ErrLegacyBinaryPath, pi: ErrLegacyBinaryPath},
	}}
	got, dropped := lc.WithoutLegacyBinaryPaths()
	if got.Defaults.BinaryPath != "/own" || got.Defaults.CodexBinaryPath != "" || got.Defaults.PiBinaryPath != "" || got.Defaults.Model != "m" {
		t.Errorf("defaults = %+v, want only the legacy paths cleared", got.Defaults)
	}
	if len(dropped) != 2 || dropped[0] != "codex_binary_path" || dropped[1] != "pi_binary_path" {
		t.Errorf("dropped = %v", dropped)
	}
	if lc.Defaults.CodexBinaryPath != "/legacy-c" {
		t.Error("the receiver was modified")
	}
}

func TestDescribe_RecordsTheBinaryVerdict(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the owner and mode check needs a Unix stat")
	}
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(binaryTrustBody), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o666); err != nil { //nolint:gosec // G302: the test needs a loose mode to prove it is refused
		t.Fatal(err)
	}
	cfg, _ := describeFile(path)
	if cfg.Launch.Defaults.BinaryFileError("claude") == nil || cfg.ExecChoiceFileError() == nil {
		t.Error("the resolved config view dropped the verdict a launch would refuse on")
	}
}
