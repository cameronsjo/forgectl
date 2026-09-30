package pr

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
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
	store, err := os.OpenRoot(c.findingsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read pr findings dir: %w", err)
	}
	defer func() { _ = store.Close() }()
	entries, err := fs.ReadDir(store.FS(), ".")
	if err != nil {
		return nil, fmt.Errorf("read pr findings dir: %w", err)
	}
	var out []FindingsEntry
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		full := filepath.Join(c.findingsDir, e.Name())
		// Through the store handle: DirEntry.Info is an lstat by path.
		info, err := store.Lstat(e.Name())
		if err != nil {
			slog.Warn("Skipping findings entry with unreadable info.", "path", full, "error", err)
			continue
		}
		out = append(out, FindingsEntry{
			Path:    full,
			ModTime: info.ModTime(),
			Size:    findingsChildSize(store, e.Name()),
		})
	}
	return out, nil
}

// openFindingsStore opens the findings store once, as the handle every
// cleanup step below goes through. os.OpenRoot resolves c.findingsDir in the
// ordinary way, so a symlinked store still opens (and reclaims) its target;
// what the handle pins is the directory that resolution reached.
//
// Before the handle is returned it is checked to be private to this user
// (verifyFindingsStore, forgectl#680); a store that is not is refused with
// errFindingsStoreUnsafe. A missing store is an error errors.Is matches to
// fs.ErrNotExist.
func (c *Client) openFindingsStore() (*os.Root, error) {
	store, err := os.OpenRoot(c.findingsDir)
	if err != nil {
		return nil, fmt.Errorf("open pr findings store: %w", err)
	}
	if err := verifyFindingsStore(store); err != nil {
		_ = store.Close()
		return nil, err
	}
	return store, nil
}

// errFindingsStoreUnsafe is openFindingsStore's refusal of a store that is
// not private to this user. Its text never names the store's path.
var errFindingsStoreUnsafe = errors.New("refusing to clean up the pr findings store: it is not private to you")

// errFindingsChildMoved is openFindingsChild's refusal of a name that no
// longer resolves to the directory the caller checked.
var errFindingsChildMoved = errors.New("findings dir changed between the check and the open")

// openFindingsChild opens name, a direct child of store, as its own handle,
// and proves it is the same directory as checked, the Lstat result the
// caller already judged a plain directory (forgectl#685). Everything cleanup
// then reads about the dir, its marker and its size, goes through that
// handle, and so does the emptying of its contents at removal
// (removeJudgedFindingsDir). Only the final rmdir goes by name, and rmdir
// cannot take a directory that still holds anything, so the handle is what
// binds the removal to the dir that was judged.
//
// store.OpenRoot follows a symlink that stays inside the store, so a name
// swapped for one after the caller's Lstat would open some other findings
// dir. The SameFile check against checked refuses that: the handle is either
// the checked directory or nothing.
func openFindingsChild(store *os.Root, name string, checked fs.FileInfo) (*os.Root, error) {
	child, err := store.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	got, err := child.Stat(".")
	if err == nil && !os.SameFile(checked, got) {
		err = errFindingsChildMoved
	}
	if err != nil {
		_ = child.Close()
		return nil, err
	}
	return child, nil
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
//
// The store is opened once (openFindingsStore), and every read below goes
// through that handle. A store that cannot be opened stops the run with one
// error (forgectl#685); only a missing one is the empty result.
func (c *Client) FindingsCleanup(ctx context.Context, olderThan time.Duration, apply bool) ([]string, error) {
	store, err := c.openFindingsStore()
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer func() { _ = store.Close() }()
	entries, err := fs.ReadDir(store.FS(), ".")
	if err != nil {
		return nil, fmt.Errorf("read pr findings dir: %w", err)
	}
	cutoff := time.Now().Add(-olderThan)
	var candidates []string
	var unmarked int
	for _, e := range entries {
		full := filepath.Join(c.findingsDir, e.Name())
		// Through the store handle: DirEntry.Info is an lstat by path.
		info, err := store.Lstat(e.Name())
		if err != nil {
			slog.Warn("Skipping findings entry with unreadable info.", "path", full, "error", err)
			continue
		}
		if !findingsRemovalCandidate(c.findingsDir, full, e.IsDir(), info.ModTime(), cutoff) {
			continue
		}
		// Advisory here, outside the lock, so the preview never offers a live
		// review's dir. FindingsRemove re-asks under the lock before removing.
		if c.skipFindingsChild(store, e.Name(), info, full, &unmarked) {
			continue
		}
		candidates = append(candidates, full)
	}
	warnUnmarkedFindings(unmarked)
	if !apply {
		return candidates, nil
	}
	return c.findingsRemoveFrom(ctx, store, candidates)
}

// skipFindingsChild is the preview's liveness ask for the store child name:
// it opens the child as its own handle and asks skipLiveFindingsDir through
// it. A child that cannot be opened as the directory the scan listed cannot be
// classified, so it is kept.
func (c *Client) skipFindingsChild(store *os.Root, name string, info fs.FileInfo, full string, unmarked *int) bool {
	child, err := openFindingsChild(store, name, info)
	if err != nil {
		slog.Warn("Skipping findings dir that cannot be opened through the store.", "path", full, "error", err)
		return true
	}
	defer func() { _ = child.Close() }()
	return c.skipLiveFindingsDir(child, full, unmarked)
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
// open through the store handle as that same directory, and not be
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
// A missing store removes nothing and is not an error. The first error stops
// the run and returns the paths removed so far: a store that exists but cannot
// be opened (reported once, before any path, forgectl#685), a busy
// lock, a cancelled ctx, an intent row that could not be written (the removal
// is refused and the dir is left in place), or a failed removal.
func (c *Client) FindingsRemove(ctx context.Context, paths []string) ([]string, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	store, err := c.openFindingsStore()
	if errors.Is(err, fs.ErrNotExist) {
		// Nothing under a store that is not there, the same answer
		// FindingsCleanup gives; one line, not one per path.
		slog.Warn("The findings store does not exist; nothing to remove.", "count", len(paths))
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = store.Close() }()
	return c.findingsRemoveFrom(ctx, store, paths)
}

// findingsRemoveFrom is FindingsRemove's loop over an already opened store
// handle. FindingsCleanup's apply half calls it with the handle its scan used,
// so the scan and the removal go through one open of the store.
func (c *Client) findingsRemoveFrom(ctx context.Context, store *os.Root, paths []string) ([]string, error) {
	var removed []string
	var unmarked int
	defer func() { warnUnmarkedFindings(unmarked) }()
	for _, full := range paths {
		got, err := c.removeFindingsDirAudited(ctx, store, full, &unmarked)
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
func (c *Client) removeFindingsDirAudited(ctx context.Context, store *os.Root, full string, unmarked *int) (string, error) {
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
		// The store is opened ONCE, by the caller, and the plain-dir check,
		// the marker read, the size, and the emptying of the dir below all go
		// through that handle or the child handle opened from it
		// (forgectl#644, forgectl#685). A path-based RemoveAll after an Lstat
		// re-resolved every component at removal time, so a store (or
		// ancestor) swapped for a symlink between the check and the removal
		// redirected it outside the store. The handle pins the directory that
		// was checked, and os.Root refuses any name that would climb out of
		// it, which is also why no path-based containment check
		// (sandbox.WithinWorkspace) runs here any more: it would resolve the
		// path at a different moment from the one the handle was opened at.
		//
		// The name inside the store is not pinned: another dir can be renamed
		// onto it at any moment. So the removal never deletes BY NAME what it
		// has not emptied through the child handle; see
		// removeJudgedFindingsDir.
		name := filepath.Base(full)
		info, err := store.Lstat(name)
		if err != nil {
			slog.Warn("Skipping findings removal target that no longer exists.", "path", full, "error", err)
			return nil
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			slog.Warn("Skipping findings removal target that is no longer a plain directory.", "path", full)
			return nil
		}
		child, err := openFindingsChild(store, name, info)
		if err != nil {
			slog.Warn("Skipping findings removal target that cannot be opened through the store.", "path", full, "error", err)
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
		defer func() { _ = child.Close() }()
		if c.skipLiveFindingsDir(child, full, unmarked) {
			return nil
		}
		size := findingsDirSize(child)
		row := RepairRow{
			Verb:       auditVerbFindingsCleanup,
			RecordPath: full,
			Detail:     fmt.Sprintf("findings dir, %d bytes", size),
		}
		rowID, err := c.beginRepairRow(row)
		if err != nil {
			return fmt.Errorf("remove findings dir %s: %w", full, err)
		}
		rerr := findingsRemoveAll(store, child, name, info)
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

// findingsRemoveAll is the removal of a judged findings dir,
// removeJudgedFindingsDir.
//
// It is a seam so a test can make one removal fail and prove the completion
// row records it as failed — a chmod-based failure is ignored by root, which
// is how this suite runs in some containers — or swap the store, or the dir
// at name, between the checks and the removal (forgectl#644, forgectl#685).
// Tests that swap it must not call t.Parallel.
var findingsRemoveAll = removeJudgedFindingsDir

// errFindingsDirSwapped is the removal's refusal of a name that no longer
// holds the dir that was judged, or of a judged dir that gained an entry
// after it was emptied.
var errFindingsDirSwapped = errors.New("findings dir changed after it was judged; left in place")

// removeJudgedFindingsDir removes the findings dir that was judged: child is
// the handle on it, name its entry in store, and judged the Lstat the caller
// checked it against.
//
// A removal by name alone (store.RemoveAll(name)) deletes whatever sits at
// name when it runs, and another dir, a live review's included, can be
// renamed onto name after the verdict. So the contents are removed through
// child, which reaches only the judged dir wherever it now sits. Only then
// is name removed, with rmdir semantics, and only while it still names the
// judged dir: a dir swapped onto name holds at least its own owner marker,
// so rmdir refuses it (ENOTEMPTY), and the SameFile check refuses a swapped
// empty dir or symlink before that. What a swap in the final gap can cost is
// one empty directory. Both refusals are errFindingsDirSwapped.
//
// os.Root never follows a symlink out of child, and removes a symlink inside
// it rather than following it. child is closed before the rmdir, because an
// open handle on a directory blocks its deletion on Windows.
func removeJudgedFindingsDir(store, child *os.Root, name string, judged fs.FileInfo) error {
	entries, err := fs.ReadDir(child.FS(), ".")
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := child.RemoveAll(e.Name()); err != nil {
			return err
		}
	}
	_ = child.Close()
	got, err := store.Lstat(name)
	if err != nil {
		return err
	}
	if !os.SameFile(judged, got) {
		return errFindingsDirSwapped
	}
	if err := store.Remove(name); err != nil {
		if errors.Is(err, syscall.ENOTEMPTY) || errors.Is(err, syscall.EEXIST) {
			return errFindingsDirSwapped
		}
		return err
	}
	return nil
}

// findingsChildSize is findingsDirSize for the store child name, for the
// `pr findings list` report; a child that cannot be opened counts as 0.
func findingsChildSize(store *os.Root, name string) int64 {
	child, err := store.OpenRoot(name)
	if err != nil {
		return 0
	}
	defer func() { _ = child.Close() }()
	return findingsDirSize(child)
}

// findingsDirSize sums the size of every regular file under dir,
// recursively — a best-effort accounting for the `pr findings list` report
// and the size detail on a `pr findings cleanup --apply` audit row. It walks
// through the handle, so it counts the directory that handle pinned
// (forgectl#685). A walk error on any individual entry is swallowed, and
// symlinks are skipped rather than counted (mirrors internal/clean's dirSize).
func findingsDirSize(dir *os.Root) int64 {
	var total int64
	_ = fs.WalkDir(dir.FS(), ".", func(_ string, d fs.DirEntry, err error) error {
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
