package docs

// Test plan for index.go
//
// NewIndex (Classification: ops layer — filesystem walk)
//   [x] Happy: finds .md/.markdown files, skips other extensions
//   [x] Happy: skips .git/node_modules/vendor and other dot-directories
//   [x] Happy: two roots with the same base name get disambiguated labels
//   [x] Happy: docs are ordered most-recently-modified first
//   [x] Unhappy: a nonexistent root directory is a hard error
//
// A file (not a directory) path is also a valid NewIndex argument — the
// single-file-root behavior (indexFileRoot) has its own test plan in
// index_file_root_test.go.
//
// scanDoc title rule (Classification: helper)
//   [x] Happy: extracts the first "# " heading
//   [x] Happy: falls back to the filename when no heading is present
//
// Index.Resolve (Classification: security gate — root-label + traversal chain)
//   [x] Happy: a real doc resolves
//   [x] Unhappy: an unknown root label is rejected
//   [x] Unhappy: a traversal attempt against a known root is rejected
//   [x] Unhappy: a disallowed extension under a known root is rejected
//   [x] Unhappy: a file that exists on disk but lives under an excluded dir
//       (.trash, node_modules, vendor, .git, any dot-dir) is not servable by
//       a direct URL, even though the traversal chain alone would resolve it

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestNewIndex_FindsMarkdownSkipsOther(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.md"), "# A")
	writeFile(t, filepath.Join(dir, "b.markdown"), "# B")
	writeFile(t, filepath.Join(dir, "c.txt"), "not markdown")
	writeFile(t, filepath.Join(dir, "sub", "d.md"), "# D")

	idx, err := NewIndex([]string{dir})
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	docs := idx.List()
	if len(docs) != 3 {
		t.Fatalf("List() has %d docs, want 3: %+v", len(docs), docs)
	}
	rels := map[string]bool{}
	for _, d := range docs {
		rels[d.RelPath] = true
	}
	for _, want := range []string{"a.md", "b.markdown", "sub/d.md"} {
		if !rels[want] {
			t.Errorf("missing indexed doc %q, got %v", want, rels)
		}
	}
	if rels["c.txt"] {
		t.Error("c.txt was indexed, want it skipped (disallowed extension)")
	}
}

func TestNewIndex_SkipsHiddenAndVendorDirs(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ".git", "COMMIT_EDITMSG.md"), "# hidden")
	writeFile(t, filepath.Join(dir, "node_modules", "pkg", "readme.md"), "# vendored")
	writeFile(t, filepath.Join(dir, "vendor", "readme.md"), "# vendored")
	writeFile(t, filepath.Join(dir, ".hidden", "readme.md"), "# dotdir")
	writeFile(t, filepath.Join(dir, "kept.md"), "# kept")

	idx, err := NewIndex([]string{dir})
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	docs := idx.List()
	if len(docs) != 1 || docs[0].RelPath != "kept.md" {
		t.Errorf("List() = %+v, want exactly [kept.md]", docs)
	}
}

func TestNewIndex_DuplicateBaseNames_Disambiguated(t *testing.T) {
	base := t.TempDir()
	dirA := filepath.Join(base, "group-a", "docs")
	dirB := filepath.Join(base, "group-b", "docs")
	writeFile(t, filepath.Join(dirA, "a.md"), "# A")
	writeFile(t, filepath.Join(dirB, "b.md"), "# B")

	idx, err := NewIndex([]string{dirA, dirB})
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	roots := idx.Roots()
	if len(roots) != 2 {
		t.Fatalf("Roots() has %d entries, want 2", len(roots))
	}
	if roots[0].Label == roots[1].Label {
		t.Errorf("both roots share label %q, want disambiguated labels", roots[0].Label)
	}
}

func TestNewIndex_OrdersMostRecentFirst(t *testing.T) {
	dir := t.TempDir()
	oldPath := filepath.Join(dir, "old.md")
	newPath := filepath.Join(dir, "new.md")
	writeFile(t, oldPath, "# old")
	writeFile(t, newPath, "# new")

	oldTime := time.Now().Add(-1 * time.Hour)
	newTime := time.Now()
	if err := os.Chtimes(oldPath, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(newPath, newTime, newTime); err != nil {
		t.Fatal(err)
	}

	idx, err := NewIndex([]string{dir})
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	docs := idx.List()
	if len(docs) != 2 {
		t.Fatalf("List() has %d docs, want 2", len(docs))
	}
	if docs[0].RelPath != "new.md" {
		t.Errorf("docs[0] = %q, want new.md first (most-recently-modified)", docs[0].RelPath)
	}
}

func TestNewIndex_NonexistentRoot_Errors(t *testing.T) {
	if _, err := NewIndex([]string{filepath.Join(t.TempDir(), "missing")}); err == nil {
		t.Error("NewIndex on a nonexistent root: got nil error, want one")
	}
}

func TestScanDoc_Title_ExtractsHeading(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "page.md")
	writeFile(t, path, "\n\n# The Real Title\n\nbody text\n")

	meta, err := scanDoc(path, "page.md")
	if err != nil {
		t.Fatalf("scanDoc: %v", err)
	}
	if meta.Title != "The Real Title" {
		t.Errorf("Title = %q, want %q", meta.Title, "The Real Title")
	}
}

func TestScanDoc_Title_FallsBackToFilename(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "no-heading.md")
	writeFile(t, path, "just a paragraph, no heading\n")

	meta, err := scanDoc(path, "no-heading.md")
	if err != nil {
		t.Fatalf("scanDoc: %v", err)
	}
	if meta.Title != "no-heading" {
		t.Errorf("Title = %q, want %q", meta.Title, "no-heading")
	}
}

func TestIndex_Resolve_RealDoc(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "page.md"), "# Page")
	idx, err := NewIndex([]string{dir})
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	label := idx.Roots()[0].Label

	if _, err := idx.Resolve(label, "page.md"); err != nil {
		t.Errorf("Resolve(%q, %q): %v", label, "page.md", err)
	}
}

func TestIndex_Resolve_UnknownRootLabel_Rejected(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "page.md"), "# Page")
	idx, err := NewIndex([]string{dir})
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}

	_, err = idx.Resolve("no-such-root", "page.md")
	if !errors.Is(err, ErrRootNotFound) {
		t.Errorf("Resolve with unknown root: err = %v, want ErrRootNotFound", err)
	}
}

func TestIndex_Resolve_Traversal_Rejected(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "page.md"), "# Page")
	idx, err := NewIndex([]string{dir})
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	label := idx.Roots()[0].Label

	// The ../ run is clamped at the root, so this names <root>/etc/passwd,
	// which does not exist: ErrNotFound, never the real /etc/passwd.
	got, err := idx.Resolve(label, "../../../../etc/passwd")
	if !errors.Is(err, ErrNotFound) || got != "" {
		t.Errorf("Resolve traversal = %q, %v, want \"\", ErrNotFound", got, err)
	}
}

// TestIndex_Resolve_ExcludedDir_NotServableByDirectURL is the regression
// test for the "walkRoot's exclusions are UI-only" finding: a markdown file
// that walkRoot deliberately skips (hidden dot-dir, node_modules, vendor,
// .git) must ALSO be unreachable through Resolve by a direct, correctly-
// spelled request — not merely absent from the sidenav. Each file here
// genuinely exists on disk (unlike the traversal tests, which target
// nonexistent paths) so the assertion pins the exact failure mode
// (ErrNotIndexed, from the pathIndex membership check) rather than
// accidentally passing via ErrOutsideRoot/a stat failure.
func TestIndex_Resolve_ExcludedDir_NotServableByDirectURL(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "readme.md"), "# Kept")

	cases := []string{
		".trash/deleted-secret.md",
		"node_modules/dep/readme.md",
		"vendor/pkg/readme.md",
		".git/COMMIT_EDITMSG.md",
		".hidden/note.md",
	}
	for _, rel := range cases {
		writeFile(t, filepath.Join(dir, filepath.FromSlash(rel)), "# Should never be servable")
	}

	idx, err := NewIndex([]string{dir})
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	label := idx.Roots()[0].Label

	// Confidence check: the walk really did skip these — List() proves the
	// UI-hiding half of the contract still works.
	if len(idx.List()) != 1 {
		t.Fatalf("List() = %+v, want exactly [readme.md] (excluded dirs must not be indexed)", idx.List())
	}

	for _, rel := range cases {
		t.Run(rel, func(t *testing.T) {
			_, err := idx.Resolve(label, rel)
			if !errors.Is(err, ErrNotIndexed) {
				t.Errorf("Resolve(%q, %q): err = %v, want ErrNotIndexed — an excluded-dir file that exists on disk must still 404, not be served", label, rel, err)
			}
		})
	}
}

// Overlapping-root variant of the test above, and the reason pathIndex is
// keyed by root rather than by path alone. Naming a normally-excluded
// directory as a root of its own is legitimate consent to index it — but that
// consent is scoped to THAT root's URL namespace. If membership were a single
// global set of absolute paths, indexing the child would also make the file
// reachable through the PARENT root's URL, where the sidenav still (correctly)
// hides it — reopening the excluded-directory leak by configuration rather
// than by code.
func TestIndex_Resolve_ExcludedDirIndexedAsItsOwnRoot_NotServableViaParentRoot(t *testing.T) {
	parent := t.TempDir()
	writeFile(t, filepath.Join(parent, "readme.md"), "# Kept")
	trash := filepath.Join(parent, ".trash")
	writeFile(t, filepath.Join(trash, "deleted-secret.md"), "# Should never be servable via the parent")

	// Both the parent AND the excluded child are configured roots.
	idx, err := NewIndex([]string{parent, trash})
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	roots := idx.Roots()
	if len(roots) != 2 {
		t.Fatalf("Roots() = %+v, want 2 roots", roots)
	}
	parentLabel, trashLabel := roots[0].Label, roots[1].Label

	// Naming .trash explicitly does grant access through ITS OWN root — that is
	// the consent half of the contract, and it must keep working.
	if _, err := idx.Resolve(trashLabel, "deleted-secret.md"); err != nil {
		t.Errorf("Resolve(%q, %q) = %v, want nil — a directory named explicitly as a root is indexed under that root", trashLabel, "deleted-secret.md", err)
	}

	// But it must NOT be reachable through the parent root's namespace, where
	// the walk deliberately skipped it.
	if _, err := idx.Resolve(parentLabel, ".trash/deleted-secret.md"); !errors.Is(err, ErrNotIndexed) {
		t.Errorf("Resolve(%q, %q): err = %v, want ErrNotIndexed — indexing a child root must not make its files servable through the parent root's URL", parentLabel, ".trash/deleted-secret.md", err)
	}
}

func TestIndex_Resolve_DisallowedExtension_Rejected(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "page.md"), "# Page")
	writeFile(t, filepath.Join(dir, "secret.env"), "API_KEY=xyz")
	idx, err := NewIndex([]string{dir})
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	label := idx.Roots()[0].Label

	_, err = idx.Resolve(label, "secret.env")
	if !errors.Is(err, ErrDisallowedExt) {
		t.Errorf("Resolve disallowed ext: err = %v, want ErrDisallowedExt", err)
	}
}

// Test plan for walk tolerance (#568)
//   [x] Unhappy: an unreadable subdirectory is skipped with a warning; siblings still index
//   [x] Unhappy: an unreadable root itself still fails the build
//   Not covered: a file vanishing between readdir and stat (d.Info error) —
//   a race with no deterministic trigger; the branch is a two-line skip.

func TestNewIndex_UnreadableSubdir_SkippedSiblingsIndexed(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory modes")
	}
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "ok.md"), "# Ok\n")
	locked := filepath.Join(dir, "locked")
	if err := os.Mkdir(locked, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(locked, "hidden.md"), "# Hidden\n")
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })

	idx, err := NewIndex([]string{dir})
	if err != nil {
		t.Fatalf("NewIndex must survive an unreadable subdirectory: %v", err)
	}
	if got := len(idx.List()); got != 1 {
		t.Errorf("indexed %d docs, want 1 (ok.md only)", got)
	}
}

func TestNewIndex_UnreadableRoot_StillErrors(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory modes")
	}
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "ok.md"), "# Ok\n")
	if err := os.Chmod(dir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	if _, err := NewIndex([]string{dir}); err == nil {
		t.Fatal("an unreadable root must fail the build")
	}
}

// Test plan for the skip ledger and walk seam (#568)
//   [x] Unhappy: an injected subdirectory error and a vanished entry are
//       recorded on the Index and the siblings still index (runs as root)
//   [x] Unhappy: an injected error on the root itself still fails the build
//   [x] Unhappy: a d.Info() failure on an existing file is recorded, not fatal
//   [x] Happy: a clean walk records nothing
//   [x] Unhappy: Check reports the skipped paths in CheckReport.Skipped

// stubEntry is a fs.DirEntry with scriptable Info, for the walk seam.
type stubEntry struct {
	name    string
	dir     bool
	infoErr error
}

func (e stubEntry) Name() string { return e.name }
func (e stubEntry) IsDir() bool  { return e.dir }
func (e stubEntry) Type() fs.FileMode {
	if e.dir {
		return fs.ModeDir
	}
	return 0
}
func (e stubEntry) Info() (fs.FileInfo, error) { return nil, e.infoErr }

func withWalk(t *testing.T, w func(root string, fn fs.WalkDirFunc) error) {
	t.Helper()
	prev := walkDir
	walkDir = w
	t.Cleanup(func() { walkDir = prev })
}

func TestNewIndex_InjectedWalkErrors_RecordedAndSiblingsIndexed(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "ok.md"), "# Ok\n")
	withWalk(t, func(root string, fn fs.WalkDirFunc) error {
		if err := filepath.WalkDir(root, fn); err != nil {
			return err
		}
		locked := filepath.Join(root, "locked")
		_ = fn(locked, stubEntry{name: "locked", dir: true}, &fs.PathError{Op: "open", Path: locked, Err: fs.ErrPermission})
		// A file the walk listed that is gone by the time EvalSymlinks runs.
		_ = fn(filepath.Join(root, "gone.md"), stubEntry{name: "gone.md"}, nil)
		return nil
	})

	idx, err := NewIndex([]string{dir})
	if err != nil {
		t.Fatalf("NewIndex must survive skipped paths: %v", err)
	}
	if got := len(idx.List()); got != 1 {
		t.Errorf("indexed %d docs, want 1", got)
	}
	got := idx.Skipped()
	if len(got) != 2 || got[0].Rel != "locked" || got[1].Rel != "gone.md" {
		t.Fatalf("Skipped() = %+v, want locked then gone.md", got)
	}
	if got[0].Root != idx.Roots()[0].Label || got[0].Reason == "" {
		t.Errorf("skipped entry lacks root label or reason: %+v", got[0])
	}
	// Reasons are the bare cause: the absolute path in the *fs.PathError
	// (and in EvalSymlinks' lstat error for gone.md) must not leak into them.
	if got[0].Reason != fs.ErrPermission.Error() {
		t.Errorf("locked reason = %q, want %q", got[0].Reason, fs.ErrPermission.Error())
	}
	if strings.Contains(got[1].Reason, "/") {
		t.Errorf("gone.md reason = %q, want no path", got[1].Reason)
	}
}

func TestNewIndex_InjectedRootError_StillFatal(t *testing.T) {
	dir := t.TempDir()
	withWalk(t, func(root string, fn fs.WalkDirFunc) error {
		return fn(root, nil, fs.ErrPermission)
	})
	if _, err := NewIndex([]string{dir}); err == nil {
		t.Fatal("an error on the root itself must fail the build")
	}
}

func TestNewIndex_InfoFailure_RecordedNotFatal(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "real.md"), "# Real\n")
	withWalk(t, func(root string, fn fs.WalkDirFunc) error {
		return fn(filepath.Join(root, "real.md"), stubEntry{name: "real.md", infoErr: fs.ErrNotExist}, nil)
	})
	idx, err := NewIndex([]string{dir})
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	if len(idx.Skipped()) != 1 || len(idx.List()) != 0 {
		t.Errorf("skipped=%+v docs=%d, want one skip and no docs", idx.Skipped(), len(idx.List()))
	}
}

func TestNewIndex_CleanWalk_RecordsNothing(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "ok.md"), "# Ok\n")
	idx, err := NewIndex([]string{dir})
	if err != nil {
		t.Fatal(err)
	}
	if len(idx.Skipped()) != 0 {
		t.Errorf("Skipped() = %+v, want none", idx.Skipped())
	}
}

func TestCheck_ReportsSkippedPaths(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "ok.md"), "# Ok\n")
	withWalk(t, func(root string, fn fs.WalkDirFunc) error {
		if err := filepath.WalkDir(root, fn); err != nil {
			return err
		}
		_ = fn(filepath.Join(root, "locked"), stubEntry{name: "locked", dir: true}, fs.ErrPermission)
		return nil
	})
	idx, err := NewIndex([]string{dir})
	if err != nil {
		t.Fatal(err)
	}
	rep := idx.Check()
	if len(rep.Skipped) != 1 || rep.Skipped[0].Rel != "locked" {
		t.Errorf("CheckReport.Skipped = %+v, want [locked]", rep.Skipped)
	}
}

func TestCheck_SkipUnderVaultRootIsNotReported(t *testing.T) {
	vault := t.TempDir()
	if err := os.MkdirAll(filepath.Join(vault, ".obsidian"), 0o750); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(vault, "n.md"), "# N\n")
	withWalk(t, func(root string, fn fs.WalkDirFunc) error {
		if err := filepath.WalkDir(root, fn); err != nil {
			return err
		}
		_ = fn(filepath.Join(root, "locked"), stubEntry{name: "locked", dir: true}, fs.ErrPermission)
		return nil
	})
	idx, err := NewIndex([]string{vault})
	if err != nil {
		t.Fatal(err)
	}
	if len(idx.Skipped()) != 1 {
		t.Fatalf("index should still record the skip: %+v", idx.Skipped())
	}
	if rep := idx.Check(); len(rep.Skipped) != 0 {
		t.Errorf("a vault root is never checked, so its skips must not fail the check: %+v", rep.Skipped)
	}
}
