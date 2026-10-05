package docs

// #655: a link is indexed exactly when the page renders it as a link. The
// docs-root page has no wikilink extension, so "[[w]]" there is literal text;
// the vault page renders it (as a link or a marked miss).

import (
	"strings"
	"testing"
)

func TestScanBody_WikilinkIndexedIffRendered(t *testing.T) {
	const src = "# T\n\nSee [[w]] and [[w#H|alias]] and [md](m.md).\n"
	for _, tc := range []struct {
		kind      RootKind
		wikilinks bool
	}{
		{RootDocs, false},
		{RootVault, true},
	} {
		scan, err := scanBodyFor(tc.kind, []byte(src))
		if err != nil {
			t.Fatalf("scanBodyFor: %v", err)
		}
		wiki := 0
		for _, l := range scan.links {
			if l.Form != FormRelPath {
				wiki++
			}
		}
		rendered, err := RenderDocFor(tc.kind, []byte(src), nil, nil)
		if err != nil {
			t.Fatalf("RenderDocFor: %v", err)
		}
		renderedAsLink := strings.Contains(rendered.HTML, "wikilink")
		literal := strings.Contains(rendered.HTML, "[[w]]")
		if renderedAsLink != tc.wikilinks || (!tc.wikilinks && !literal) {
			t.Errorf("kind %v: page renders wikilink=%v literal=%v, want wikilink=%v", tc.kind, renderedAsLink, literal, tc.wikilinks)
		}
		if (wiki > 0) != renderedAsLink {
			t.Errorf("kind %v: indexed %d wikilinks but the page renders them as links = %v", tc.kind, wiki, renderedAsLink)
		}
		if tc.wikilinks && wiki != 2 {
			t.Errorf("vault indexed %d wikilinks, want 2", wiki)
		}
		if len(scan.links)-wiki != 1 {
			t.Errorf("kind %v: the markdown link must stay indexed, got %+v", tc.kind, scan.links)
		}
	}
}
