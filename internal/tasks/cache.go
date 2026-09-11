package tasks

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Snapshot is everything a cache holds: task, project, and label data plus
// when it was fetched. It deliberately has no field that could carry a
// credential — there is nothing here for a caller to accidentally persist.
type Snapshot struct {
	FetchedAt time.Time `json:"fetched_at"`
	Projects  []Project `json:"projects"`
	Tasks     []Task    `json:"tasks"`
	Labels    []Label   `json:"labels"`
}

// Age returns how long ago the snapshot was fetched, relative to now.
func (s Snapshot) Age(now time.Time) time.Duration { return now.Sub(s.FetchedAt) }

// SaveCache writes snap to path as JSON (0600), creating the parent
// directory (0700) if needed. Snapshot carries no token field, so there is
// no redaction to perform here — the type itself cannot hold one.
func SaveCache(path string, snap Snapshot) error {
	// termsafe:allow-raw-json persisted cache record, never command output
	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return fmt.Errorf("tasks: encode cache: %w", err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("tasks: create cache directory %s: %w", dir, err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("tasks: write cache %s: %w", path, err)
	}
	return nil
}

// LoadCache reads a previously saved Snapshot from path. A missing file is
// reported as os.ErrNotExist (test with os.IsNotExist), not wrapped, so a
// caller can distinguish "never cached" from a real read/decode failure.
func LoadCache(path string) (Snapshot, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path is caller-resolved (config.TasksCachePath), not external input
	if err != nil {
		return Snapshot{}, err
	}
	var snap Snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return Snapshot{}, fmt.Errorf("tasks: decode cache %s: %w", path, err)
	}
	return snap, nil
}
