package pr

import (
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/cameronsjo/forgectl/internal/sandbox"
)

// FindingsEntry describes one findings directory recorded under the
// client's durable findings dir (config.PrFindingsDir) — the surviving
// deliverable of a `forgectl pr local` review, named
// "forgectl-findings-<random>".
type FindingsEntry struct {
	Path    string
	ModTime time.Time
	Size    int64
}

// FindingsList enumerates the direct children of c.findingsDir. A missing
// dir (no local review has ever run) returns (nil, nil), mirroring List's
// os.IsNotExist handling for the sessions dir.
func (c *Client) FindingsList() ([]FindingsEntry, error) {
	entries, err := os.ReadDir(c.findingsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read pr findings dir: %w", err)
	}
	var out []FindingsEntry
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		full := filepath.Join(c.findingsDir, e.Name())
		info, err := e.Info()
		if err != nil {
			slog.Warn("Skipping findings entry with unreadable info.", "path", full, "error", err)
			continue
		}
		out = append(out, FindingsEntry{
			Path:    full,
			ModTime: info.ModTime(),
			Size:    findingsDirSize(full),
		})
	}
	return out, nil
}

// FindingsCleanup reclaims findings directories older than olderThan. With
// apply==false (the default posture everywhere in forgectl) it returns what
// WOULD be removed and deletes nothing.
//
// DELETION GUARD: there is no path parameter — every removal target is
// re-derived from c.findingsDir by directory listing, and a candidate is
// removed only when ALL hold: it is a directory entry (a top-level symlink
// or plain file is skipped outright), it is contained within c.findingsDir
// after symlink resolution (sandbox.WithinWorkspace — the same
// symlink-resolved containment guard breadcrumb.go uses before trusting a
// Workspace path), and its mtime is older than the cutoff.
func (c *Client) FindingsCleanup(olderThan time.Duration, apply bool) ([]string, error) {
	entries, err := os.ReadDir(c.findingsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read pr findings dir: %w", err)
	}
	cutoff := time.Now().Add(-olderThan)
	var removed []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		full := filepath.Join(c.findingsDir, e.Name())
		if !sandbox.WithinWorkspace(c.findingsDir, full) {
			slog.Warn("Skipping findings entry that escapes the findings dir.", "path", full)
			continue
		}
		info, err := e.Info()
		if err != nil {
			slog.Warn("Skipping findings entry with unreadable info.", "path", full, "error", err)
			continue
		}
		if info.ModTime().After(cutoff) {
			continue
		}
		removed = append(removed, full)
		if !apply {
			continue
		}
		if err := os.RemoveAll(full); err != nil {
			slog.Error("Failed to remove findings dir.", "path", full, "error", err)
			return removed, fmt.Errorf("remove findings dir %s: %w", full, err)
		}
		slog.Info("Reclaimed findings dir.", "path", full)
	}
	return removed, nil
}

// findingsDirSize sums the size of every regular file under root,
// recursively — a best-effort accounting for the `pr findings list` report.
// A walk error on any individual entry is swallowed, and symlinks are
// skipped rather than counted (mirrors internal/clean's dirSize).
func findingsDirSize(root string) int64 {
	var total int64
	_ = filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d == nil || d.IsDir() || d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		total += info.Size()
		return nil
	})
	return total
}
