package docker

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

// cacheEntry is the on-disk shape of the last-built-tag cache
// (config.DockerLastTagPath()): the tag from the most recent successful
// `docker build`, so `run`/`shell` can omit --tag. Mirrors internal/net's
// cacheEntry.
type cacheEntry struct {
	Tag     string    `json:"tag"`
	BuiltAt time.Time `json:"builtAt"`
}

// readLastTag reads and decodes path. ok is false for a missing file, an
// unreadable file, or malformed JSON — all three are treated identically by
// the caller: no cached tag, not an error.
func readLastTag(path string) (entry cacheEntry, ok bool) {
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

// writeLastTag persists entry to path, creating the parent directory as
// needed. Callers treat a write failure as non-fatal (the build itself
// still succeeded; it just won't be remembered for the next run/shell).
func writeLastTag(path string, entry cacheEntry) error {
	if path == "" {
		return errors.New("docker: last-tag cache path unset")
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
