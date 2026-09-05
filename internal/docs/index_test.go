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
// titleFor (Classification: helper)
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
	"os"
	"path/filepath"
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

	_, err = idx.Resolve(label, "../../../../etc/passwd")
	if !errors.Is(err, ErrOutsideRoot) {
		t.Errorf("Resolve traversal: err = %v, want ErrOutsideRoot", err)
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
