package tasks

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/cameronsjo/forgectl/internal/termsafe"
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
//
// The file is replaced, never written in place. The cache is what a read verb
// falls back to during a network outage, and a write cut short in place — a
// crash, a full disk, a second forgectl saving at the same moment — leaves a
// truncated file that the fallback then fails to decode, during exactly the
// outage it exists for. So the snapshot is written to a temporary file in the
// same directory, synced, and renamed over the target: a rename within one
// directory is atomic, so a reader sees the old cache or the new one, never
// part of either. Any failure removes the temporary file and leaves the old
// cache as it was.
//
// The temporary file is created 0600 by os.CreateTemp, so the saved cache is
// 0600 even when the file it replaces was not.
func SaveCache(path string, snap Snapshot) error {
	// termsafe:allow-raw-json persisted cache record, never command output
	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return fmt.Errorf("tasks: encode cache: %w", err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("tasks: create cache directory %s: %w", termsafe.QuotePath(dir), termsafe.Error(err))
	}
	if err := replaceFile(path, data); err != nil {
		return fmt.Errorf("tasks: write cache %s: %w", termsafe.QuotePath(path), termsafe.Error(err))
	}
	return nil
}

// replaceFile writes data to a new 0600 file beside path and renames it over
// path. On any failure the new file is removed and path is untouched.
func replaceFile(path string, data []byte) (err error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() {
		if err != nil {
			// The rename is the only step that consumes the temporary file, so
			// on every failure it is still ours to remove. Its own error is
			// dropped: the failure being returned is the one that matters, and
			// a leftover dot-file is harmless.
			_ = tmp.Close()
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err = tmp.Write(data); err != nil {
		return err
	}
	// Sync before the rename. Without it a crash after the rename can leave
	// the new name pointing at a file whose data never reached the disk.
	if err = tmp.Sync(); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
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
		return Snapshot{}, termsafe.Categorical("tasks: decode cache "+termsafe.QuotePath(path)+": the cache file is not valid JSON", err)
	}
	return snap, nil
}
