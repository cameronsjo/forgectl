package net

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

// cacheEntry is the on-disk shape of the reachability cache
// (config.NetCachePath()): the last probed answer plus when it was checked.
type cacheEntry struct {
	Reachable bool      `json:"reachable"`
	CheckedAt time.Time `json:"checkedAt"`
}

// readCache reads and decodes path. ok is false for a missing file, an
// unreadable file, or malformed JSON — all three are treated identically by
// the caller: a cache miss, not an error.
func readCache(path string) (entry cacheEntry, ok bool) {
	if path == "" {
		return cacheEntry{}, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return cacheEntry{}, false
	}
	if err := json.Unmarshal(data, &entry); err != nil {
		return cacheEntry{}, false
	}
	return entry, true
}

// writeCache persists entry to path, creating the parent directory as
// needed. Callers treat a write failure as non-fatal (the probe answer is
// still valid for this call; it just won't be cached for the next one).
func writeCache(path string, entry cacheEntry) error {
	if path == "" {
		return errors.New("net: cache path unset")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}
