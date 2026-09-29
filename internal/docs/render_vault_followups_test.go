package docs

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

var blockIDAttr = regexp.MustCompile(`<(?:p|li) id="([^"]*)"`)

// blockIDAttrs returns every id attribute on a <p> or <li> in out.
func blockIDAttrs(out string) []string {
	var ids []string
	for _, m := range blockIDAttr.FindAllStringSubmatch(out, -1) {
		ids = append(ids, m[1])
	}
	return ids
}

// TestRenderVault_BlockIDRendered: a trailing "^id" marker becomes the id of
// its paragraph or list item and leaves the page, while a marker that is
// not one (no space before it, a standalone line, a callout's title
// paragraph) keeps its text and gets no id.
func TestRenderVault_BlockIDRendered(t *testing.T) {
	cases := []struct {
		src, want string
	}{
		{"A paragraph. ^blk-1\n", `<p id="blk-1">A paragraph.</p>`},
		{"two\nlines ^two\n", "<p id=\"two\">two\nlines</p>"},
		{"split by _ ^my_id\n", `<p id="my_id">split by _</p>`},
		{"> quoted ^q1\n", "<blockquote>\n<p id=\"q1\">quoted</p>"},
		{"- item ^li-1\n- other\n", `<li id="li-1">item</li>`},
		{"- loose ^li-2\n\n- other\n", "<li>\n<p id=\"li-2\">loose</p>"},
		{"[[x]] after a link ^wl\n", `id="wl"`},
	}
	for _, c := range cases {
		out := renderKind(t, c.src, RootVault)
		if !strings.Contains(out, c.want) {
			t.Errorf("%q: want %q in %s", c.src, c.want, out)
		}
		if strings.Contains(out, "^") {
			t.Errorf("%q: the marker is still on the page: %s", c.src, out)
		}
	}
	for _, src := range []string{
		"area r^2\n",
		"para\n\n^solo\n",
		"> [!tip]\n> body ^cal\n",
		"## Heading ^hd\n",
	} {
		out := renderKind(t, src, RootVault)
		if ids := blockIDAttrs(out); len(ids) != 0 {
			t.Errorf("%q: rendered ids %v: %s", src, ids, out)
		}
		if !strings.Contains(out, "^") {
			t.Errorf("%q: marker text dropped with no id placed: %s", src, out)
		}
	}
	if out := renderKind(t, "> [!tip]\n> body ^cal\n", RootVault); !strings.Contains(out, `class="callout tip"`) {
		t.Errorf("a block id in a callout's first paragraph broke the callout: %s", out)
	}
}

// TestRenderVault_BlockIDNotInCodeMathComment: a marker inside a code
// block, a $$ block or a %% comment is not a block id: nothing gets an id
// and the code keeps its text.
func TestRenderVault_BlockIDNotInCodeMathComment(t *testing.T) {
	const src = "```\nfence ^fenced\n```\n\n    indented ^ind\n\n$$\nx ^mth\n$$\n\n%%\nhid ^cmt\n%%\n\nvisible ^keep\n"
	out := renderKind(t, src, RootVault)
	if ids := blockIDAttrs(out); !slices.Equal(ids, []string{"keep"}) {
		t.Errorf("rendered ids %v, want [keep]: %s", ids, out)
	}
	for _, id := range []string{"fenced", "ind", "mth", "cmt"} {
		if strings.Contains(out, `id="`+id+`"`) {
			t.Errorf("%s got an id: %s", id, out)
		}
	}
	for _, kept := range []string{"^fenced", "^ind", "^mth"} {
		if !strings.Contains(out, kept) {
			t.Errorf("code lost %s: %s", kept, out)
		}
	}
}

// TestRenderDocs_BlockIDStaysLiteral: a docs root renders plain GFM byte
// for byte, marker and all.
func TestRenderDocs_BlockIDStaysLiteral(t *testing.T) {
	const src = "A paragraph. ^blk-1\n\n- item ^li-1\n"
	const want = "<p>A paragraph. ^blk-1</p>\n<ul>\n<li>item ^li-1</li>\n</ul>\n"
	if out := renderKind(t, src, RootDocs); out != want {
		t.Errorf("docs render changed:\n got %q\nwant %q", out, want)
	}
}

// TestScanVault_RenderedBlockIDsAreIndexed: every id the page renders is
// one the index holds, so a link to it resolves; for the forms the render
// handles, the two sets are equal.
func TestScanVault_RenderedBlockIDsAreIndexed(t *testing.T) {
	const src = "# T\n\npara ^p1\n\n> quote ^q1\n\n- item ^l1\n- loose\n\n  more ^l2\n\nsplit ^s_1\n\n```\ncode ^c1\n```\n"
	p := filepath.Join(t.TempDir(), "n.md")
	if err := os.WriteFile(p, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	meta, err := scanDocFor(RootVault, p, "n.md")
	if err != nil {
		t.Fatal(err)
	}
	rendered := blockIDAttrs(renderKind(t, src, RootVault))
	slices.Sort(rendered)
	if !slices.Equal(rendered, meta.BlockIDs) {
		t.Errorf("rendered ids %v, indexed %v", rendered, meta.BlockIDs)
	}
}

// TestRenderVault_EscapedPipeAlias: "[[note\|alias]]", Obsidian's alias in
// a table cell, resolves to the note in a table and outside one, showing
// the alias.
func TestRenderVault_EscapedPipeAlias(t *testing.T) {
	idx, label := tempVaultIndex(t, map[string]string{
		"a.md": "# A\n\n## Part\n",
		"b.md": "| h |\n|---|\n| [[a\\|al]] |\n\n[[a\\|out]] and [[a#Part\\|sec]]\n",
	})
	from := mustFindDoc(t, idx, label, "b.md")
	for _, l := range from.Links {
		if l.Path != "a" {
			t.Errorf("indexed link %+v, want path a", l)
		}
	}
	out := renderVaultFrom(t, idx, from, "| h |\n|---|\n| [[a\\|al]] |\n\n[[a\\|out]] and [[a#Part\\|sec]]\n")
	for _, want := range []string{
		`<td><a class="wikilink" href="/doc/` + label + `/a.md" rel="nofollow">al</a></td>`,
		`<a class="wikilink" href="/doc/` + label + `/a.md" rel="nofollow">out</a>`,
		`<a class="wikilink" href="/doc/` + label + `/a.md#part" rel="nofollow">sec</a>`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("want %s in %s", want, out)
		}
	}
	if strings.Contains(out, "wikilink-miss") {
		t.Errorf("an escaped-pipe alias missed: %s", out)
	}
}

// TestScan_SetextSoftBreakIsASpace: a setext heading's line break reads as
// a space in its Text, so a vault link written the way the heading reads
// resolves.
func TestScan_SetextSoftBreakIsASpace(t *testing.T) {
	for _, kind := range []RootKind{RootDocs, RootVault} {
		scan, err := scanBodyFor(kind, []byte("first part\nsecond *part*\n===\n"))
		if err != nil || len(scan.headings) != 1 {
			t.Fatalf("kind %v: %v, %v", kind, scan.headings, err)
		}
		if got, want := scan.headings[0].Text, "first part second part"; got != want {
			t.Errorf("kind %v: heading text %q, want %q", kind, got, want)
		}
	}
	idx, label := tempVaultIndex(t, map[string]string{
		"n.md": "first part\nsecond part\n===\n",
		"f.md": "x\n",
	})
	from := mustFindDoc(t, idx, label, "f.md")
	if _, miss := idx.ResolveLink(from, "n#first part second part"); miss != MissNone {
		t.Errorf("setext heading link: miss %v", miss)
	}
}

// TestRenderVault_WikilinkInsideRawAnchorStaysLiteral: a wikilink after a
// raw-HTML <a> the author opened in the same block renders as its source,
// so no anchor nests in another. One after the </a>, or after a tag that
// is not <a>, still resolves.
func TestRenderVault_WikilinkInsideRawAnchorStaysLiteral(t *testing.T) {
	idx := newLinksTestIndex(t)
	from := mustFindDoc(t, idx, "vault", "index.md")
	for _, src := range []string{
		`x <a href="https://e.x">see [[notes/orphan]]</a> y`,
		`<A HREF="https://e.x">see *[[notes/orphan]]*</A> y`,
		"x <a\nhref=\"https://e.x\">see [[notes/orphan]]</a>",
	} {
		out := renderVaultFrom(t, idx, from, src+"\n")
		if n := strings.Count(strings.ToLower(out), "<a "); n != 1 {
			t.Errorf("%q: want one anchor, got %d: %s", src, n, out)
		}
		if !strings.Contains(out, "[[notes/orphan]]") || strings.Contains(out, "wikilink") {
			t.Errorf("%q: the wikilink is not its source text: %s", src, out)
		}
	}
	for _, src := range []string{
		`<a href="https://e.x">see</a> [[notes/orphan]]`,
		`<abbr title="t">x</abbr> [[notes/orphan]]`,
		"<a href=\"https://e.x\">open\n\n[[notes/orphan]]",
	} {
		out := renderVaultFrom(t, idx, from, src+"\n")
		if !strings.Contains(out, `<a class="wikilink" href="/doc/vault/notes/orphan.md"`) {
			t.Errorf("%q: the wikilink did not resolve: %s", src, out)
		}
	}
}
