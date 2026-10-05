package config

import "path/filepath"

// DocsServersDir returns the directory holding one immutable discovery record
// per running `forgectl docs serve` generation:
// <os.UserConfigDir()>/forgectl/docs-servers (macOS: ~/Library/Application
// Support/forgectl/docs-servers; Linux: ~/.config/forgectl/docs-servers).
//
// A DIRECTORY of generation-named records replaces the single shared
// docs-server.json (DocsServerPath, now a read-only legacy fallback) because one
// shared pathname cannot be owned. Two overlapping servers both wrote it, and
// whichever stopped first deleted the other's discoverability — forgectl#277.
// Each server now publishes under a name only it will ever use and removes only
// that name, so cleanup cannot reach a sibling's record.
//
// This function only computes the path. Creating the directory is the publisher's
// job, because creation carries the ownership and permission checks that make the
// records safe to read, and those belong beside the code that reads them.
func DocsServersDir() (string, error) {
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "docs-servers"), nil
}

// SurfaceReadyPredicatesPath is the optional override for the readiness
// predicates `forgectl surface ready` uses: <os.UserConfigDir()>/forgectl/
// surface-ready.toml. It lives in forgectl's own config directory, never a
// repo or worktree, so a branch cannot mark its own trust dialog as ready.
// This function only computes the path; the file need not exist.
func SurfaceReadyPredicatesPath() (string, error) {
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "surface-ready.toml"), nil
}
