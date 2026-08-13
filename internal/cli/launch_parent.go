package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

type configParentOps struct {
	stat    func(string) (os.FileInfo, error)
	mkdir   func(string, os.FileMode) error
	syncDir func(string) error
}

func nativeConfigParentOps() configParentOps {
	return configParentOps{stat: os.Stat, mkdir: os.Mkdir, syncDir: syncDirectory}
}

// ensureConfigParentDurable creates missing config ancestors one component at
// a time. For every successful mkdir it syncs the new directory and then its
// parent before advancing, so the next component is never built on a name
// whose crash durability is still unknown.
func ensureConfigParentDurable(configPath string, ops configParentOps) (created []string, err error) {
	parent := filepath.Clean(filepath.Dir(configPath))
	var missing []string
	for current := parent; ; current = filepath.Dir(current) {
		info, statErr := ops.stat(current)
		if statErr == nil {
			if !info.IsDir() {
				return created, fmt.Errorf("config ancestor is not a directory: %s", current)
			}
			break
		}
		if !errors.Is(statErr, fs.ErrNotExist) {
			return created, fmt.Errorf("inspect config ancestor %s: %w", current, statErr)
		}
		missing = append(missing, current)
		next := filepath.Dir(current)
		if next == current {
			return created, fmt.Errorf("no existing ancestor for config parent %s", parent)
		}
	}

	for i := len(missing) - 1; i >= 0; i-- {
		dir := missing[i]
		if mkdirErr := ops.mkdir(dir, 0o700); mkdirErr != nil {
			if !errors.Is(mkdirErr, fs.ErrExist) {
				return created, fmt.Errorf("create config directory %s: %w", dir, mkdirErr)
			}
			info, statErr := ops.stat(dir)
			if statErr != nil || !info.IsDir() {
				return created, fmt.Errorf("config directory race at %s did not produce a directory", dir)
			}
			// A racing process made the name visible, but this attempt still
			// needs its own durability proof before building the next child.
			if syncErr := ops.syncDir(dir); syncErr != nil {
				return created, fmt.Errorf("sync raced config directory %s: %w", dir, syncErr)
			}
			if syncErr := ops.syncDir(filepath.Dir(dir)); syncErr != nil {
				return created, fmt.Errorf("sync parent after raced config directory %s: %w", dir, syncErr)
			}
			continue
		}
		created = append(created, dir)
		if syncErr := ops.syncDir(dir); syncErr != nil {
			return created, fmt.Errorf("sync new config directory %s: %w", dir, syncErr)
		}
		if syncErr := ops.syncDir(filepath.Dir(dir)); syncErr != nil {
			return created, fmt.Errorf("sync parent after creating config directory %s: %w", dir, syncErr)
		}
	}
	return created, nil
}
