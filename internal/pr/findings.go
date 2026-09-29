package pr

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
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
	if !isFindingsStoreChild(findingsDir, full) {
		return false
	}
	if !sandbox.WithinWorkspace(findingsDir, full) {
		return false
	}
	return !modTime.After(cutoff)
}

// isFindingsStoreChild reports whether full names exactly one directory
// PrepareLocal could have created: a DIRECT child of findingsDir whose base
// name carries findingsDirPrefix (forgectl#558). It is lexical by design and
// runs before any filesystem call, so it refuses the store itself (full ==
// findingsDir, a trailing slash, or "x/.." all clean to it), anything nested
// deeper, anything that climbs out through "..", and any name PrepareLocal
// never mints. sandbox.WithinWorkspace alone accepts all of those except the
// climb: it answers "inside or equal", never "exactly one level down".
//
// The comparison is against the configured findingsDir as spelled, not its
// symlink-resolved form. Every caller builds full with filepath.Join over that
// same spelling, so a symlinked store still matches; a caller that resolved
// the path first is refused, which is the safe direction. Case is compared
// exactly, so on a case-insensitive volume a differently-cased spelling is
// likewise refused rather than accepted.
//
// A store that cleans to "." is refused outright (forgectl#575): New leaves
// findingsDir empty when config.PrFindingsDir fails, and filepath.Clean of "",
// "./" or "x/.." is ".", which would make every bare "forgectl-findings-*"
// name a child of the process cwd. The check is on the cleaned root, not the
// raw spelling, so every spelling of the cwd is refused. The
// base name must also be strictly longer than the prefix, because
// os.MkdirTemp always appends a random suffix, so a dir named exactly
// findingsDirPrefix is not one PrepareLocal made.
//
// The clean == root test is belt and braces: filepath.Dir(clean) == root
// already excludes the store everywhere but the filesystem root, and the
// root's base name never carries the prefix.
func isFindingsStoreChild(findingsDir, full string) bool {
	root := filepath.Clean(findingsDir)
	if root == "." {
		return false
	}
	clean := filepath.Clean(full)
	if clean == root || filepath.Dir(clean) != root {
		return false
	}
	base := filepath.Base(clean)
	return len(base) > len(findingsDirPrefix) && strings.HasPrefix(base, findingsDirPrefix)
}

// FindingsCleanup reports findings directories older than olderThan. With
// apply==false (the default posture everywhere in forgectl) it returns what
// WOULD be removed and deletes nothing. With apply==true it hands exactly that
// candidate set to FindingsRemove, so there is one delete path and it is the
// audited one.
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
func (c *Client) FindingsCleanup(ctx context.Context, olderThan time.Duration, apply bool) ([]string, error) {
	entries, err := os.ReadDir(c.findingsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read pr findings dir: %w", err)
	}
	cutoff := time.Now().Add(-olderThan)
	var candidates []string
	var unmarked int
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
		// Advisory here, outside the lock, so the preview never offers a live
		// review's dir. FindingsRemove re-asks under the lock before removing.
		if c.skipLiveFindingsDir(full, &unmarked) {
			continue
		}
		candidates = append(candidates, full)
	}
	warnUnmarkedFindings(unmarked)
	if !apply {
		return candidates, nil
	}
	return c.FindingsRemove(ctx, candidates)
}

// FindingsRemove removes exactly the given findings-dir paths — the set a
// caller already derived via FindingsCleanup(ctx, olderThan, false) and had a
// human confirm. This is the apply half of the scan-once precedent: the
// confirmed set and the deleted set must be the same set, never
// independently re-derived.
//
// Each removal is audited the way every other destructive pr verb is: it takes
// the lifecycle lock, and inside that one hold it re-validates the path, writes
// an intent row to the repair log, removes the dir, and completes the row. The
// lock is what makes the row safe to append — `pr repair --prune` rewrites the
// log by rename under the same lock, so an unlocked appender could write to a
// file that is about to be replaced and lose its row silently.
//
// The re-validation — it must be a findings dir directly under c.findingsDir
// (isFindingsStoreChild: never the store itself, never nested deeper, always
// carrying the findings prefix), exist, be a plain directory (not a symlink),
// remain contained within c.findingsDir after symlink resolution, and not be
// owned by a review whose session record still exists or carry no owner
// marker at all (findingsDirLiveness, forgectl#558) — means a
// path that never qualified, or stopped qualifying between preview and apply
// (already removed, replaced by something else), is skipped with a logged note
// rather than silently re-scanned into a different set. A skip writes no row,
// because nothing happened.
//
// Each returned path is the cleaned spelling that was actually removed, the
// same string the audit row records as RecordPath — not the caller's spelling,
// which may carry a trailing slash or dot segments.
//
// The first error stops the run and returns the paths removed so far: a busy
// lock, a cancelled ctx, an intent row that could not be written (the removal
// is refused and the dir is left in place), or a failed removal.
func (c *Client) FindingsRemove(ctx context.Context, paths []string) ([]string, error) {
	var removed []string
	var unmarked int
	defer func() { warnUnmarkedFindings(unmarked) }()
	for _, full := range paths {
		got, err := c.removeFindingsDirAudited(ctx, full, &unmarked)
		if err != nil {
			return removed, err
		}
		if got != "" {
			removed = append(removed, got)
		}
	}
	return removed, nil
}

// removeFindingsDirAudited is FindingsRemove's per-path body: one lock hold
// covering the re-checks, the intent row, the removal, and the completion row,
// in that order. It returns the cleaned path it removed (the audit row's
// RecordPath); a skipped path is ("", nil).
//
// The row mirrors teardownRowFor's shape for a subject that is not a session
// record: RecordPath names the findings dir and Detail its size, while Ref,
// Mode, FromPhase, and Workspace stay empty — filling any of them would make
// the trail claim a session was involved.
func (c *Client) removeFindingsDirAudited(ctx context.Context, full string, unmarked *int) (string, error) {
	removed := ""
	err := c.withLifecycleLock(ctx, auditVerbFindingsCleanup, func() error {
		if !isFindingsStoreChild(c.findingsDir, full) {
			slog.Warn("Skipping findings removal target that is not a findings dir directly under the store.", "path", full)
			return nil
		}
		// Every filesystem call below uses the cleaned spelling. A trailing
		// slash would make Lstat follow a symlink to its target and report a
		// plain directory, defeating the symlink check that follows.
		full = filepath.Clean(full)
		// The store is opened ONCE, and the plain-dir check and the removal
		// below both go through that handle (forgectl#644). A path-based
		// RemoveAll after an Lstat re-resolved every component at removal
		// time, so a store (or ancestor) swapped for a symlink between the
		// check and the removal redirected it outside the store. The handle
		// pins the directory that was checked, and os.Root refuses any name
		// that would climb out of it.
		root, err := os.OpenRoot(c.findingsDir)
		if err != nil {
			slog.Warn("Skipping findings removal target: the findings store cannot be opened.", "path", full, "error", err)
			return nil
		}
		defer func() { _ = root.Close() }()
		name := filepath.Base(full)
		info, err := root.Lstat(name)
		if err != nil {
			slog.Warn("Skipping findings removal target that no longer exists.", "path", full, "error", err)
			return nil
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			slog.Warn("Skipping findings removal target that is no longer a plain directory.", "path", full)
			return nil
		}
		if !sandbox.WithinWorkspace(c.findingsDir, full) {
			slog.Warn("Skipping findings removal target that escapes the findings dir.", "path", full)
			return nil
		}
		// LIVENESS (forgectl#558), asked in the same lock hold as the removal,
		// never trusted from the preview. A stale verdict cannot flip to live
		// before the RemoveAll below. PrepareLocal writes a marker only after
		// the record it names exists, so a marker never names a record that
		// is still to come. Record names carry their creation nanosecond, so
		// a record found gone is not recreated under the same name. The
		// reverse flip, a live record deleted mid-check, only keeps a dir
		// that could have gone, and teardown deletes records under this same
		// lock, so it cannot land inside this hold anyway.
		if c.skipLiveFindingsDir(full, unmarked) {
			return nil
		}
		row := RepairRow{
			Verb:       auditVerbFindingsCleanup,
			RecordPath: full,
			Detail:     fmt.Sprintf("findings dir, %d bytes", findingsDirSize(full)),
		}
		rowID, err := c.beginRepairRow(row)
		if err != nil {
			return fmt.Errorf("remove findings dir %s: %w", full, err)
		}
		rerr := findingsRemoveAll(root, name)
		c.completeRepairRow(rowID, row, rerr)
		if rerr != nil {
			slog.Error("Failed to remove findings dir.", "path", full, "error", rerr)
			return fmt.Errorf("remove findings dir %s: %w", full, rerr)
		}
		slog.Info("Reclaimed findings dir.", "path", full)
		removed = full
		return nil
	})
	if err != nil {
		return "", err
	}
	return removed, nil
}

// findingsRemoveAll removes name, a direct child of the store, through the
// store handle root: it never follows a symlink out of the store, and a
// final-component symlink is unlinked rather than followed.
//
// It is a seam so a test can make one removal fail and prove the completion
// row records it as failed — a chmod-based failure is ignored by root, which
// is how this suite runs in some containers — or swap the store between the
// checks and the removal (forgectl#644). Tests that swap it must not call
// t.Parallel.
var findingsRemoveAll = (*os.Root).RemoveAll

// findingsDirSize sums the size of every regular file under root,
// recursively — a best-effort accounting for the `pr findings list` report
// and the size detail on a `pr findings cleanup --apply` audit row.
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
