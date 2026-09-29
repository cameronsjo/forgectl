package docs

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
	"go.abhg.dev/goldmark/wikilink"
)

// renderVaultFrom renders src as a vault page of idx, its wikilinks resolved
// from the indexed doc from, the way handleDoc does.
func renderVaultFrom(t *testing.T, idx *Index, from *Doc, src string) string {
	t.Helper()
	out, err := RenderDocFor(RootVault, []byte(src), idx, from)
	if err != nil {
		t.Fatalf("RenderDocFor(%q): %v", src, err)
	}
	return out.HTML
}

// renderVaultWith renders src as a vault page with a given resolver.
func renderVaultWith(t *testing.T, src string, resolve wikilinkResolver) string {
	t.Helper()
	out, err := renderWith([]byte(src), RootVault, resolve)
	if err != nil {
		t.Fatalf("renderWith(%q): %v", src, err)
	}
	return out
}

var (
	htmlTag  = regexp.MustCompile(`<[^>]*>`)
	htmlAttr = regexp.MustCompile(`\s([a-zA-Z-]+)="([^"]*)"`)
)

// tagAttrs returns every attribute, name and value, on the tags in out. The
// sanitizer writes every value double-quoted with any quote in it escaped,
// so a quoted value never hides another attribute from this scan.
func tagAttrs(out string) [][2]string {
	var attrs [][2]string
	for _, tag := range htmlTag.FindAllString(out, -1) {
		for _, m := range htmlAttr.FindAllStringSubmatch(tag, -1) {
			attrs = append(attrs, [2]string{m[1], m[2]})
		}
	}
	return attrs
}

// TestRenderVault_WikilinkResolvedBecomesAnchor: a wikilink that resolves
// renders as an anchor to the note's reader page, by path, by alias, and
// with an alias label shown in place of the target.
func TestRenderVault_WikilinkResolvedBecomesAnchor(t *testing.T) {
	idx := newLinksTestIndex(t)
	from := mustFindDoc(t, idx, "vault", "index.md")
	for src, want := range map[string]string{
		"[[notes/orphan]]":          `<a class="wikilink" href="/doc/vault/notes/orphan.md" rel="nofollow">notes/orphan</a>`,
		"[[Beta Note]]":             `<a class="wikilink" href="/doc/vault/notes/beta.md" rel="nofollow">Beta Note</a>`,
		"[[Alpha One]]":             `<a class="wikilink" href="/doc/vault/notes/Alpha.md" rel="nofollow">Alpha One</a>`,
		"[[notes/beta|Beta <b>]]":   `<a class="wikilink" href="/doc/vault/notes/beta.md" rel="nofollow">Beta &lt;b&gt;</a>`,
		"[[deep/Alpha|second one]]": `<a class="wikilink" href="/doc/vault/notes/deep/Alpha.md" rel="nofollow">second one</a>`,
	} {
		out := renderVaultFrom(t, idx, from, src+"\n")
		if !strings.Contains(out, want) {
			t.Errorf("%s: want %s in %s", src, want, out)
		}
		if strings.Contains(out, "wikilink-miss") {
			t.Errorf("%s: a hit is marked as a miss: %s", src, out)
		}
	}
}

// TestRenderVault_WikilinkHeadingUsesSlug: a heading link's fragment is the
// Slug of the heading it matched, the id the target page renders, never the
// text the author wrote.
func TestRenderVault_WikilinkHeadingUsesSlug(t *testing.T) {
	idx := newLinksTestIndex(t)
	from := mustFindDoc(t, idx, "vault", "index.md")
	for src, want := range map[string]string{
		"[[notes/anchors#Some Heading#Sub]]": `href="/doc/vault/notes/anchors.md#sub"`,
		"[[notes/anchors#some heading]]":     `href="/doc/vault/notes/anchors.md#some-heading"`,
		"[[notes/anchors#Some Heading]]":     `href="/doc/vault/notes/anchors.md#some-heading"`,
		"[[#Vault Index]]":                   `href="/doc/vault/index.md#vault-index"`,
	} {
		out := renderVaultFrom(t, idx, from, src+"\n")
		if !strings.Contains(out, want) {
			t.Errorf("%s: want %s in %s", src, want, out)
		}
		if strings.Contains(out, "wikilink-miss") || strings.Contains(out, "%20") {
			t.Errorf("%s: %s", src, out)
		}
	}
}

// TestRenderVault_WikilinkMissKinds: a wikilink with no note to link to is a
// span with no href, titled with its reason. The titles are exact: the
// sanitizer drops a title holding a colon, so each must survive it as is.
func TestRenderVault_WikilinkMissKinds(t *testing.T) {
	idx := newLinksTestIndex(t)
	from := mustFindDoc(t, idx, "vault", "index.md")
	for _, tc := range []struct{ src, title string }{
		{"[[Nope]]", "Broken link (no-target)"},
		{"[[Alpha]]", "Broken link (ambiguous)"},
		{"[[../repo/index]]", "Broken link (outside-root)"},
	} {
		out := renderVaultFrom(t, idx, from, tc.src+"\n")
		want := `<span class="wikilink wikilink-miss" title="` + tc.title + `">`
		if !strings.Contains(out, want) {
			t.Errorf("%s: want %s in %s", tc.src, want, out)
		}
		if strings.Contains(out, "href=") || strings.Contains(out, "<a ") {
			t.Errorf("%s: a miss carries a link: %s", tc.src, out)
		}
	}
}

// TestRenderVault_WikilinkAnchorMissLinksDoc: a note that resolved while
// its heading or block id did not still links to the note, with no
// fragment, marked as a miss.
func TestRenderVault_WikilinkAnchorMissLinksDoc(t *testing.T) {
	idx := newLinksTestIndex(t)
	from := mustFindDoc(t, idx, "vault", "index.md")
	for _, src := range []string{"[[notes/anchors#No Such]]", "[[notes/anchors#^nope]]", "[[#Nowhere]]"} {
		out := renderVaultFrom(t, idx, from, src+"\n")
		wantHref := `href="/doc/vault/notes/anchors.md"`
		if src == "[[#Nowhere]]" {
			wantHref = `href="/doc/vault/index.md"`
		}
		for _, want := range []string{
			`<a class="wikilink wikilink-miss"`,
			`title="Broken link (heading or block not found)"`,
			wantHref,
		} {
			if !strings.Contains(out, want) {
				t.Errorf("%s: want %s in %s", src, want, out)
			}
		}
		for _, a := range tagAttrs(out) {
			if a[0] == "href" && strings.Contains(a[1], "#") {
				t.Errorf("%s: the href keeps a fragment: %s", src, out)
			}
		}
	}
}

// TestRenderVault_WikilinkBlockRefLinksDoc: a block link that resolves
// jumps to the block, whose "^blk-1" id the target page renders
// (blockIDTransformer), built from the indexed id; url.URL escapes the '^'.
func TestRenderVault_WikilinkBlockRefLinksDoc(t *testing.T) {
	idx := newLinksTestIndex(t)
	from := mustFindDoc(t, idx, "vault", "index.md")
	out := renderVaultFrom(t, idx, from, "[[notes/anchors#^blk-1]]\n")
	if !strings.Contains(out, `<a class="wikilink" href="/doc/vault/notes/anchors.md#%5Eblk-1" rel="nofollow">`) {
		t.Errorf("block link: %s", out)
	}
	if strings.Contains(out, "wikilink-miss") {
		t.Errorf("block link marked as a miss: %s", out)
	}
	for _, a := range tagAttrs(out) {
		if a[0] == "href" && a[1] != "/doc/vault/notes/anchors.md#%5Eblk-1" {
			t.Errorf("block link href = %q", a[1])
		}
	}
}

// TestRenderVault_WikilinkNeverEmitsRawTarget: no href on the page comes
// from a wikilink's text. Hostile targets through the real resolver render
// as misses, and a resolver that returns anything but a reader page path
// is refused, so every href the render emits is a /doc/ path and no tag
// carries an attribute an author could have written.
func TestRenderVault_WikilinkNeverEmitsRawTarget(t *testing.T) {
	idx := newLinksTestIndex(t)
	from := mustFindDoc(t, idx, "vault", "index.md")
	hostile := []string{
		"[[javascript:alert(1)]]",
		"[[https://evil.example/]]",
		"[[//evil.example]]",
		`[[x" onmouseover="y]]`,
		`[[notes/orphan|x" onmouseover="y]]`,
		"[[notes/orphan#javascript:alert(1)]]",
		"[[/\\evil.example]]",
		"[[data:text/html,x]]",
	}
	check := func(label, out string) {
		t.Helper()
		for _, a := range tagAttrs(out) {
			name, value := a[0], a[1]
			switch name {
			case "class", "title", "rel":
			case "href":
				if !strings.HasPrefix(value, "/doc/") {
					t.Errorf("%s: href %q is not a reader path: %s", label, value, out)
				}
			default:
				t.Errorf("%s: unexpected attribute %s=%q: %s", label, name, value, out)
			}
			for _, bad := range []string{"evil.example", "javascript:", "onmouseover", "data:"} {
				if strings.Contains(strings.ToLower(value), bad) {
					t.Errorf("%s: attribute %s=%q carries %q: %s", label, name, value, bad, out)
				}
			}
		}
	}
	for _, src := range hostile {
		check(src, renderVaultFrom(t, idx, from, src+"\n"))
	}

	// A resolver is trusted only for an href that is a reader page path.
	for _, href := range []string{
		"javascript:alert(1)", "https://evil.example/", "//evil.example",
		" /doc/x", "/Doc/x", "doc/x", "", "data:text/html,x",
	} {
		resolve := wikilinkResolver(func(LinkRef) (string, Miss) { return href, MissNone })
		out := renderVaultWith(t, "[[anything]]\n", resolve)
		check("resolver "+href, out)
		if strings.Contains(out, "href=") || strings.Contains(out, "<a ") {
			t.Errorf("resolver href %q reached the page: %s", href, out)
		}
		if !strings.Contains(out, `<span class="wikilink wikilink-miss" title="Broken link (no-target)">anything</span>`) {
			t.Errorf("resolver href %q: want a no-target miss span: %s", href, out)
		}
	}

	// A reader path with a quote in it cannot break out of the attribute.
	resolve := wikilinkResolver(func(LinkRef) (string, Miss) { return `/doc/x" onmouseover="y`, MissNone })
	out := renderVaultWith(t, "[[anything]]\n", resolve)
	for _, a := range tagAttrs(out) {
		if a[0] != "class" && a[0] != "href" && a[0] != "rel" {
			t.Errorf("quoted href: unexpected attribute %s=%q: %s", a[0], a[1], out)
		}
	}
}

// TestRenderResolvedWikilink_RejectsNonDocHref: the renderer repeats the
// transformer's /doc/ check, so a node holding any other href still renders
// as a span, never as an anchor.
func TestRenderResolvedWikilink_RejectsNonDocHref(t *testing.T) {
	for _, href := range []string{"javascript:alert(1)", "https://evil.example/", "//evil.example"} {
		n := &resolvedWikilinkNode{Href: href}
		var buf bytes.Buffer
		w := bufio.NewWriter(&buf)
		for _, entering := range []bool{true, false} {
			if _, err := renderResolvedWikilink(w, nil, n, entering); err != nil {
				t.Fatal(err)
			}
		}
		_ = w.Flush()
		out := buf.String()
		want := `<span class="wikilink wikilink-miss" title="Broken link (no-target)"></span>`
		if out != want {
			t.Errorf("href %q rendered %q, want %q", href, out, want)
		}
	}
}

// TestRenderVault_WikilinkHrefEscapesReserved: an href is escaped as a URL
// path, so a space in a note's name cannot truncate the link.
func TestRenderVault_WikilinkHrefEscapesReserved(t *testing.T) {
	idx, label := tempVaultIndex(t, map[string]string{
		"index.md":   "# Index\n",
		"My Note.md": "# My Note\n\n## Été Über\n",
		"a&b.md":     "# A and B\n",
	})
	from := mustFindDoc(t, idx, label, "index.md")
	for src, want := range map[string]string{
		"[[My Note]]":            `href="/doc/` + label + `/My%20Note.md"`,
		"[[My Note#Été Über]]":   `href="/doc/` + label + `/My%20Note.md#t-ber"`,
		"[[a&b]]":                `href="/doc/` + label + `/a&amp;b.md"`,
		"[[My Note#Été Über|x]]": `#t-ber" rel="nofollow">x</a>`,
	} {
		out := renderVaultFrom(t, idx, from, src+"\n")
		if !strings.Contains(out, want) {
			t.Errorf("%s: want %s in %s", src, want, out)
		}
	}
}

// TestRenderVault_WikilinkDuplicateHeadingTakesFirst: with two headings of
// the same text, a link lands on the first, whose id has no "-1" suffix,
// as the resolver behind docs check and backlinks matches it.
func TestRenderVault_WikilinkDuplicateHeadingTakesFirst(t *testing.T) {
	idx, label := tempVaultIndex(t, map[string]string{
		"index.md": "# Index\n",
		"Note.md":  "# Note\n\n## Dup\n\ntext\n\n## Dup\n",
	})
	from := mustFindDoc(t, idx, label, "index.md")
	out := renderVaultFrom(t, idx, from, "[[Note#Dup]]\n")
	if want := `href="/doc/` + label + `/Note.md#dup"`; !strings.Contains(out, want) {
		t.Errorf("want %s in %s", want, out)
	}
}

// tempVaultIndex indexes a new vault holding files, returning the index and
// the vault root's label.
func tempVaultIndex(t *testing.T, files map[string]string) (*Index, string) {
	t.Helper()
	vault := t.TempDir()
	if err := os.MkdirAll(filepath.Join(vault, ".obsidian"), 0o750); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(vault, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	idx, err := NewIndex([]string{vault})
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	return idx, idx.Roots()[0].Label
}

// TestRenderVault_WikilinkInsideLinkStaysLiteral: a wikilink inside a
// markdown link or image is not resolved, so no anchor nests in another.
func TestRenderVault_WikilinkInsideLinkStaysLiteral(t *testing.T) {
	idx := newLinksTestIndex(t)
	from := mustFindDoc(t, idx, "vault", "index.md")

	out := renderVaultFrom(t, idx, from, "[a [[notes/orphan]]](c.md)\n")
	if n := strings.Count(out, "<a "); n != 1 {
		t.Errorf("want exactly one anchor, got %d: %s", n, out)
	}
	if !strings.Contains(out, "[[notes/orphan]]") || strings.Contains(out, "wikilink") {
		t.Errorf("a wikilink inside a link was not left as source: %s", out)
	}

	out = renderVaultFrom(t, idx, from, "![alt [[notes/orphan]]](p.png)\n")
	if strings.Contains(out, "<a ") || strings.Contains(out, "wikilink") || strings.Count(out, "<img") != 1 {
		t.Errorf("a wikilink inside an image became markup: %s", out)
	}
}

// TestRenderVault_EmbedLeftLiteral: an embed is not rendered yet, so it
// stays its own source text, with no image, link, or wikilink class.
func TestRenderVault_EmbedLeftLiteral(t *testing.T) {
	idx := newLinksTestIndex(t)
	from := mustFindDoc(t, idx, "vault", "index.md")
	for _, src := range []string{"![[notes/beta]]", "![[pic.png]]", "![[Nope]]"} {
		out := renderVaultFrom(t, idx, from, src+"\n")
		if out != "<p>"+src+"</p>\n" {
			t.Errorf("%s rendered %q", src, out)
		}
	}
}

// TestRenderVault_CommentedWikilinkNotResolved: a wikilink inside a comment
// is not on the page, so it is never resolved.
func TestRenderVault_CommentedWikilinkNotResolved(t *testing.T) {
	var calls []string
	resolve := wikilinkResolver(func(ref LinkRef) (string, Miss) {
		calls = append(calls, ref.Raw)
		return "", MissNoTarget
	})
	out := renderVaultWith(t, "text %%[[notes/orphan]]%% and [[shown]]\n", resolve)
	if strings.Join(calls, ",") != "shown" {
		t.Errorf("resolver calls = %q, want only the visible link: %s", calls, out)
	}
	if strings.Contains(out, "orphan") {
		t.Errorf("commented link reached the page: %s", out)
	}
}

// TestRenderVault_NoResolverFailsClosed: a vault render with no resolver
// never checked any target, so every wikilink is an unresolved miss span
// around its source text, and never a link.
func TestRenderVault_NoResolverFailsClosed(t *testing.T) {
	out := renderKind(t, "see [[notes/orphan]] and [[x|y]]\n", RootVault)
	for _, want := range []string{
		`<span class="wikilink wikilink-miss" title="Broken link (unresolved)">[[notes/orphan]]</span>`,
		`<span class="wikilink wikilink-miss" title="Broken link (unresolved)">[[x|y]]</span>`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("want %s in %s", want, out)
		}
	}
	if strings.Contains(out, "href=") {
		t.Errorf("an unresolved wikilink became a link: %s", out)
	}
	// RenderDocFor with no index or no doc is the same render.
	idx := newLinksTestIndex(t)
	from := mustFindDoc(t, idx, "vault", "index.md")
	for _, got := range []string{
		renderVaultFrom(t, nil, from, "see [[notes/orphan]] and [[x|y]]\n"),
		renderVaultFrom(t, idx, nil, "see [[notes/orphan]] and [[x|y]]\n"),
	} {
		if got != out {
			t.Errorf("nil index or doc rendered %s, want %s", got, out)
		}
	}
}

// TestScanVault_TransformerLeavesIndexUnchanged: the scan parses with the
// render's parser set, and no resolver, so the transformer must leave every
// wikilink node where the scan reads it.
func TestScanVault_TransformerLeavesIndexUnchanged(t *testing.T) {
	src := []byte("[[a]] [[b#c]] ![[d]]\n")
	scan, err := scanBodyFor(RootVault, src)
	if err != nil {
		t.Fatal(err)
	}
	var forms []LinkForm
	for _, l := range scan.links {
		forms = append(forms, l.Form)
	}
	if len(forms) != 3 || forms[0] != FormPlain || forms[1] != FormHeading || forms[2] != FormEmbed {
		t.Errorf("scan links = %+v", scan.links)
	}

	doc := linkMarkdownVault.Parser().Parse(text.NewReader(src), parser.WithContext(parser.NewContext()))
	nodes := 0
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if entering && n.Kind() == wikilink.Kind {
			nodes++
		}
		return ast.WalkContinue, nil
	})
	if nodes != 3 {
		t.Errorf("parse left %d wikilink nodes, want 3", nodes)
	}
}

// TestRenderVault_WikilinkVerdictMatchesResolver: the page marks a wikilink
// as a hit exactly when the resolver behind docs check and backlinks does.
func TestRenderVault_WikilinkVerdictMatchesResolver(t *testing.T) {
	idx := newLinksTestIndex(t)
	for _, rel := range []string{"index.md", "notes/linker.md"} {
		from := mustFindDoc(t, idx, "vault", rel)
		seen := 0
		for _, l := range from.Links {
			if l.Form == FormEmbed {
				continue
			}
			seen++
			_, miss := idx.resolveParts(from, l.Path, l.Fragment)
			out := renderVaultFrom(t, idx, from, "[["+l.Raw+"]]\n")
			hit := strings.Contains(out, `<a class="wikilink" `) && !strings.Contains(out, "wikilink-miss")
			if hit != (miss == MissNone) {
				t.Errorf("%s: %q resolves with miss %v, renders %s", rel, l.Raw, miss, out)
			}
		}
		if seen == 0 {
			t.Errorf("%s: no wikilinks to compare", rel)
		}
	}
	// Misses of every kind, which the fixture notes barely carry.
	from := mustFindDoc(t, idx, "vault", "index.md")
	for _, raw := range []string{
		"Nope", "Alpha", "../repo/index", "notes/anchors#No Such", "notes/anchors#^nope",
		"#Vault Index", "#Nowhere", "notes/anchors#^blk-1", "Alpha One",
	} {
		path0, frag := splitFirstHash(raw)
		_, miss := idx.resolveParts(from, path0, frag)
		out := renderVaultFrom(t, idx, from, "[["+raw+"]]\n")
		hit := strings.Contains(out, `<a class="wikilink" `) && !strings.Contains(out, "wikilink-miss")
		if hit != (miss == MissNone) {
			t.Errorf("%q resolves with miss %v, renders %s", raw, miss, out)
		}
	}
}

// TestRenderDocs_WikilinkStaysLiteral: a docs root renders byte for byte as
// before, resolver or not; "[[…]]" is plain text there.
func TestRenderDocs_WikilinkStaysLiteral(t *testing.T) {
	src := "# Guide\n\n[[guide]] [[x#y]] ![[z]] [[a|b]]\n"
	plain := renderKind(t, src, RootDocs)
	resolve := wikilinkResolver(func(LinkRef) (string, Miss) { return "/doc/docs/guide.md", MissNone })
	withResolver, err := renderWith([]byte(src), RootDocs, resolve)
	if err != nil {
		t.Fatal(err)
	}
	if withResolver != plain {
		t.Errorf("a resolver changed a docs render:\n%s\nvs\n%s", withResolver, plain)
	}
	want := "<h1 id=\"guide\">Guide</h1>\n<p>[[guide]] [[x#y]] ![[z]] [[a|b]]</p>\n"
	if plain != want {
		t.Errorf("docs render = %q, want %q", plain, want)
	}
	idx := newLinksTestIndex(t)
	from := mustFindDoc(t, idx, "vault", "index.md")
	rendered, err := RenderDocFor(RootDocs, []byte(src), idx, from)
	if err != nil {
		t.Fatal(err)
	}
	if rendered.HTML != plain {
		t.Errorf("RenderDocFor with an index changed a docs render: %s", rendered.HTML)
	}
}
