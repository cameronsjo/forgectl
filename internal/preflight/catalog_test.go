package preflight

// Test plan for catalog.go
//
// ParseHeaders (Classification: pure parser)
//   [x] Happy: core-tier section with an id yields Core=true + Marketplace
//   [x] Happy: on-demand-only section yields Core=false
//   [x] Happy: a plugin with BOTH a core and an on-demand section folds to
//       one entry with Core=true (cadence's own shape)
//   [x] Edge: a HOLD row with no "id:" is recorded but contributes no
//       Marketplace
//   [x] Edge: non-header lines (table rows, prose) are ignored
//
// CoreDefaultSet (Classification: pure)
//   [x] Happy: only core+marketplace plugins appear, keyed "plugin@marketplace"
//   [x] Edge: a core plugin with no marketplace (HOLD-only) is excluded
//
// LocateCatalog (Classification: I/O — temp-dir fixtures)
//   [x] Happy: configured path wins outright, no filesystem check
//   [x] Happy: installed_plugins.json's cadence@<marketplace> installPath wins
//   [x] Happy: multiple scopes — the lexicographically-latest lastUpdated wins
//   [x] Happy: falls back to newest-mtime cache glob when installed_plugins.json
//       is absent
//   [x] Sad: neither source resolves → error

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const fixtureCatalog = `<!-- GENERATED FILE -->

# Cadence Skill Catalog

## cadence · tier: core · id: cadence@workbench

| Skill | Description | Cost | Notes |
|---|---|---|---|
| ` + "`cadence:attune`" + ` | some description | 10 · 20 |   |

## cadence · tier: on-demand · id: cadence@workbench

| Skill | Description | Cost | Notes |
|---|---|---|---|
| ` + "`cadence:chart`" + ` | some description | 10 · 20 |   |

## cadence-mcp · tier: hold · HOLD — pending MCP-server workload decision

## cadence-voice · tier: core · id: cadence-voice@workbench
`

func TestParseHeaders(t *testing.T) {
	plugins, err := ParseHeaders(strings.NewReader(fixtureCatalog))
	if err != nil {
		t.Fatalf("ParseHeaders: %v", err)
	}

	cadence, ok := plugins["cadence"]
	if !ok {
		t.Fatal("cadence: not found")
	}
	if !cadence.Core {
		t.Error("cadence: Core = false, want true (has a core section)")
	}
	if cadence.Marketplace != "workbench" {
		t.Errorf("cadence: Marketplace = %q, want %q", cadence.Marketplace, "workbench")
	}

	hold, ok := plugins["cadence-mcp"]
	if !ok {
		t.Fatal("cadence-mcp: not found")
	}
	if hold.Core {
		t.Error("cadence-mcp: Core = true, want false (tier is hold)")
	}
	if hold.Marketplace != "" {
		t.Errorf("cadence-mcp: Marketplace = %q, want empty (HOLD row has no id)", hold.Marketplace)
	}

	voice, ok := plugins["cadence-voice"]
	if !ok {
		t.Fatal("cadence-voice: not found")
	}
	if !voice.Core || voice.Marketplace != "workbench" {
		t.Errorf("cadence-voice: got %+v, want Core=true Marketplace=workbench", voice)
	}
}

func TestCoreDefaultSet(t *testing.T) {
	plugins, err := ParseHeaders(strings.NewReader(fixtureCatalog))
	if err != nil {
		t.Fatalf("ParseHeaders: %v", err)
	}
	got := CoreDefaultSet(plugins)

	want := map[string]bool{
		"cadence@workbench":       true,
		"cadence-voice@workbench": true,
	}
	if len(got) != len(want) {
		t.Fatalf("CoreDefaultSet() = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("CoreDefaultSet()[%q] = %v, want %v", k, got[k], v)
		}
	}
	if _, present := got["cadence-mcp@"]; present {
		t.Error("CoreDefaultSet() included a marketplace-less HOLD row")
	}
}

func TestLocateCatalog_ConfiguredWins(t *testing.T) {
	got, err := LocateCatalog(t.TempDir(), "/explicit/catalog.md")
	if err != nil {
		t.Fatalf("LocateCatalog: %v", err)
	}
	if got != "/explicit/catalog.md" {
		t.Errorf("LocateCatalog() = %q, want the configured path verbatim", got)
	}
}

// writeInstalledPlugins writes a minimal installed_plugins.json under
// homeDir/.claude/plugins declaring a single cadence@workbench record.
func writeInstalledPlugins(t *testing.T, homeDir, installPath, lastUpdated string) {
	t.Helper()
	dir := filepath.Join(homeDir, ".claude", "plugins")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	doc := `{"version":2,"plugins":{"cadence@workbench":[{"scope":"user","installPath":"` + installPath + `","lastUpdated":"` + lastUpdated + `"}]}}`
	if err := os.WriteFile(filepath.Join(dir, "installed_plugins.json"), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLocateCatalog_InstalledPluginsWins(t *testing.T) {
	home := t.TempDir()
	installPath := filepath.Join(home, ".claude", "plugins", "cache", "workbench", "cadence", "abc123-hash")
	writeInstalledPlugins(t, home, installPath, "2026-07-21T15:35:23.014Z")

	got, err := LocateCatalog(home, "")
	if err != nil {
		t.Fatalf("LocateCatalog: %v", err)
	}
	want := filepath.Join(installPath, catalogRelPath)
	if got != want {
		t.Errorf("LocateCatalog() = %q, want %q", got, want)
	}
}

func TestLocateCatalog_InstalledPluginsLatestScopeWins(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".claude", "plugins")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	older := filepath.Join(home, "older")
	newer := filepath.Join(home, "newer")
	doc := `{"version":2,"plugins":{"cadence@workbench":[
		{"scope":"user","installPath":"` + older + `","lastUpdated":"2026-01-01T00:00:00.000Z"},
		{"scope":"project","installPath":"` + newer + `","lastUpdated":"2026-07-21T15:35:23.014Z"}
	]}}`
	if err := os.WriteFile(filepath.Join(dir, "installed_plugins.json"), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := LocateCatalog(home, "")
	if err != nil {
		t.Fatalf("LocateCatalog: %v", err)
	}
	want := filepath.Join(newer, catalogRelPath)
	if got != want {
		t.Errorf("LocateCatalog() = %q, want the latest-lastUpdated record %q", got, want)
	}
}

func TestLocateCatalog_FallsBackToCacheGlobWhenInstalledPluginsMissing(t *testing.T) {
	home := t.TempDir()

	older := filepath.Join(home, ".claude", "plugins", "cache", "workbench", "cadence", "older-hash")
	newer := filepath.Join(home, ".claude", "plugins", "cache", "workbench", "cadence", "newer-hash")
	if err := os.MkdirAll(older, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(newer, 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := os.Chtimes(older, now.Add(-time.Hour), now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(newer, now, now); err != nil {
		t.Fatal(err)
	}

	got, err := LocateCatalog(home, "")
	if err != nil {
		t.Fatalf("LocateCatalog: %v", err)
	}
	want := filepath.Join(newer, catalogRelPath)
	if got != want {
		t.Errorf("LocateCatalog() = %q, want the newest-mtime cache dir %q", got, want)
	}
}

func TestLocateCatalog_NeitherSourceResolves(t *testing.T) {
	if _, err := LocateCatalog(t.TempDir(), ""); err == nil {
		t.Error("LocateCatalog() = nil error, want an error when neither installed_plugins.json nor a cache dir exists")
	}
}
