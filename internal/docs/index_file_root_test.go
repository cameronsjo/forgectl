package docs

// Test plan for the single-file-root path in index.go (NewIndex/indexFileRoot).
//
// NewIndex with a file argument (Classification: security gate — scope of grant)
//   [x] Happy: a single markdown file argument is indexed as its own root
//   [x] Unhappy: naming one file does NOT grant access to its sibling files
//   [x] Unhappy: a non-markdown file argument is a hard error
//   [x] Happy: mixing a directory root and a file root both work in one Index

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestNewIndex_FileArg_IndexedAsOwnRoot(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "note.md")
	writeFile(t, target, "# Note")

	idx, err := NewIndex([]string{target})
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	docs := idx.List()
	if len(docs) != 1 {
		t.Fatalf("List() has %d docs, want 1: %+v", len(docs), docs)
	}
	if docs[0].RelPath != "note.md" {
		t.Errorf("RelPath = %q, want %q", docs[0].RelPath, "note.md")
	}
}

func TestNewIndex_FileArg_DoesNotGrantSiblingAccess(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "note.md")
	sibling := filepath.Join(dir, "sibling.md")
	writeFile(t, target, "# Note")
	writeFile(t, sibling, "# Sibling")

	idx, err := NewIndex([]string{target})
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	label := idx.Roots()[0].Label

	// The named file resolves.
	if _, err := idx.Resolve(label, "note.md"); err != nil {
		t.Errorf("Resolve named file: %v", err)
	}
	// Its sibling, in the same directory, must NOT resolve — naming one
	// file must not silently open the whole directory.
	_, err = idx.Resolve(label, "sibling.md")
	if !errors.Is(err, ErrOutsideRoot) {
		t.Errorf("Resolve(sibling): err = %v, want ErrOutsideRoot (single-file root must not leak siblings)", err)
	}
	// The sibling must also be absent from the index itself.
	if _, ok := idx.Find(label, "sibling.md"); ok {
		t.Error("sibling.md was indexed under a single-file root, want it absent")
	}
}

func TestNewIndex_FileArg_NonMarkdown_Errors(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "notes.txt")
	writeFile(t, target, "not markdown")

	if _, err := NewIndex([]string{target}); err == nil {
		t.Error("NewIndex with a non-markdown file: got nil error, want one")
	}
}

// TestNewIndex_FileArg_ScansHeadingsAndLinks pins the single-file-root path
// (indexFileRoot) through the same scanDoc call the directory-walk path
// (walkRoot) uses — a Doc built from a lone file argument must carry
// non-nil Headings and Links exactly like one discovered by a directory
// walk, not a bare Title-only stub.
func TestNewIndex_FileArg_ScansHeadingsAndLinks(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "note.md")
	writeFile(t, target, "# Note\n\n## A Heading\n\nSee [[other]] and [text](../elsewhere.md).\n")

	idx, err := NewIndex([]string{target})
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	docs := idx.List()
	if len(docs) != 1 {
		t.Fatalf("List() has %d docs, want 1: %+v", len(docs), docs)
	}
	doc := docs[0]
	if doc.Headings == nil {
		t.Error("Headings is nil, want the doc's headings")
	}
	if doc.Links == nil {
		t.Error("Links is nil, want the doc's outbound links")
	}
	if len(doc.Headings) != 2 || doc.Headings[1].Text != "A Heading" {
		t.Errorf("Headings = %+v, want [Note, A Heading]", doc.Headings)
	}
	if len(doc.Links) != 2 {
		t.Errorf("Links = %+v, want 2 entries", doc.Links)
	}
}

func TestNewIndex_MixedDirAndFileRoots(t *testing.T) {
	dirRoot := t.TempDir()
	writeFile(t, filepath.Join(dirRoot, "a.md"), "# A")

	fileDir := t.TempDir()
	fileRoot := filepath.Join(fileDir, "b.md")
	writeFile(t, fileRoot, "# B")

	idx, err := NewIndex([]string{dirRoot, fileRoot})
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	if len(idx.Roots()) != 2 {
		t.Fatalf("Roots() has %d entries, want 2", len(idx.Roots()))
	}
	if len(idx.List()) != 2 {
		t.Fatalf("List() has %d docs, want 2", len(idx.List()))
	}
}
