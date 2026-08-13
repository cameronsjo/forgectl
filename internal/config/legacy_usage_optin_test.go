package config

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/BurntSushi/toml"
)

// The migration writers re-encode a whole LaunchConfig. TOML puts every scalar
// in a table BEFORE that table's sub-tables, so a scalar declared after
// Projects in the struct must still be emitted ahead of [launch.defaults] and
// [[launch.project]] — otherwise it silently reattaches to the last project
// block on the next read, and an operator's opt-in changes meaning without
// anything failing.
func TestLaunchConfig_UsageStatsSurvivesTOMLRoundTrip(t *testing.T) {
	original := LaunchConfig{
		Defaults:   LaunchDefaults{Harness: "claude", Model: "opus"},
		Projects:   []LaunchProject{{Match: "~/Projects/one", Model: "sonnet"}},
		UsageStats: true,
	}
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(struct {
		Launch LaunchConfig `toml:"launch"`
	}{Launch: original}); err != nil {
		t.Fatalf("encode: %v", err)
	}

	cfg, err := DecodeStrict(buf.Bytes())
	if err != nil {
		t.Fatalf("decode re-encoded config: %v\n%s", err, buf.String())
	}
	if !cfg.Launch.UsageStats {
		t.Fatalf("usage_stats did not survive the round trip:\n%s", buf.String())
	}
	if len(cfg.Launch.Projects) != 1 || cfg.Launch.Projects[0].Match != "~/Projects/one" {
		t.Fatalf("project profiles did not survive the round trip:\n%s", buf.String())
	}
	if cfg.Launch.Defaults.Model != "opus" {
		t.Fatalf("defaults did not survive the round trip:\n%s", buf.String())
	}
}

// The legacy claunch.conf is a compatibility source, not a consent surface. A
// `usage_stats = true` planted there must not switch collection on, and must
// not ride an automatic migration into config.toml.
func TestLaunchMigration_NeverEnablesUsageStats(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	dir := filepath.Join(xdg, "claunch")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := "usage_stats = true\n[defaults]\nharness = \"claude\"\nmodel = \"opus\"\n"
	if err := os.WriteFile(filepath.Join(dir, "claunch.conf"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	lc, _, err := LoadLegacyLaunch()
	if err != nil {
		t.Fatalf("LoadLegacyLaunch: %v", err)
	}
	if lc.UsageStats {
		t.Fatal("a legacy claunch.conf enabled usage statistics")
	}
	// The rest of the legacy section still loads — the strip is surgical, not
	// a refusal to read the file.
	if lc.Defaults.Model != "opus" {
		t.Fatalf("legacy model = %q, want it preserved", lc.Defaults.Model)
	}

	env, err := CaptureEnvSnapshot()
	if err != nil {
		t.Skipf("environment snapshot unavailable: %v", err)
	}
	boundary, err := PrepareLegacyMigrationBoundary(env, NativeMigrationFS())
	if err != nil {
		t.Skipf("migration boundary unavailable: %v", err)
	}
	defer boundary.Close() //nolint:errcheck // read-only in this test
	fromBoundary, err := boundary.LoadReadOnlyLegacy()
	if err != nil {
		t.Fatalf("LoadReadOnlyLegacy: %v", err)
	}
	if fromBoundary.UsageStats {
		t.Fatal("the migration boundary carried a legacy usage_stats opt-in through")
	}
}
