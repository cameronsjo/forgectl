package sessions

import (
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// The Syncthing-blobs-only guard, enforced on the steady-state path: Syncthing
// carries only document blobs (the vault, the runbook markdown corpus) —
// NEVER the append-only JSONL ledgers. Three machines appending to a synced
// JSONL produce silent .sync-conflict-* divergence, corrupting the WAL the
// whole ETL rests on. Every `sessions sync` run re-checks before touching the
// mart, so a misconfigured share fails the sync loudly instead of slowly
// poisoning the ledger.
//
// Failure posture (resiliency rule): the CONDITION fails closed — a folder
// covering a JSONL root aborts the sync. The guard's own faults fail open —
// a missing or unparseable config yields a warning, never a blocked sync.

// syncthingConfig is the slice of Syncthing's config.xml the guard reads:
// top-level <folder> elements only. The <defaults> template folder is a
// different XML path, so it is structurally excluded here — no special-casing.
type syncthingConfig struct {
	Folders []struct {
		Path string `xml:"path,attr"`
	} `xml:"folder"`
}

// DefaultSyncthingConfigPath returns the platform's Syncthing config.xml
// location, or "" when none exists (nothing syncs on this machine).
func DefaultSyncthingConfigPath(home string) string {
	candidates := []string{
		filepath.Join(home, ".config", "syncthing", "config.xml"),
	}
	if runtime.GOOS == "darwin" {
		candidates = []string{
			filepath.Join(home, "Library", "Application Support", "Syncthing", "config.xml"),
			filepath.Join(home, ".config", "syncthing", "config.xml"),
		}
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return ""
}

// CheckSyncthingFolders parses the config and reports every configured folder
// that covers a forbidden JSONL root — at, above, or below it (an ancestor
// share syncs the JSONL just as surely). Pure given the file's bytes.
func CheckSyncthingFolders(configPath, home string) (violations []string, err error) {
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("read syncthing config %s: %w", configPath, err)
	}
	var cfg syncthingConfig
	if err := xml.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("parse syncthing config %s: %w", configPath, err)
	}
	forbidden := []string{
		filepath.Join(home, ".claude", "metrics"),
		filepath.Join(home, ".claude", "cadence", "sessions"),
	}
	for _, f := range cfg.Folders {
		folder := f.Path
		if strings.HasPrefix(folder, "~") {
			folder = filepath.Join(home, strings.TrimPrefix(folder, "~"))
		}
		folder = filepath.Clean(folder)
		for _, root := range forbidden {
			if folder == root ||
				strings.HasPrefix(root+string(filepath.Separator), folder+string(filepath.Separator)) ||
				strings.HasPrefix(folder+string(filepath.Separator), root+string(filepath.Separator)) {
				violations = append(violations,
					fmt.Sprintf("syncthing folder %q covers JSONL root %q", f.Path, root))
			}
		}
	}
	return violations, nil
}
