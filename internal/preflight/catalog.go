// Package preflight computes forgectl's configuration-alignment verb: the
// deterministic diff between a project's currently-enabled Claude Code
// plugins and the cadence skill catalog's core-tier default set (Cut A —
// see cameronsjo/forgectl#91 and the design doc, claude-configurations
// docs/plans/2026-07-12-preflight-bootstrap-design.md). Every function here
// is pure or takes an explicit path/homeDir argument — no ambient os.Getwd()
// or os.UserHomeDir() reads — so internal/cli/preflight.go owns environment
// resolution and this package stays table-testable.
package preflight

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// catalogRelPath is the generated catalog's location inside an installed
// cadence plugin checkout (cadence/plugins/cadence/skills/catalog/references
// in the monorepo source; the plugin cache mirrors the same plugin-relative
// layout).
const catalogRelPath = "skills/catalog/references/catalog.md"

// TierCore is the catalog section tier Cut A's alignment targets — see
// PluginInfo.
const TierCore = "core"

// PluginInfo is one plugin's resolved catalog membership: its marketplace id
// and whether ANY of its sections declared tier:core. A plugin can own
// multiple sections (cadence itself ships both a core and an on-demand
// section, split by skill) — ParseHeaders folds them to one entry per
// plugin, Core=true if any section was core.
type PluginInfo struct {
	Marketplace string
	Core        bool
}

// headerRe matches a generated catalog section header:
//
//	## <plugin> · tier: <tier> · id: <plugin>@<marketplace>
//
// The trailing "id: …" group is optional — a HOLD row carries no id (e.g.
// "## cadence-mcp · tier: hold · HOLD — pending MCP-server workload
// decision") — such a row's tier is still recorded, but with no
// Marketplace it can never form an "plugin@marketplace" enabledPlugins key,
// so CoreDefaultSet skips it.
var headerRe = regexp.MustCompile(`^## (\S+) · tier: (\S+)(?: · id: (\S+))?`)

// ParseHeaders reads a generated catalog markdown document and returns one
// PluginInfo per plugin name. Only section headers are parsed — the
// per-skill table rows aren't needed by Cut A's plugin-level alignment.
func ParseHeaders(r io.Reader) (map[string]PluginInfo, error) {
	plugins := map[string]PluginInfo{}
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		m := headerRe.FindStringSubmatch(scanner.Text())
		if m == nil {
			continue
		}
		name, tier, id := m[1], m[2], m[3]
		info := plugins[name]
		if tier == TierCore {
			info.Core = true
		}
		if id != "" {
			if _, marketplace, ok := strings.Cut(id, "@"); ok {
				info.Marketplace = marketplace
			}
		}
		plugins[name] = info
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan catalog: %w", err)
	}
	return plugins, nil
}

// ReadCatalog opens path and parses it via ParseHeaders — the composed
// "find it, then parse it" entry point CLI callers use.
func ReadCatalog(path string) (map[string]PluginInfo, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open catalog %s: %w", path, err)
	}
	defer f.Close()
	plugins, err := ParseHeaders(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return plugins, nil
}

// CoreDefaultSet returns the "plugin@marketplace" enabledPlugins keys for
// every plugin ParseHeaders found with at least one core-tier section and a
// resolvable marketplace — the catalog's deterministic default set Cut A
// aligns a project to.
func CoreDefaultSet(plugins map[string]PluginInfo) map[string]bool {
	target := map[string]bool{}
	for name, info := range plugins {
		if info.Core && info.Marketplace != "" {
			target[name+"@"+info.Marketplace] = true
		}
	}
	return target
}

// LocateCatalog resolves the on-disk path of the generated skill catalog.
// configured, when non-empty (the [preflight].catalog_path config
// override), wins outright — no existence check here, ReadCatalog surfaces
// a missing/unreadable file itself. Otherwise it reads
// ~/.claude/plugins/installed_plugins.json for the "cadence@<marketplace>"
// entry's installPath — the authoritative, race-free record Claude Code
// itself writes on every plugin pin — and falls back to a newest-mtime glob
// over ~/.claude/plugins/cache/*/cadence/* only when that file is missing,
// unreadable, or carries no cadence entry.
func LocateCatalog(homeDir, configured string) (string, error) {
	if configured != "" {
		return configured, nil
	}
	if path, ok := locateViaInstalledPlugins(homeDir); ok {
		return path, nil
	}
	return locateViaCacheGlob(homeDir)
}

// installedPluginsDoc is the subset of installed_plugins.json preflight
// reads: one or more install records per "plugin@marketplace" key (Claude
// Code allows multiple scopes to install the same plugin independently).
type installedPluginsDoc struct {
	Plugins map[string][]struct {
		InstallPath string `json:"installPath"`
		LastUpdated string `json:"lastUpdated"`
	} `json:"plugins"`
}

// locateViaInstalledPlugins looks up the cadence plugin's installPath in
// ~/.claude/plugins/installed_plugins.json, picking the record with the
// lexicographically greatest lastUpdated (an RFC3339 timestamp, so
// lexicographic order matches chronological order) when more than one scope
// installed it. ok is false for any failure — missing file, malformed JSON,
// or no "cadence@*" key — signaling the caller to fall back to the cache
// glob.
func locateViaInstalledPlugins(homeDir string) (path string, ok bool) {
	data, err := os.ReadFile(filepath.Join(homeDir, ".claude", "plugins", "installed_plugins.json"))
	if err != nil {
		return "", false
	}
	var doc installedPluginsDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return "", false
	}
	var bestPath, bestWhen string
	for key, records := range doc.Plugins {
		name, _, cut := strings.Cut(key, "@")
		if !cut || name != "cadence" {
			continue
		}
		for _, r := range records {
			if r.InstallPath == "" {
				continue
			}
			if bestPath == "" || r.LastUpdated > bestWhen {
				bestPath, bestWhen = r.InstallPath, r.LastUpdated
			}
		}
	}
	if bestPath == "" {
		return "", false
	}
	return filepath.Join(bestPath, catalogRelPath), true
}

// locateViaCacheGlob falls back to the newest-mtime directory matching
// ~/.claude/plugins/cache/*/cadence/* (marketplace wildcard) — the same
// per-SHA cache layout installed_plugins.json's installPath points into,
// used only when that file is unavailable.
func locateViaCacheGlob(homeDir string) (string, error) {
	pattern := filepath.Join(homeDir, ".claude", "plugins", "cache", "*", "cadence", "*")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return "", fmt.Errorf("glob plugin cache %s: %w", pattern, err)
	}
	var newest string
	var newestMod time.Time
	for _, m := range matches {
		fi, err := os.Stat(m)
		if err != nil || !fi.IsDir() {
			continue
		}
		if newest == "" || fi.ModTime().After(newestMod) {
			newest, newestMod = m, fi.ModTime()
		}
	}
	if newest == "" {
		return "", fmt.Errorf("no cadence plugin cache found under %s", pattern)
	}
	return filepath.Join(newest, catalogRelPath), nil
}
