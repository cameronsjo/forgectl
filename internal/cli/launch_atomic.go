package cli

import (
	"fmt"
	"os"
	"path/filepath"
)

// writeConfigAtomic writes data to config.toml at path by creating a temp
// file in the same directory, syncing, closing, then renaming over path —
// the write either lands whole or not at all, so a process killed mid-write
// can never leave config.toml truncated or empty with no recovery copy.
// Shared by appendLaunchSection (launch init's scaffold write, and
// writeImportedLaunchSection's fallback-scenario import) and
// replaceLaunchSection (the shadow-scenario rewrite) — the two write paths
// security review flagged for having no atomic write and no backup of the
// file being rewritten. Mirrors internal/preflight's writeLocalAtomic
// (itself mirroring internal/env's writeAtomic), simplified the same way:
// no "was the prior file's mode loosened" signal to track.
//
// os.CreateTemp defaults new files to 0600; the temp file is chmod'd to
// 0644 before the rename so this atomic rewrite doesn't silently narrow
// config.toml's permissions as a side effect — that's a separate, explicitly
// out-of-scope question (config.toml's file mode vs the legacy file's own
// mode) tracked as a follow-up, not something this write mechanics change
// should quietly decide.
func writeConfigAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}

	tmp, err := os.CreateTemp(dir, ".config-*.toml.tmp")
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
	if err := os.Chmod(tmpPath, 0o644); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("set permissions on %s: %w", filepath.Base(path), err)
	}

	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("rename %s into place: %w", filepath.Base(path), err)
	}
	return nil
}
