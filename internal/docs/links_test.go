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

import (
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

	html, err := Render([]byte("## Getting Started\n"))
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	wantID := `id="` + slug + `"`
	if !strings.Contains(html, wantID) {
		t.Errorf("Render output %q does not contain %q (scanDoc slug %q)", html, wantID, slug)
	}
}
