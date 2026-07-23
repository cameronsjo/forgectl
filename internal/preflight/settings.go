package preflight

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// UserPath, ProjectPath, and LocalPath resolve the three on-disk settings
// locations preflight reads, given the user's home dir and the project
// root — in ascending precedence per Claude Code's own resolution order. A
// higher scope REPLACES, never merges, enabledPlugins (locked design
// decision 1/2, claude-configurations
// docs/plans/2026-07-12-preflight-bootstrap-design.md): a present
// enabledPlugins at a higher scope is the WHOLE answer for that scope, not
// folded with a lower one.
func UserPath(homeDir string) string { return filepath.Join(homeDir, ".claude", "settings.json") }
func ProjectPath(projectDir string) string {
	return filepath.Join(projectDir, ".claude", "settings.json")
}
func LocalPath(projectDir string) string {
	return filepath.Join(projectDir, ".claude", "settings.local.json")
}

// Document is the slice of a settings.json/settings.local.json file
// preflight cares about: the enabled-plugin map and the marketplace
// registry. Present distinguishes "the file exists but declares neither
// key" from "the file doesn't exist at all" — replace-not-merge only lets a
// higher scope shadow a lower one when the higher scope is genuinely
// present on disk, so a missing settings.local.json must never be read as
// "everything disabled".
type Document struct {
	Present        bool
	EnabledPlugins map[string]bool
	Marketplaces   map[string]json.RawMessage
}

// ReadDocument reads and decodes path. A missing file is not an error — it
// yields a Present:false zero Document.
func ReadDocument(path string) (Document, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Document{}, nil
		}
		return Document{}, fmt.Errorf("read %s: %w", path, err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return Document{}, fmt.Errorf("parse %s: %w", path, err)
	}
	doc := Document{Present: true}
	if v, ok := raw["enabledPlugins"]; ok {
		if err := json.Unmarshal(v, &doc.EnabledPlugins); err != nil {
			return Document{}, fmt.Errorf("parse %s enabledPlugins: %w", path, err)
		}
	}
	if v, ok := raw["extraKnownMarketplaces"]; ok {
		if err := json.Unmarshal(v, &doc.Marketplaces); err != nil {
			return Document{}, fmt.Errorf("parse %s extraKnownMarketplaces: %w", path, err)
		}
	}
	return doc, nil
}

// EffectiveEnabled resolves the CURRENT enabled-plugin set across the three
// scopes under replace-not-merge precedence: local shadows project shadows
// user OUTRIGHT (the first present scope with a declared enabledPlugins key
// is the entire answer). Returns an empty, non-nil map when no scope
// declares one.
func EffectiveEnabled(user, project, local Document) map[string]bool {
	for _, doc := range []Document{local, project, user} {
		if doc.Present && doc.EnabledPlugins != nil {
			return doc.EnabledPlugins
		}
	}
	return map[string]bool{}
}

// EffectiveMarketplaces mirrors EffectiveEnabled for extraKnownMarketplaces.
func EffectiveMarketplaces(user, project, local Document) map[string]json.RawMessage {
	for _, doc := range []Document{local, project, user} {
		if doc.Present && doc.Marketplaces != nil {
			return doc.Marketplaces
		}
	}
	return map[string]json.RawMessage{}
}

// WriteLocal writes enabled and marketplaces into projectDir's
// .claude/settings.local.json as a read-modify-write: every other top-level
// key already on disk (permissions, hooks, a human's other local overrides,
// …) is preserved untouched — only enabledPlugins and
// extraKnownMarketplaces are replaced. Mirrors the
// MkdirAll(0700)/MarshalIndent/WriteFile(0600) idiom internal/pr's
// writeSettings uses for its own .claude/ write (allowlist.go), but as a
// merge-preserving RMW rather than a whole-file overwrite — preflight's
// local settings file is a shared, personal override surface it does not
// own exclusively. Returns the path written.
func WriteLocal(projectDir string, enabled map[string]bool, marketplaces map[string]json.RawMessage) (string, error) {
	dir := filepath.Join(projectDir, ".claude")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create %s: %w", dir, err)
	}
	path := LocalPath(projectDir)

	raw := map[string]json.RawMessage{}
	if existing, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(existing, &raw); err != nil {
			return "", fmt.Errorf("parse existing %s: %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("read %s: %w", path, err)
	}

	if enabled == nil {
		enabled = map[string]bool{}
	}
	if marketplaces == nil {
		marketplaces = map[string]json.RawMessage{}
	}

	enabledJSON, err := json.Marshal(enabled)
	if err != nil {
		return "", fmt.Errorf("marshal enabledPlugins: %w", err)
	}
	raw["enabledPlugins"] = enabledJSON

	marketplacesJSON, err := json.Marshal(marketplaces)
	if err != nil {
		return "", fmt.Errorf("marshal extraKnownMarketplaces: %w", err)
	}
	raw["extraKnownMarketplaces"] = marketplacesJSON

	data, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal %s: %w", path, err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		return "", fmt.Errorf("write %s: %w", path, err)
	}
	return path, nil
}
