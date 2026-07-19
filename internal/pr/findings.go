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

// findingsRemovalCandidate is the pure per-entry removal decision
// FindingsCleanup scans with — factored out of the ReadDir loop so it can be
// unit-tested directly with isDir forced true, proving the containment
// check actually rejects an escaping path rather than silently passing.
//
// In the real ReadDir loop, isDir comes from a DirEntry, and Go's
// DirEntry.IsDir() is FALSE for a symlink even when it targets a directory
// (it reflects the Lstat-style entry itself, never the resolved target) —
// so every top-level symlink is already excluded by the isDir==false branch
// before sandbox.WithinWorkspace ever runs. The containment check is a
// SECOND, independent line of defense (the same symlink-resolved guard
// breadcrumb.go uses before trusting a Workspace path): it is not what
// rejects today's top-level-symlink case, but it becomes load-bearing the
// moment that isDir filter is ever loosened (e.g. to follow symlinks, or if
// a future caller feeds this a Stat-following isDir instead of Lstat's).
func findingsRemovalCandidate(findingsDir, full string, isDir bool, modTime, cutoff time.Time) bool {
	if !isDir {
		return false
	}
	if !sandbox.WithinWorkspace(findingsDir, full) {
		return false
	}
	return !modTime.After(cutoff)
}

// FindingsCleanup reports findings directories older than olderThan. With
// apply==false (the default posture everywhere in forgectl) it returns what
// WOULD be removed and deletes nothing.
//
// DELETION GUARD: there is no path parameter — every removal target is
// re-derived from c.findingsDir by directory listing, and a candidate is
// removed only when findingsRemovalCandidate (above) says yes.
//
// Callers wanting the scan-once precedent (show a confirm prompt against a
// set, then remove exactly that set) should derive the removal set once
// with apply=false and hand it to FindingsRemove, rather than calling
// FindingsCleanup a second time with apply=true — a second call re-derives
// its set from a fresh ReadDir and could diverge from what was confirmed.
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
		full := filepath.Join(c.findingsDir, e.Name())
		info, err := e.Info()
		if err != nil {
			slog.Warn("Skipping findings entry with unreadable info.", "path", full, "error", err)
			continue
		}
		if !findingsRemovalCandidate(c.findingsDir, full, e.IsDir(), info.ModTime(), cutoff) {
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

// FindingsRemove removes exactly the given findings-dir paths — the set a
// caller already derived via FindingsCleanup(olderThan, false) and had a
// human confirm. This is the apply half of the scan-once precedent: the
// confirmed set and the deleted set must be the same set, never
// independently re-derived. Each path is still re-validated at removal
// time — it must exist, be a plain directory (not a symlink), and remain
// contained within c.findingsDir after symlink resolution — so a path that
// stopped qualifying between preview and apply (already removed, replaced
// by something else) is skipped with a logged note rather than silently
// re-scanned into a different set.
func (c *Client) FindingsRemove(paths []string) ([]string, error) {
	var removed []string
	for _, full := range paths {
		info, err := os.Lstat(full)
		if err != nil {
			slog.Warn("Skipping findings removal target that no longer exists.", "path", full, "error", err)
			continue
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			slog.Warn("Skipping findings removal target that is no longer a plain directory.", "path", full)
			continue
		}
		if !sandbox.WithinWorkspace(c.findingsDir, full) {
			slog.Warn("Skipping findings removal target that escapes the findings dir.", "path", full)
			continue
		}
		if err := os.RemoveAll(full); err != nil {
			slog.Error("Failed to remove findings dir.", "path", full, "error", err)
			return removed, fmt.Errorf("remove findings dir %s: %w", full, err)
		}
		slog.Info("Reclaimed findings dir.", "path", full)
		removed = append(removed, full)
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
