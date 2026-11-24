package docs

import (
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestRebuild_IncrementalMatchesColdAndSkipsUnchanged(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".obsidian"), 0o750); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "a.md"), "---\naliases: [alpha]\n---\n# Alpha\n\nLinks to [[b]] and [[missing]].\n\nParagraph ^blk-a\n")
	writeFile(t, filepath.Join(dir, "b.md"), "# Beta\n\nBack to [[alpha]] and [[c]].\n")
	writeFile(t, filepath.Join(dir, "c.md"), "# Gamma\n\nStandalone note.\n")

	var scanned []string
	prevScan := scanDocFn
	scanDocFn = func(kind RootKind, r io.Reader, relPath string) (docMeta, error) {
		scanned = append(scanned, relPath)
		return prevScan(kind, r, relPath)
	}
	t.Cleanup(func() { scanDocFn = prevScan })

	idx1, err := NewIndex([]string{dir})
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	if len(scanned) != 3 {
		t.Fatalf("initial scan count = %d (%v), want 3", len(scanned), scanned)
	}

	// Unchanged Rebuild: 0 files scanned, identical to cold build.
	scanned = nil
	idx2, err := idx1.Rebuild()
	if err != nil {
		t.Fatalf("Rebuild unchanged: %v", err)
	}
	if len(scanned) != 0 {
		t.Fatalf("Rebuild unchanged scanned %v, want 0 files", scanned)
	}
	if !sameIndex(idx1, idx2) {
		t.Fatal("Rebuild unchanged differs from initial index")
	}

	// Modify b.md, add d.md, delete c.md.
	future := time.Now().Add(2 * time.Second)
	bPath := filepath.Join(dir, "b.md")
	writeFile(t, bPath, "# Beta Revised\n\nNow links to [[d]] and [[alpha]].\n")
	if err := os.Chtimes(bPath, future, future); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "d.md"), "# Delta\n\nLinks to [[a]].\n")
	if err := os.Remove(filepath.Join(dir, "c.md")); err != nil {
		t.Fatal(err)
	}

	scanned = nil
	idx3, err := idx2.Rebuild()
	if err != nil {
		t.Fatalf("Rebuild after edits: %v", err)
	}
	wantScanned := []string{"b.md", "d.md"}
	if !reflect.DeepEqual(scanned, wantScanned) {
		t.Fatalf("incremental Rebuild scanned %v, want %v (a.md must be skipped)", scanned, wantScanned)
	}

	// Compare incremental Rebuild against a cold NewIndex on the exact same tree.
	cold, err := NewIndex([]string{dir})
	if err != nil {
		t.Fatalf("cold NewIndex: %v", err)
	}
	if !sameIndex(idx3, cold) {
		t.Fatal("incremental Rebuild does not match cold NewIndex")
	}
	if !reflect.DeepEqual(idx3.List(), cold.List()) {
		t.Fatalf("List mismatch:\ngot:  %+v\nwant: %+v", idx3.List(), cold.List())
	}
	for _, doc := range cold.List() {
		gotBL := idx3.Backlinks(&doc)
		wantBL := cold.Backlinks(&doc)
		if !reflect.DeepEqual(gotBL, wantBL) {
			t.Fatalf("Backlinks(%q) mismatch:\ngot:  %+v\nwant: %+v", doc.RelPath, gotBL, wantBL)
		}
	}

	// Verify no mutable slice aliasing between idx2 and idx3 for reused doc a.md.
	for i := range idx2.docs {
		if idx2.docs[i].RelPath == "a.md" && len(idx2.docs[i].Aliases) > 0 {
			idx2.docs[i].Aliases[0] = "mutated"
		}
	}
	aDoc, ok := idx3.Find(idx3.Roots()[0].Label, "a.md")
	if !ok || len(aDoc.Aliases) != 1 || aDoc.Aliases[0] != "alpha" {
		t.Fatalf("reused doc a.md aliased previous Index slice: %+v", aDoc.Aliases)
	}
}

func TestRebuild_SingleFileRootIncremental(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "solo.md")
	writeFile(t, f, "# Solo\n")

	var scanned []string
	prevScan := scanDocFn
	scanDocFn = func(kind RootKind, r io.Reader, relPath string) (docMeta, error) {
		scanned = append(scanned, relPath)
		return prevScan(kind, r, relPath)
	}
	t.Cleanup(func() { scanDocFn = prevScan })

	idx1, err := NewIndex([]string{f})
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	if len(scanned) != 1 {
		t.Fatalf("initial scan = %v, want 1", scanned)
	}

	scanned = nil
	idx2, err := idx1.Rebuild()
	if err != nil {
		t.Fatalf("Rebuild unchanged: %v", err)
	}
	if len(scanned) != 0 {
		t.Fatalf("Rebuild unchanged scanned %v, want 0", scanned)
	}
	if !sameIndex(idx1, idx2) {
		t.Fatal("unchanged single-file Rebuild differs")
	}

	future := time.Now().Add(2 * time.Second)
	writeFile(t, f, "# Solo Updated\n")
	if err := os.Chtimes(f, future, future); err != nil {
		t.Fatal(err)
	}
	scanned = nil
	idx3, err := idx2.Rebuild()
	if err != nil {
		t.Fatalf("Rebuild modified: %v", err)
	}
	if len(scanned) != 1 || idx3.List()[0].Title != "Solo Updated" {
		t.Fatalf("modified single-file Rebuild: scanned=%v title=%q", scanned, idx3.List()[0].Title)
	}
}

func TestRebuild_RootKindChangeInvalidatesCache(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "note.md"), "# Note\n\nSee [[other]].\n")

	var scanned []string
	prevScan := scanDocFn
	scanDocFn = func(kind RootKind, r io.Reader, relPath string) (docMeta, error) {
		scanned = append(scanned, relPath)
		return prevScan(kind, r, relPath)
	}
	t.Cleanup(func() { scanDocFn = prevScan })

	idx1, err := NewIndex([]string{dir})
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	if len(idx1.List()[0].Links) != 0 {
		t.Fatalf("docs root should not parse wikilink: %+v", idx1.List()[0].Links)
	}

	// Flip the root from RootDocs to RootVault without touching note.md.
	if err := os.Mkdir(filepath.Join(dir, ".obsidian"), 0o750); err != nil {
		t.Fatal(err)
	}
	scanned = nil
	idx2, err := idx1.Rebuild()
	if err != nil {
		t.Fatalf("Rebuild after .obsidian added: %v", err)
	}
	if len(scanned) != 1 {
		t.Fatalf("expected re-scan after RootKind change, got scanned=%v", scanned)
	}
	if len(idx2.List()[0].Links) != 1 || idx2.List()[0].Links[0].Path != "other" {
		t.Fatalf("vault Rebuild did not pick up wikilink: %+v", idx2.List()[0].Links)
	}
}
