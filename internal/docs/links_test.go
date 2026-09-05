package docs

// Test plan for links.go (ResolveLink)
//
// ResolveLink (Classification: pure lookup over per-root tables built by
// NewIndex; no I/O of its own)
//   [x] Happy: vault basename hit (byName fallback)
//   [x] Unhappy: MissNoTarget when no doc matches
//   [x] Unhappy: MissAmbiguous when two docs share a basename
//   [x] Happy: a "/"-qualified target disambiguates an otherwise-ambiguous
//       basename via RelPath suffix filtering
//   [x] Unhappy: MissOutsideRoot for a vault target that escapes the root
//       and a docs-root relative link that escapes the root
//   [x] Happy: alias list and alias scalar hits
//   [x] Happy: case-folded target still hits
//   [x] Happy/Unhappy: vault heading anchor hit and miss
//   [x] Happy: Obsidian block-id anchor hit
//   [x] Happy: nested heading path "A#B" matches on its last segment
//   [x] Happy: docs-root anchor matches goldmark's auto-ID slug
//   [x] Unhappy: within-root isolation — a vault doc can't resolve a
//       docs-root basename and vice versa
//   [x] Happy: a fragment-only link ("#heading") resolves to the calling doc
//   [x] Pinning: a heading's Slug agrees with the id Render() actually emits
//   [x] Happy: a "./" or "../" target in a vault resolves relative to the
//       linking doc, and one that climbs past the root is MissOutsideRoot
//   [x] Happy: a note whose name contains a dot ("Node.js.md") is linkable
//       as [[Node.js]] and never mistaken for "Node.md"
//   [x] Happy: a docs root keeps exact case and distinguishes "notes.md"
//       from "notes.markdown"
//   [x] Happy: a root-absolute markdown link ("/guide.md") resolves from
//       the root, not the linking doc's directory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newLinksTestIndex(t *testing.T) *Index {
	t.Helper()
	idx, err := NewIndex([]string{
		filepath.Join("testdata", "links", "vault"),
		filepath.Join("testdata", "links", "repo"),
	})
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	return idx
}

func mustFindDoc(t *testing.T, idx *Index, rootLabel, relPath string) *Doc {
	t.Helper()
	d, ok := idx.Find(rootLabel, relPath)
	if !ok {
		t.Fatalf("Find(%q, %q): not found", rootLabel, relPath)
	}
	return &d
}

func TestResolveLink_VaultBasenameHit(t *testing.T) {
	idx := newLinksTestIndex(t)
	from := mustFindDoc(t, idx, "vault", "index.md")

	doc, miss := idx.ResolveLink(from, "orphan")
	if miss != MissNone {
		t.Fatalf("miss = %v, want MissNone", miss)
	}
	if doc == nil || doc.RelPath != "notes/orphan.md" {
		t.Errorf("doc = %+v, want notes/orphan.md", doc)
	}
}

func TestResolveLink_MissNoTarget_NoFile(t *testing.T) {
	idx := newLinksTestIndex(t)
	from := mustFindDoc(t, idx, "vault", "index.md")

	doc, miss := idx.ResolveLink(from, "does-not-exist")
	if miss != MissNoTarget {
		t.Fatalf("miss = %v, want MissNoTarget", miss)
	}
	if doc != nil {
		t.Errorf("doc = %+v, want nil", doc)
	}
}

func TestResolveLink_MissAmbiguous_TwoAlphaFiles(t *testing.T) {
	idx := newLinksTestIndex(t)
	from := mustFindDoc(t, idx, "vault", "index.md")

	doc, miss := idx.ResolveLink(from, "Alpha")
	if miss != MissAmbiguous {
		t.Fatalf("miss = %v, want MissAmbiguous", miss)
	}
	if doc != nil {
		t.Errorf("doc = %+v, want nil", doc)
	}
}

func TestResolveLink_PathPrefixDisambiguatesAmbiguousBasename(t *testing.T) {
	idx := newLinksTestIndex(t)
	from := mustFindDoc(t, idx, "vault", "index.md")

	doc, miss := idx.ResolveLink(from, "deep/Alpha")
	if miss != MissNone {
		t.Fatalf("miss = %v, want MissNone", miss)
	}
	if doc == nil || doc.RelPath != "notes/deep/Alpha.md" {
		t.Errorf("doc = %+v, want notes/deep/Alpha.md", doc)
	}
}

func TestResolveLink_MissOutsideRoot_VaultEscape(t *testing.T) {
	idx := newLinksTestIndex(t)
	from := mustFindDoc(t, idx, "vault", "index.md")

	doc, miss := idx.ResolveLink(from, "../repo/index")
	if miss != MissOutsideRoot {
		t.Fatalf("miss = %v, want MissOutsideRoot", miss)
	}
	if doc != nil {
		t.Errorf("doc = %+v, want nil", doc)
	}
}

func TestResolveLink_MissOutsideRoot_DocsRelativeEscape(t *testing.T) {
	idx := newLinksTestIndex(t)
	from := mustFindDoc(t, idx, "repo", "index.md")

	doc, miss := idx.ResolveLink(from, "../../etc/passwd")
	if miss != MissOutsideRoot {
		t.Fatalf("miss = %v, want MissOutsideRoot", miss)
	}
	if doc != nil {
		t.Errorf("doc = %+v, want nil", doc)
	}
}

func TestResolveLink_AliasList(t *testing.T) {
	idx := newLinksTestIndex(t)
	from := mustFindDoc(t, idx, "vault", "index.md")

	doc, miss := idx.ResolveLink(from, "Alpha One")
	if miss != MissNone {
		t.Fatalf("miss = %v, want MissNone", miss)
	}
	if doc == nil || doc.RelPath != "notes/Alpha.md" {
		t.Errorf("doc = %+v, want notes/Alpha.md", doc)
	}
}

func TestResolveLink_AliasScalar(t *testing.T) {
	idx := newLinksTestIndex(t)
	from := mustFindDoc(t, idx, "vault", "index.md")

	doc, miss := idx.ResolveLink(from, "Beta Note")
	if miss != MissNone {
		t.Fatalf("miss = %v, want MissNone", miss)
	}
	if doc == nil || doc.RelPath != "notes/beta.md" {
		t.Errorf("doc = %+v, want notes/beta.md", doc)
	}
}

func TestResolveLink_CaseFold(t *testing.T) {
	idx := newLinksTestIndex(t)
	from := mustFindDoc(t, idx, "vault", "index.md")

	doc, miss := idx.ResolveLink(from, "NOTES/ORPHAN")
	if miss != MissNone {
		t.Fatalf("miss = %v, want MissNone", miss)
	}
	if doc == nil || doc.RelPath != "notes/orphan.md" {
		t.Errorf("doc = %+v, want notes/orphan.md", doc)
	}
}

func TestResolveLink_VaultHeadingAnchorHit(t *testing.T) {
	idx := newLinksTestIndex(t)
	from := mustFindDoc(t, idx, "vault", "index.md")

	doc, miss := idx.ResolveLink(from, "notes/anchors#Some Heading")
	if miss != MissNone {
		t.Fatalf("miss = %v, want MissNone", miss)
	}
	if doc == nil || doc.RelPath != "notes/anchors.md" {
		t.Errorf("doc = %+v, want notes/anchors.md", doc)
	}
}

func TestResolveLink_VaultHeadingAnchorMiss(t *testing.T) {
	idx := newLinksTestIndex(t)
	from := mustFindDoc(t, idx, "vault", "index.md")

	doc, miss := idx.ResolveLink(from, "notes/anchors#Nonexistent Heading")
	if miss != MissNoTarget {
		t.Fatalf("miss = %v, want MissNoTarget", miss)
	}
	if doc == nil || doc.RelPath != "notes/anchors.md" {
		t.Errorf("doc = %+v, want the resolved file even on an anchor miss", doc)
	}
}

func TestResolveLink_BlockID(t *testing.T) {
	idx := newLinksTestIndex(t)
	from := mustFindDoc(t, idx, "vault", "index.md")

	doc, miss := idx.ResolveLink(from, "notes/anchors#^blk-1")
	if miss != MissNone {
		t.Fatalf("miss = %v, want MissNone", miss)
	}
	if doc == nil || doc.RelPath != "notes/anchors.md" {
		t.Errorf("doc = %+v, want notes/anchors.md", doc)
	}
}

func TestResolveLink_NestedHeadingMatchesLastSegment(t *testing.T) {
	idx := newLinksTestIndex(t)
	from := mustFindDoc(t, idx, "vault", "index.md")

	doc, miss := idx.ResolveLink(from, "anchors#Some Heading#Sub")
	if miss != MissNone {
		t.Fatalf("miss = %v, want MissNone", miss)
	}
	if doc == nil || doc.RelPath != "notes/anchors.md" {
		t.Errorf("doc = %+v, want notes/anchors.md", doc)
	}
}

func TestResolveLink_DocsRootAnchor(t *testing.T) {
	idx := newLinksTestIndex(t)
	from := mustFindDoc(t, idx, "repo", "index.md")

	doc, miss := idx.ResolveLink(from, "guide.md#getting-started")
	if miss != MissNone {
		t.Fatalf("miss = %v, want MissNone", miss)
	}
	if doc == nil || doc.RelPath != "guide.md" {
		t.Errorf("doc = %+v, want guide.md", doc)
	}
}

func TestResolveLink_WithinRootIsolation(t *testing.T) {
	idx := newLinksTestIndex(t)

	vaultDoc := mustFindDoc(t, idx, "vault", "index.md")
	if doc, miss := idx.ResolveLink(vaultDoc, "guide"); miss != MissNoTarget || doc != nil {
		t.Errorf("vault->repo leak: doc = %+v, miss = %v, want nil/MissNoTarget", doc, miss)
	}

	repoDoc := mustFindDoc(t, idx, "repo", "index.md")
	if doc, miss := idx.ResolveLink(repoDoc, "orphan"); miss != MissNoTarget || doc != nil {
		t.Errorf("repo->vault leak: doc = %+v, miss = %v, want nil/MissNoTarget", doc, miss)
	}
}

func TestResolveLink_FragmentOnlyResolvesToFrom(t *testing.T) {
	idx := newLinksTestIndex(t)
	from := mustFindDoc(t, idx, "vault", "notes/anchors.md")

	doc, miss := idx.ResolveLink(from, "#some-heading")
	if miss != MissNone {
		t.Fatalf("miss = %v, want MissNone", miss)
	}
	if doc != from {
		t.Errorf("doc = %+v, want the same *Doc as from (%+v)", doc, from)
	}
}

// TestResolveLink_HeadingSlugAgreesWithRender pins the Global Constraint:
// scanDoc's Heading.Slug must equal the id goldmark's auto-heading-id pass
// actually assigns when render.go renders the same source, or a resolved
// anchor link could point at an id the rendered page never produces.
func TestResolveLink_HeadingSlugAgreesWithRender(t *testing.T) {
	idx := newLinksTestIndex(t)
	guide := mustFindDoc(t, idx, "repo", "guide.md")

	var slug string
	for _, h := range guide.Headings {
		if h.Text == "Getting Started" {
			slug = h.Slug
			break
		}
	}
	if slug == "" {
		t.Fatalf("guide.md has no %q heading in %+v", "Getting Started", guide.Headings)
	}

	// Render the fixture's own bytes, not a synthetic heading: goldmark
	// suffixes a repeated id within one document ("getting-started-1"),
	// so only the same document context pins the same slug.
	src, err := os.ReadFile(guide.AbsPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	html, err := Render(src)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	wantID := `id="` + slug + `"`
	if !strings.Contains(html, wantID) {
		t.Errorf("Render output %q does not contain %q (scanDoc slug %q)", html, wantID, slug)
	}
}

func TestResolveLink_VaultRelativeTargetResolvesFromLinkingDoc(t *testing.T) {
	idx := newLinksTestIndex(t)
	from := mustFindDoc(t, idx, "vault", "notes/deep/Alpha.md")

	cases := []struct {
		target string
		want   string
		miss   Miss
	}{
		{"../beta.md", "notes/beta.md", MissNone},
		{"./../anchors", "notes/anchors.md", MissNone},
		{"../../index.md", "index.md", MissNone},
		{"../../../repo/index.md", "", MissOutsideRoot},
	}
	for _, c := range cases {
		doc, miss := idx.ResolveLink(from, c.target)
		if miss != c.miss {
			t.Errorf("ResolveLink(%q) miss = %v, want %v", c.target, miss, c.miss)
			continue
		}
		if c.want == "" {
			if doc != nil {
				t.Errorf("ResolveLink(%q) doc = %+v, want nil", c.target, doc)
			}
			continue
		}
		if doc == nil || doc.RelPath != c.want {
			t.Errorf("ResolveLink(%q) doc = %+v, want %s", c.target, doc, c.want)
		}
	}
}

func TestResolveLink_DottedFilenameIsNotTruncated(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".obsidian"), 0o750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	writeFile(t, filepath.Join(dir, "Node.js.md"), "# Node.js\n")
	writeFile(t, filepath.Join(dir, "Node.md"), "# Node\n")
	writeFile(t, filepath.Join(dir, "index.md"), "[[Node.js]] [[Node]]\n")
	idx, err := NewIndex([]string{dir})
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	roots := idx.Roots()
	from := mustFindDoc(t, idx, roots[0].Label, "index.md")

	for target, want := range map[string]string{"Node.js": "Node.js.md", "Node": "Node.md", "Node.js.md": "Node.js.md"} {
		doc, miss := idx.ResolveLink(from, target)
		if miss != MissNone || doc == nil || doc.RelPath != want {
			t.Errorf("ResolveLink(%q) = (%+v, %v), want %s", target, doc, miss, want)
		}
	}
}

func TestResolveLink_DocsRootKeepsExactCaseAndExtension(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "notes.md"), "# md\n")
	writeFile(t, filepath.Join(dir, "notes.markdown"), "# markdown\n")
	writeFile(t, filepath.Join(dir, "index.md"), "[a](notes.md) [b](notes.markdown)\n")
	idx, err := NewIndex([]string{dir})
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	roots := idx.Roots()
	if roots[0].Kind != RootDocs {
		t.Fatalf("Kind = %v, want RootDocs", roots[0].Kind)
	}
	from := mustFindDoc(t, idx, roots[0].Label, "index.md")

	for _, target := range []string{"notes.md", "notes.markdown"} {
		doc, miss := idx.ResolveLink(from, target)
		if miss != MissNone || doc == nil || doc.RelPath != target {
			t.Errorf("ResolveLink(%q) = (%+v, %v), want an exact hit", target, doc, miss)
		}
	}
	if doc, miss := idx.ResolveLink(from, "Notes.md"); miss != MissNoTarget || doc != nil {
		t.Errorf("ResolveLink(Notes.md) on a docs root = (%+v, %v), want MissNoTarget — docs roots do not fold case", doc, miss)
	}
}

func TestResolveLink_DocsRootAbsoluteLinkResolvesFromRoot(t *testing.T) {
	idx := newLinksTestIndex(t)
	from := mustFindDoc(t, idx, "repo", "sub/nested.md")

	doc, miss := idx.ResolveLink(from, "/guide.md#getting-started")
	if miss != MissNone {
		t.Fatalf("miss = %v, want MissNone", miss)
	}
	if doc == nil || doc.RelPath != "guide.md" {
		t.Errorf("doc = %+v, want guide.md (root-relative, not sub/guide.md)", doc)
	}
	if doc, miss := idx.ResolveLink(from, "/../guide.md"); miss != MissNone || doc == nil || doc.RelPath != "guide.md" {
		t.Errorf("ResolveLink(/../guide.md) = (%+v, %v), want guide.md — a leading slash cannot climb above the root", doc, miss)
	}
}

func TestResolveLink_EncodedHashInFilenameStaysInPath(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "A#B.md"), "# Hash\n")
	writeFile(t, filepath.Join(dir, "index.md"), "[x](A%23B.md)\n")
	idx, err := NewIndex([]string{dir})
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	label := idx.Roots()[0].Label
	from := mustFindDoc(t, idx, label, "index.md")
	if len(from.Links) != 1 || from.Links[0].Path != "A#B.md" || from.Links[0].Fragment != "" {
		t.Fatalf("Links = %+v, want Path A#B.md with no fragment", from.Links)
	}
	target := mustFindDoc(t, idx, label, "A#B.md")
	got := idx.Backlinks(target)
	if len(got) != 1 || got[0].RelPath != "index.md" {
		t.Errorf("Backlinks(A#B.md) = %+v, want index.md — the encoded '#' must not be re-split", got)
	}
}

func TestResolveLink_VaultRootAbsoluteIsVaultRelative(t *testing.T) {
	idx := newLinksTestIndex(t)
	from := mustFindDoc(t, idx, "vault", "notes/deep/Alpha.md")
	doc, miss := idx.ResolveLink(from, "/notes/beta")
	if miss != MissNone || doc == nil || doc.RelPath != "notes/beta.md" {
		t.Errorf("ResolveLink(/notes/beta) = (%+v, %v), want notes/beta.md", doc, miss)
	}
}

func TestIndex_ListAndFindReturnIndependentCopies(t *testing.T) {
	idx := newLinksTestIndex(t)
	anchors := mustFindDoc(t, idx, "vault", "notes/anchors.md")
	if len(anchors.Headings) == 0 {
		t.Fatal("anchors.md has no headings")
	}
	want := anchors.Headings[0].Slug
	anchors.Headings[0].Slug = "tampered"
	for _, d := range idx.List() {
		if d.RootLabel == "vault" && d.RelPath == "notes/anchors.md" {
			d.Headings[0].Slug = "tampered-again"
		}
	}
	again := mustFindDoc(t, idx, "vault", "notes/anchors.md")
	if again.Headings[0].Slug != want {
		t.Errorf("Slug after writes through Find/List copies = %q, want %q untouched", again.Headings[0].Slug, want)
	}
}
