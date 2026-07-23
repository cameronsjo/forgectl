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

// EffectiveMarketplaces mirrors EffectiveEnabled for extraKnownMarketplaces —
// the CURRENTLY effective registry across all three scopes, replace-not-merge.
// Read-only / reporting use only: it includes PROJECT scope, so it must NEVER
// be used to compute what --apply writes. See TrustedMarketplaces, which
// deliberately excludes project for exactly that reason, and FilterMarketplaces,
// which applies the resulting trust boundary to a target set.
func EffectiveMarketplaces(user, project, local Document) map[string]json.RawMessage {
	for _, doc := range []Document{local, project, user} {
		if doc.Present && doc.Marketplaces != nil {
			return doc.Marketplaces
		}
	}
	return map[string]json.RawMessage{}
}

// TrustedMarketplaces resolves the marketplace SOURCE registry --apply may
// draw from: local shadows user OUTRIGHT (replace, not merge — mirrors
// EffectiveEnabled), but PROJECT scope is deliberately excluded. Unlike
// enabledPlugins (which Target() folds a project's committed choices into,
// per locked decision 2), a marketplace SOURCE is the actual thing a plugin
// resolves against — folding a project-committed one in the same way would
// let a cloned malicious repo's committed .claude/settings.json register an
// attacker-controlled marketplace into the victim's local settings on a bare
// --apply. FilterMarketplaces is what applies this registry to a target set.
func TrustedMarketplaces(user, local Document) map[string]json.RawMessage {
	for _, doc := range []Document{local, user} {
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
// extraKnownMarketplaces are replaced. The read-modify-write's own directory
// creation mirrors allowlist.go's writeSettings (MkdirAll(0700)), but the
// final write goes through writeLocalAtomic (temp file + rename, mirroring
// internal/env's writeAtomic) rather than a bare os.WriteFile — this file
// commonly also holds a human's other local overrides, so a crash or a
// concurrent writer mid-write must never leave it half-written. Returns the
// path written.
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
	if err := writeLocalAtomic(path, append(data, '\n')); err != nil {
		return "", err
	}
	return path, nil
}

// writeLocalAtomic writes data to path by creating a temp file in the same
// directory (0600 from creation — os.CreateTemp's own default, no chmod
// window to close), writing, syncing, closing, then renaming over path. The
// temp file is removed on any error before that final rename. Mirrors
// internal/env's writeAtomic; simplified here since WriteLocal's caller
// doesn't need the "was the prior file's mode loosened" signal env's version
// tracks.
func writeLocalAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)

	tmp, err := os.CreateTemp(dir, ".settings.local-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp file in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}

	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return fmt.Errorf("write %s: %w", filepath.Base(path), err)
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("sync %s: %w", filepath.Base(path), err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("close %s: %w", filepath.Base(path), err)
	}

	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("rename into place %s: %w", filepath.Base(path), err)
	}
	return nil
}
