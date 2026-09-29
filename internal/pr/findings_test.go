package pr

// Test plan for findings.go
//
// FindingsList (Classification: read-only enumeration over a forgectl-owned dir)
//   [x] Returns one entry per direct-child findings dir
//   [x] Absent findings dir returns (nil, nil), mirroring List's os.IsNotExist handling
//
// findingsRemovalCandidate (Classification: pure decision — the actual containment guard)
//   [x] Rejects a path outside findingsDir even with isDir forced true (the
//       branch a real symlink entry never reaches, since a real DirEntry's
//       IsDir() is already false for a symlink — this is the direct proof
//       the containment check itself works, independent of that filter)
//   [x] Accepts a contained, old-enough directory
//
// FindingsCleanup (Classification: deletion guard — no path parameter)
//   [x] Only removes dirs older than the cutoff
//   [x] Dry-run (apply=false) reports what would be removed and deletes nothing
//   [x] A top-level symlink is skipped, never removed (via the isDir filter —
//       see findingsRemovalCandidate's doc comment for why containment is a
//       second, currently-unreached line of defense for this exact case)
//   [x] A plain file at the top level is ignored, never removed
//
// FindingsRemove (Classification: TOCTOU-safe apply over an explicit, already-confirmed set;
// destructive verb, audited under the lifecycle lock)
//   [x] Removes exactly the given paths
//   [x] A path that stopped qualifying since the confirm (already gone) is
//       skipped, not treated as an error
//   [x] Writes the intent-then-completion pair, sharing one ID, verb
//       findings-cleanup, RecordPath = the dir, Mode empty
//   [x] A skipped path (already gone, or a symlink) writes no row — the
//       re-checks run before the intent
//   [x] The removal happens inside the lock hold, not merely the row appends
//   [x] A busy lock refuses with *lockBusyError: the dir survives, no row
//       (findings_lock_unix_test.go — lockClient and lockBusyError are
//       unix-only, like the lock itself)
//   [x] An intent row that cannot be written refuses: the dir survives and
//       the error is returned
//   [x] A removal that fails completes its row as failed (not applied), is
//       not reported removed, and returns the error (findingsRemoveAll seam —
//       a chmod failure is invisible to root)
//   [x] Stop-on-first-error: [ok, fail, ok] returns only the first path, the
//       third dir survives with no row
//   [x] A ctx cancelled while another holder has the lock returns the ctx
//       error with no row and the dir intact (findings_lock_unix_test.go)
//   [x] The #558 probe — a nested path, the store with a trailing slash, and
//       the bare store — removes nothing, leaves the store intact, and writes
//       no row; so do "." / ".." spellings and a nested prefixed dir
//   [x] A direct child without the findings prefix is skipped with no row
//   [x] A symlink passed with a trailing slash is not followed: the link and
//       its in-store target both survive, no row
//   [x] A store reached through a symlink still reclaims its own children
//       (the shape check compares the configured spelling, not the resolved one)
//   [x] An empty store (WithFindingsDir("")) never removes a cwd-relative
//       findings dir, and writes no row (#575)
//   [x] The returned path is the cleaned spelling removed, equal to the audit
//       row's RecordPath, even when the caller passed a trailing slash (#575)
//
// isFindingsStoreChild (Classification: pure lexical shape check, #558)
//   [x] Accepts only a prefixed direct child; refuses the store, trailing
//       slash and dot spellings of it, deeper nesting, a ".." climb to a
//       sibling, an unprefixed name, a differently-cased prefix, and a name
//       that is the bare prefix with no MkdirTemp suffix (#575)
//   [x] Refuses everything under an empty store, which would otherwise clean
//       to "." and adopt the cwd (#575)
//
// FindingsCleanup preview
//   [x] An unprefixed dir in the store is never offered as a candidate, so
//       the preview and the apply-time re-check agree
//
// Tests that swap the findingsRemoveAll seam (failRemovalOf) must not call
// t.Parallel.
//
// Every client here carries its own sessions dir (findingsClient), so no test
// appends to the real ~/.config/forgectl/pr-sessions audit log.

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// findingsClient is a client over dir with a private sessions dir, so the
// audit log and lifecycle lock an --apply path touches live in the test's own
// temp tree.
func findingsClient(t *testing.T, dir string) *Client {
	t.Helper()
	return New(nil, WithFindingsDir(dir), WithSessionsDir(t.TempDir()))
}

func TestFindingsList_ReturnsCreatedDirs(t *testing.T) {
	dir := t.TempDir()
	c := findingsClient(t, dir)

	mustMkdir(t, filepath.Join(dir, findingsDirPrefix+"aaa"))
	mustMkdir(t, filepath.Join(dir, findingsDirPrefix+"bbb"))

	entries, err := c.FindingsList()
	if err != nil {
		t.Fatalf("FindingsList: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("FindingsList returned %d entries, want 2", len(entries))
	}
}

func TestFindingsList_EmptyOnAbsentDir(t *testing.T) {
	c := findingsClient(t, filepath.Join(t.TempDir(), "does-not-exist"))

	entries, err := c.FindingsList()
	if err != nil {
		t.Fatalf("FindingsList: %v", err)
	}
	if entries != nil {
		t.Errorf("FindingsList on an absent dir = %v, want nil", entries)
	}
}

func TestFindingsCleanup_RemovesOnlyOldDirsUnderApply(t *testing.T) {
	dir := t.TempDir()
	c := findingsClient(t, dir)

	oldDir := filepath.Join(dir, findingsDirPrefix+"old")
	newDir := filepath.Join(dir, findingsDirPrefix+"new")
	mustMkdir(t, oldDir)
	mustMkdir(t, newDir)

	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(oldDir, old, old); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	removed, err := c.FindingsCleanup(t.Context(), 24*time.Hour, true)
	if err != nil {
		t.Fatalf("FindingsCleanup: %v", err)
	}
	if len(removed) != 1 || removed[0] != oldDir {
		t.Fatalf("FindingsCleanup removed %v, want only %q", removed, oldDir)
	}
	if _, err := os.Stat(oldDir); !os.IsNotExist(err) {
		t.Errorf("oldDir %q still exists after apply", oldDir)
	}
	if _, err := os.Stat(newDir); err != nil {
		t.Errorf("newDir %q was removed, want it left alone: %v", newDir, err)
	}
}

func TestFindingsCleanup_DryRunDeletesNothing(t *testing.T) {
	dir := t.TempDir()
	c := findingsClient(t, dir)

	oldDir := filepath.Join(dir, findingsDirPrefix+"old")
	mustMkdir(t, oldDir)
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(oldDir, old, old); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	removed, err := c.FindingsCleanup(t.Context(), 24*time.Hour, false)
	if err != nil {
		t.Fatalf("FindingsCleanup: %v", err)
	}
	if len(removed) != 1 || removed[0] != oldDir {
		t.Fatalf("FindingsCleanup dry-run reported %v, want only %q", removed, oldDir)
	}
	if _, err := os.Stat(oldDir); err != nil {
		t.Errorf("dry-run deleted %q, want it left alone: %v", oldDir, err)
	}
}

func TestFindingsCleanup_SkipsTopLevelSymlink(t *testing.T) {
	// This proves the observed BEHAVIOR (a symlink at the top level is never
	// removed) — the MECHANISM is the isDir filter in findingsRemovalCandidate,
	// not the containment check: a real os.DirEntry's IsDir() is false for a
	// symlink even when it targets a directory, so this symlink never reaches
	// sandbox.WithinWorkspace at all. TestFindingsRemovalCandidate_* below
	// exercises the containment check directly, with isDir forced true, since
	// this test structurally can't.
	dir := t.TempDir()
	outside := t.TempDir()
	c := findingsClient(t, dir)

	link := filepath.Join(dir, findingsDirPrefix+"escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	old := time.Now().Add(-48 * time.Hour)
	// Lutimes isn't portable via os.Chtimes (it follows the symlink), so age
	// the link's target instead — if the isDir filter were broken this would
	// still be old enough to match the cutoff.
	if err := os.Chtimes(outside, old, old); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	removed, err := c.FindingsCleanup(t.Context(), 24*time.Hour, true)
	if err != nil {
		t.Fatalf("FindingsCleanup: %v", err)
	}
	if len(removed) != 0 {
		t.Errorf("FindingsCleanup removed %v, want the top-level symlink skipped", removed)
	}
	if _, err := os.Lstat(link); err != nil {
		t.Errorf("symlink %q was removed, want it left alone: %v", link, err)
	}
}

func TestFindingsRemovalCandidate_RejectsPathOutsideFindingsDir(t *testing.T) {
	findingsDir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "elsewhere")
	old := time.Now().Add(-48 * time.Hour)
	cutoff := time.Now().Add(-24 * time.Hour)

	// isDir forced true: this is the case a real symlink entry never
	// produces (DirEntry.IsDir() is false for a symlink), so this is the
	// only way to exercise the containment branch directly and prove it
	// independently rejects an escaping path rather than silently passing.
	if got := findingsRemovalCandidate(findingsDir, outside, true, old, cutoff); got {
		t.Errorf("findingsRemovalCandidate(%q, %q, isDir=true, ...) = true, want false (outside findingsDir)", findingsDir, outside)
	}
}

func TestFindingsRemovalCandidate_AcceptsOldContainedDir(t *testing.T) {
	findingsDir := t.TempDir()
	contained := filepath.Join(findingsDir, findingsDirPrefix+"old")
	// sandbox.WithinWorkspace resolves EvalSymlinks on both sides; findingsDir
	// (a real, existing t.TempDir()) resolves through macOS's /var ->
	// /private/var symlink, so contained must exist too, or the two sides
	// resolve asymmetrically and a genuinely-contained path reads as escaping.
	mustMkdir(t, contained)
	old := time.Now().Add(-48 * time.Hour)
	cutoff := time.Now().Add(-24 * time.Hour)

	if got := findingsRemovalCandidate(findingsDir, contained, true, old, cutoff); !got {
		t.Errorf("findingsRemovalCandidate(%q, %q, isDir=true, old, ...) = false, want true (contained + old enough)", findingsDir, contained)
	}
}

func TestFindingsCleanup_IgnoresPlainFile(t *testing.T) {
	dir := t.TempDir()
	c := findingsClient(t, dir)

	file := filepath.Join(dir, findingsDirPrefix+"stray.txt")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(file, old, old); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	removed, err := c.FindingsCleanup(t.Context(), 24*time.Hour, true)
	if err != nil {
		t.Fatalf("FindingsCleanup: %v", err)
	}
	if len(removed) != 0 {
		t.Errorf("FindingsCleanup removed %v, want the plain file ignored", removed)
	}
	if _, err := os.Stat(file); err != nil {
		t.Errorf("file %q was removed, want it left alone: %v", file, err)
	}
}

func TestFindingsRemove_RemovesGivenPaths(t *testing.T) {
	dir := t.TempDir()
	c := findingsClient(t, dir)

	a := filepath.Join(dir, findingsDirPrefix+"a")
	b := filepath.Join(dir, findingsDirPrefix+"b")
	mustMkdir(t, a)
	mustMkdir(t, b)

	removed, err := c.FindingsRemove(t.Context(), []string{a, b})
	if err != nil {
		t.Fatalf("FindingsRemove: %v", err)
	}
	if len(removed) != 2 {
		t.Fatalf("FindingsRemove removed %v, want both paths", removed)
	}
	for _, p := range []string{a, b} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%q still exists after FindingsRemove", p)
		}
	}
}

func TestFindingsRemove_SkipsPathThatNoLongerQualifies(t *testing.T) {
	// Simulates the TOCTOU gap between a preview scan and the confirmed
	// apply: one path was already removed out-of-band before FindingsRemove
	// runs. It must be skipped with a note, not treated as an error, and
	// must not cause the other (still-valid) path to be re-scanned or
	// dropped.
	dir := t.TempDir()
	c := findingsClient(t, dir)

	gone := filepath.Join(dir, findingsDirPrefix+"gone")
	stillHere := filepath.Join(dir, findingsDirPrefix+"still-here")
	mustMkdir(t, stillHere)
	// gone is never created — stands in for "removed between preview and apply".

	removed, err := c.FindingsRemove(t.Context(), []string{gone, stillHere})
	if err != nil {
		t.Fatalf("FindingsRemove: %v", err)
	}
	if len(removed) != 1 || removed[0] != stillHere {
		t.Fatalf("FindingsRemove removed %v, want only %q", removed, stillHere)
	}
	if _, err := os.Stat(stillHere); !os.IsNotExist(err) {
		t.Errorf("%q still exists, want it removed", stillHere)
	}
}

func TestFindingsRemove_WritesIntentAndCompletion(t *testing.T) {
	dir := t.TempDir()
	c := findingsClient(t, dir)
	target := filepath.Join(dir, findingsDirPrefix+"audited")
	mustMkdir(t, target)
	if err := os.WriteFile(filepath.Join(target, "findings.md"), []byte("12345"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := c.FindingsRemove(t.Context(), []string{target}); err != nil {
		t.Fatalf("FindingsRemove: %v", err)
	}
	rows := auditRows(t, c)
	if len(rows) != 2 {
		t.Fatalf("audit rows = %d, want intent + completion: %+v", len(rows), rows)
	}
	if rows[0].ID == "" || rows[0].ID != rows[1].ID {
		t.Errorf("row IDs = %q, %q, want one shared non-empty ID", rows[0].ID, rows[1].ID)
	}
	if rows[0].Outcome != repairOutcomeIntent || rows[1].Outcome != repairOutcomeApplied {
		t.Errorf("outcomes = %q, %q, want intent then applied", rows[0].Outcome, rows[1].Outcome)
	}
	for i, r := range rows {
		if r.Verb != auditVerbFindingsCleanup {
			t.Errorf("rows[%d].Verb = %q, want %q", i, r.Verb, auditVerbFindingsCleanup)
		}
		if r.RecordPath != target {
			t.Errorf("rows[%d].RecordPath = %q, want %q", i, r.RecordPath, target)
		}
		if r.Mode != "" || r.Ref != "" || r.FromPhase != "" || r.Workspace != "" {
			t.Errorf("rows[%d] claims a session (mode=%q ref=%q phase=%q ws=%q), want all empty",
				i, r.Mode, r.Ref, r.FromPhase, r.Workspace)
		}
		if r.Detail != "findings dir, 5 bytes" {
			t.Errorf("rows[%d].Detail = %q, want the dir's size", i, r.Detail)
		}
	}
}

func TestFindingsRemove_SkippedPathWritesNoRow(t *testing.T) {
	dir := t.TempDir()
	c := findingsClient(t, dir)
	gone := filepath.Join(dir, findingsDirPrefix+"gone")
	link := filepath.Join(dir, findingsDirPrefix+"link")
	if err := os.Symlink(t.TempDir(), link); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	removed, err := c.FindingsRemove(t.Context(), []string{gone, link})
	if err != nil {
		t.Fatalf("FindingsRemove: %v", err)
	}
	if len(removed) != 0 {
		t.Fatalf("FindingsRemove removed %v, want both skipped", removed)
	}
	if rows := auditRows(t, c); len(rows) != 0 {
		t.Errorf("audit rows = %+v, want none for a skipped path", rows)
	}
	if _, err := os.Lstat(link); err != nil {
		t.Errorf("symlink %q was removed, want it left alone: %v", link, err)
	}
}

func TestFindingsRemove_RemovesInsideLockHold(t *testing.T) {
	dir := t.TempDir()
	c := findingsClient(t, dir)
	target := filepath.Join(dir, findingsDirPrefix+"held")
	mustMkdir(t, target)
	var events []string
	c.onLock = func(verb, event string) {
		_, statErr := os.Stat(target)
		events = append(events, event+":"+verb+":exists="+strconv.FormatBool(statErr == nil))
	}

	if _, err := c.FindingsRemove(t.Context(), []string{target}); err != nil {
		t.Fatalf("FindingsRemove: %v", err)
	}
	want := []string{
		"acquire:" + auditVerbFindingsCleanup + ":exists=true",
		"release:" + auditVerbFindingsCleanup + ":exists=false",
	}
	if len(events) != len(want) {
		t.Fatalf("lock events = %v, want exactly one hold %v", events, want)
	}
	for i := range want {
		if events[i] != want[i] {
			t.Errorf("lock events = %v, want %v (the removal must fall inside one hold)", events, want)
			break
		}
	}
}

func TestFindingsRemove_IntentWriteFailureRefuses(t *testing.T) {
	dir := t.TempDir()
	c := findingsClient(t, dir)
	target := filepath.Join(dir, findingsDirPrefix+"unlogged")
	mustMkdir(t, target)
	// A directory where the log should be: OpenFile(O_RDWR) fails, so the
	// intent cannot be written.
	mustMkdir(t, c.repairLogPath())

	removed, err := c.FindingsRemove(t.Context(), []string{target})
	if err == nil {
		t.Fatal("FindingsRemove succeeded with an unwritable audit log, want a refusal")
	}
	if len(removed) != 0 {
		t.Errorf("removed = %v, want nothing without an intent row", removed)
	}
	if _, serr := os.Stat(target); serr != nil {
		t.Errorf("dir %q was removed with no intent row written: %v", target, serr)
	}
}

func TestFindingsRemove_RemoveFailureCompletesAsFailed(t *testing.T) {
	dir := t.TempDir()
	c := findingsClient(t, dir)
	target := filepath.Join(dir, findingsDirPrefix+"stuck")
	mustMkdir(t, target)
	failRemovalOf(t, target)

	removed, err := c.FindingsRemove(t.Context(), []string{target})
	if err == nil {
		t.Fatal("FindingsRemove succeeded though the removal failed, want the error")
	}
	if len(removed) != 0 {
		t.Errorf("removed = %v, want a failed removal not reported as removed", removed)
	}
	rows := auditRows(t, c)
	if len(rows) != 2 {
		t.Fatalf("audit rows = %d, want intent + completion: %+v", len(rows), rows)
	}
	if rows[0].ID == "" || rows[0].ID != rows[1].ID {
		t.Errorf("row IDs = %q, %q, want one shared non-empty ID", rows[0].ID, rows[1].ID)
	}
	if rows[1].Outcome != repairOutcomeFailed || rows[1].Error == "" {
		t.Errorf("completion = outcome %q error %q, want %q with the cause", rows[1].Outcome, rows[1].Error, repairOutcomeFailed)
	}
	if _, serr := os.Stat(target); serr != nil {
		t.Errorf("dir %q is gone, but the seam refused its removal: %v", target, serr)
	}
}

func TestFindingsRemove_StopsAtFirstErrorReturningRemovedSoFar(t *testing.T) {
	dir := t.TempDir()
	c := findingsClient(t, dir)
	first := filepath.Join(dir, findingsDirPrefix+"1-ok")
	second := filepath.Join(dir, findingsDirPrefix+"2-fail")
	third := filepath.Join(dir, findingsDirPrefix+"3-untouched")
	for _, p := range []string{first, second, third} {
		mustMkdir(t, p)
	}
	failRemovalOf(t, second)

	removed, err := c.FindingsRemove(t.Context(), []string{first, second, third})
	if err == nil {
		t.Fatal("FindingsRemove returned no error, want the second path's failure")
	}
	if len(removed) != 1 || removed[0] != first {
		t.Errorf("removed = %v, want only %q", removed, first)
	}
	if _, serr := os.Stat(third); serr != nil {
		t.Errorf("%q was touched after the run stopped: %v", third, serr)
	}
	rows := auditRows(t, c)
	if len(rows) != 4 {
		t.Fatalf("audit rows = %d, want two pairs (first applied, second failed): %+v", len(rows), rows)
	}
	if rows[1].Outcome != repairOutcomeApplied || rows[1].RecordPath != first {
		t.Errorf("first completion = %q for %q, want applied for %q", rows[1].Outcome, rows[1].RecordPath, first)
	}
	if rows[3].Outcome != repairOutcomeFailed || rows[3].RecordPath != second {
		t.Errorf("second completion = %q for %q, want failed for %q", rows[3].Outcome, rows[3].RecordPath, second)
	}
}

// failRemovalOf makes findingsRemoveAll refuse exactly path and remove
// everything else normally, restoring the seam when the test ends.
func failRemovalOf(t *testing.T, path string) {
	t.Helper()
	orig := findingsRemoveAll
	t.Cleanup(func() { findingsRemoveAll = orig })
	findingsRemoveAll = func(p string) error {
		if p == path {
			return &os.PathError{Op: "unlinkat", Path: p, Err: syscall.EBUSY}
		}
		return orig(p)
	}
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatalf("MkdirAll(%q): %v", path, err)
	}
}

// TestFindingsRemove_RefusesStoreRootAndNestedPaths replays the #558 probe:
// sandbox.WithinWorkspace accepts rel == "." and any depth, so before the
// shape check this call removed store/a/b and then the whole store.
func TestFindingsRemove_RefusesStoreRootAndNestedPaths(t *testing.T) {
	store := t.TempDir()
	c := findingsClient(t, store)
	sep := string(filepath.Separator)
	nested := filepath.Join(store, "a", "b")
	mustMkdir(t, nested)
	keep := filepath.Join(store, findingsDirPrefix+"keep")
	mustMkdir(t, filepath.Join(keep, findingsDirPrefix+"inner"))

	probe := []string{
		store + sep + "a" + sep + "b",
		store + sep,
		store,
		store + sep + ".",
		keep + sep + "..",
		keep + sep + findingsDirPrefix + "inner",
	}
	removed, err := c.FindingsRemove(t.Context(), probe)
	if err != nil {
		t.Fatalf("FindingsRemove: %v", err)
	}
	if len(removed) != 0 {
		t.Errorf("FindingsRemove removed %v, want every probe path refused", removed)
	}
	for _, p := range []string{nested, filepath.Join(keep, findingsDirPrefix+"inner")} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%q is gone, want the store left intact: %v", p, err)
		}
	}
	if rows := auditRows(t, c); len(rows) != 0 {
		t.Errorf("audit rows = %+v, want none for a refused path", rows)
	}
}

func TestFindingsRemove_SkipsUnprefixedChild(t *testing.T) {
	store := t.TempDir()
	c := findingsClient(t, store)
	notes := filepath.Join(store, "notes")
	mustMkdir(t, notes)

	removed, err := c.FindingsRemove(t.Context(), []string{notes})
	if err != nil {
		t.Fatalf("FindingsRemove: %v", err)
	}
	if len(removed) != 0 {
		t.Errorf("FindingsRemove removed %v, want the unprefixed dir skipped", removed)
	}
	if _, err := os.Stat(notes); err != nil {
		t.Errorf("%q is gone, want it left alone: %v", notes, err)
	}
	if rows := auditRows(t, c); len(rows) != 0 {
		t.Errorf("audit rows = %+v, want none for a skipped path", rows)
	}
}

// TestFindingsRemove_TrailingSlashSymlinkIsNotFollowed pins the Clean before
// Lstat: with a trailing slash, lstat(2) resolves the link and reports its
// target as a plain directory, so the symlink check would pass. The target is
// inside the store, so containment cannot catch it either.
func TestFindingsRemove_TrailingSlashSymlinkIsNotFollowed(t *testing.T) {
	store := t.TempDir()
	c := findingsClient(t, store)
	target := filepath.Join(store, findingsDirPrefix+"real")
	mustMkdir(t, target)
	if err := os.WriteFile(filepath.Join(target, "findings.md"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(store, findingsDirPrefix+"link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("Symlink: %v", err)
	}

	removed, err := c.FindingsRemove(t.Context(), []string{link + string(filepath.Separator)})
	if err != nil {
		t.Fatalf("FindingsRemove: %v", err)
	}
	if len(removed) != 0 {
		t.Errorf("FindingsRemove removed %v, want the symlink skipped", removed)
	}
	if _, err := os.Lstat(link); err != nil {
		t.Errorf("symlink %q was removed, want it left alone: %v", link, err)
	}
	if _, err := os.Stat(filepath.Join(target, "findings.md")); err != nil {
		t.Errorf("the link's target lost its contents, want it untouched: %v", err)
	}
	if rows := auditRows(t, c); len(rows) != 0 {
		t.Errorf("audit rows = %+v, want none for a skipped path", rows)
	}
}

func TestIsFindingsStoreChild(t *testing.T) {
	sep := string(filepath.Separator)
	store := filepath.Join(t.TempDir(), "store")
	child := filepath.Join(store, findingsDirPrefix+"abc")
	sibling := filepath.Join(filepath.Dir(store), findingsDirPrefix+"abc")
	cases := []struct {
		name string
		full string
		want bool
	}{
		{"prefixed direct child", child, true},
		{"prefixed child with trailing slash", child + sep, true},
		{"store", store, false},
		{"store with trailing slash", store + sep, false},
		{"store dot", store + sep + ".", false},
		{"child dot-dot", child + sep + "..", false},
		{"nested prefixed", filepath.Join(child, findingsDirPrefix+"x"), false},
		{"nested unprefixed", store + sep + "a" + sep + "b", false},
		{"dot-dot climb to a sibling", store + sep + ".." + sep + findingsDirPrefix + "abc", false},
		{"sibling of the store", sibling, false},
		{"unprefixed child", filepath.Join(store, "notes"), false},
		{"differently cased prefix", filepath.Join(store, strings.ToUpper(findingsDirPrefix)+"abc"), false},
		{"bare prefix only", filepath.Join(store, findingsDirPrefix), false},
	}
	// Filesystem root as the store, the one spelling where filepath.Dir(x) ==
	// x. The root's base name never carries the prefix, so the prefix check
	// refuses this as well as the explicit equality check does.
	if root := filepath.VolumeName(store) + sep; isFindingsStoreChild(root, root) {
		t.Errorf("isFindingsStoreChild(%q, %q) = true, want the root store refused", root, root)
	}
	for _, tc := range cases {
		if got := isFindingsStoreChild(store, tc.full); got != tc.want {
			t.Errorf("%s: isFindingsStoreChild(%q, %q) = %v, want %v", tc.name, store, tc.full, got, tc.want)
		}
		if got := isFindingsStoreChild(store+sep, tc.full); got != tc.want {
			t.Errorf("%s: with a trailing-slash store, isFindingsStoreChild(%q) = %v, want %v", tc.name, tc.full, got, tc.want)
		}
	}
}

// TestIsFindingsStoreChild_EmptyStoreRefused pins the empty-store refusal at
// the predicate: filepath.Clean("") is ".", so without it a bare prefixed name
// reads as a direct child of the cwd.
func TestIsFindingsStoreChild_EmptyStoreRefused(t *testing.T) {
	sep := string(filepath.Separator)
	for _, store := range []string{"", ".", "." + sep, "x" + sep + ".."} {
		for _, full := range []string{
			findingsDirPrefix + "x",
			"." + sep + findingsDirPrefix + "x",
		} {
			if isFindingsStoreChild(store, full) {
				t.Errorf("isFindingsStoreChild(%q, %q) = true, want a store that cleans to %q refused", store, full, ".")
			}
		}
	}
}

// TestFindingsRemove_EmptyStoreNeverTouchesCwd replays the #575 probe: New
// leaves findingsDir empty when config.PrFindingsDir fails, and before the
// refusal FindingsRemove took an empty store to mean the cwd and removed
// ./forgectl-findings-cwd. Not parallel: t.Chdir changes process state.
func TestFindingsRemove_EmptyStoreNeverTouchesCwd(t *testing.T) {
	cwd := t.TempDir()
	t.Chdir(cwd)
	name := findingsDirPrefix + "cwd"
	victim := filepath.Join(cwd, name)
	if err := os.Mkdir(victim, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(victim, "findings.md"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := findingsClient(t, "")

	removed, err := c.FindingsRemove(t.Context(), []string{name, "." + string(filepath.Separator) + name})
	if err != nil {
		t.Fatalf("FindingsRemove: %v", err)
	}
	if len(removed) != 0 {
		t.Errorf("FindingsRemove removed %v, want nothing under an empty store", removed)
	}
	if _, err := os.Stat(filepath.Join(victim, "findings.md")); err != nil {
		t.Errorf("cwd-relative %q lost its contents, want it untouched: %v", victim, err)
	}
	if rows := auditRows(t, c); len(rows) != 0 {
		t.Errorf("audit rows = %+v, want none for a refused path", rows)
	}

	got, err := c.FindingsCleanup(t.Context(), 0, true)
	if err != nil {
		t.Fatalf("FindingsCleanup: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("FindingsCleanup under an empty store = %v, want nothing", got)
	}
	if _, err := os.Stat(victim); err != nil {
		t.Errorf("cwd-relative %q was removed by cleanup: %v", victim, err)
	}
}

// TestFindingsRemove_ReturnsCleanedPath pins that the returned path is the one
// the audit row names: a caller passing a trailing slash gets back the cleaned
// spelling that was actually removed, not its own.
func TestFindingsRemove_ReturnsCleanedPath(t *testing.T) {
	store := t.TempDir()
	c := findingsClient(t, store)
	want := filepath.Join(store, findingsDirPrefix+"x")
	mustMkdir(t, want)

	removed, err := c.FindingsRemove(t.Context(), []string{want + string(filepath.Separator)})
	if err != nil {
		t.Fatalf("FindingsRemove: %v", err)
	}
	if len(removed) != 1 || removed[0] != want {
		t.Fatalf("FindingsRemove returned %q, want [%q]", removed, want)
	}
	if _, err := os.Stat(want); !os.IsNotExist(err) {
		t.Errorf("%q survived (err=%v), want it removed", want, err)
	}
	rows := auditRows(t, c)
	if len(rows) == 0 {
		t.Fatal("no audit rows, want an intent/completion pair")
	}
	for _, r := range rows {
		if r.RecordPath != removed[0] {
			t.Errorf("audit row RecordPath = %q, want it to match the returned %q", r.RecordPath, removed[0])
		}
	}
}

func TestFindingsCleanup_PreviewSkipsUnprefixedDir(t *testing.T) {
	store := t.TempDir()
	c := findingsClient(t, store)
	notes := filepath.Join(store, "notes")
	mustMkdir(t, notes)
	ours := filepath.Join(store, findingsDirPrefix+"ours")
	mustMkdir(t, ours)

	got, err := c.FindingsCleanup(t.Context(), 0, false)
	if err != nil {
		t.Fatalf("FindingsCleanup: %v", err)
	}
	if len(got) != 1 || got[0] != ours {
		t.Errorf("FindingsCleanup preview = %v, want only %q", got, ours)
	}
}

func TestFindingsCleanup_SymlinkedStoreStillReclaims(t *testing.T) {
	realDir := t.TempDir()
	store := filepath.Join(t.TempDir(), "store-link")
	if err := os.Symlink(realDir, store); err != nil {
		t.Skipf("Symlink: %v", err)
	}
	c := findingsClient(t, store)
	target := filepath.Join(realDir, findingsDirPrefix+"old")
	mustMkdir(t, target)

	removed, err := c.FindingsCleanup(t.Context(), 0, true)
	if err != nil {
		t.Fatalf("FindingsCleanup: %v", err)
	}
	want := filepath.Join(store, findingsDirPrefix+"old")
	if len(removed) != 1 || removed[0] != want {
		t.Errorf("FindingsCleanup removed %v, want [%q]", removed, want)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Errorf("%q survived (err=%v), want it reclaimed through the symlinked store", target, err)
	}
	if _, err := os.Stat(realDir); err != nil {
		t.Errorf("the store's real dir is gone, want it intact: %v", err)
	}
}
