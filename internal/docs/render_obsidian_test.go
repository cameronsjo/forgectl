package docs

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
	"go.abhg.dev/goldmark/wikilink"
)

// renderKind is render with a test failure on error.
func renderKind(t *testing.T, src string, kind RootKind) string {
	t.Helper()
	out, err := render([]byte(src), kind)
	if err != nil {
		t.Fatalf("render(%q): %v", src, err)
	}
	return out
}

func TestRenderVault_HighlightBecomesMark(t *testing.T) {
	out := renderKind(t, "a ==hot== b", RootVault)
	if !strings.Contains(out, "<mark>hot</mark>") {
		t.Errorf("want <mark>hot</mark>, got %s", out)
	}
}

func TestRenderVault_HighlightNeedsFlanking(t *testing.T) {
	for _, src := range []string{"a == b == c", "x ===y=== z"} {
		out := renderKind(t, src, RootVault)
		if strings.Contains(out, "<mark>") {
			t.Errorf("%q: want no <mark>, got %s", src, out)
		}
	}
}

// TestRenderDocs_FlavorStaysLiteral pins that a docs root is plain GFM: none
// of the vault dialect reaches it.
func TestRenderDocs_FlavorStaysLiteral(t *testing.T) {
	out := renderKind(t, "==x== %%y%% #z\n\n%%\nblock\n%%\n\n> [!info]\n> body\n", RootDocs)
	for _, want := range []string{"==x==", "%%y%%", "#z", "block", "[!info]"} {
		if !strings.Contains(out, want) {
			t.Errorf("docs root dropped literal %q: %s", want, out)
		}
	}
	for _, bad := range []string{"<mark>", `class="tag"`, "callout"} {
		if strings.Contains(out, bad) {
			t.Errorf("docs root rendered vault markup %q: %s", bad, out)
		}
	}
}

func TestRenderVault_InlineCommentStripped(t *testing.T) {
	out := renderKind(t, "keep %%secret%% keep", RootVault)
	if strings.Contains(out, "secret") || strings.Contains(out, "%%") {
		t.Errorf("inline comment leaked: %s", out)
	}
	if strings.Count(out, "keep") != 2 {
		t.Errorf("text around the comment lost: %s", out)
	}
}

func TestRenderVault_BlockCommentStripped(t *testing.T) {
	cases := map[string]string{
		"multi-line": "%%\nhidden\n\nhidden2\n%%\n\nafter",
		"one-line":   "%%hidden hidden2%%\n\nafter",
	}
	for name, src := range cases {
		out := renderKind(t, src, RootVault)
		if strings.Contains(out, "hidden") || strings.Contains(out, "%%") {
			t.Errorf("%s: block comment leaked: %s", name, out)
		}
		if !strings.Contains(out, "after") {
			t.Errorf("%s: text after the comment lost: %s", name, out)
		}
	}
}

// TestRenderVault_UnterminatedCommentStaysVisible is the load-bearing
// keep-when-unsure test: a "%%" whose end cannot be located must never hide
// what follows it.
func TestRenderVault_UnterminatedCommentStaysVisible(t *testing.T) {
	cases := []struct {
		src  string
		want []string
	}{
		{"a %%b\n\nc", []string{"a %%b", "c"}},
		{"%%\nrest of doc\n", []string{"%%", "rest of doc"}},
		{"%%\nrest\n\nmore rest\n", []string{"rest", "more rest"}},
		// The first "%%" after the opener has text behind it, so the block
		// declines; the paragraph's own "%%" pair then hides only up to
		// that "%%", and the text behind it stays.
		{"%%\nh\nx %% shown\n\nz", []string{"shown", "z"}},
		// A comment with text after its closer: only the delimited part
		// may go.
		{"%%gone%% tail", []string{"tail"}},
		// An opener line that closes itself is a paragraph, not the start
		// of a block that would run to the next "%%" line.
		{"%%a%% bee\ncee\n%%\n", []string{"bee", "cee"}},
		// Inside a container the closer may sit past the container's end.
		{"> %%\n> quoted\n\nz %%\n", []string{"quoted", "z %%"}},
	}
	for _, c := range cases {
		out := renderKind(t, c.src, RootVault)
		for _, want := range c.want {
			if !strings.Contains(out, want) {
				t.Errorf("%q: %q hidden: %s", c.src, want, out)
			}
		}
	}
}

func TestRenderVault_CommentMarkersInCodeKept(t *testing.T) {
	cases := map[string]string{
		"span":  "a `%%x%%` b",
		"fence": "```\n%%x%%\n%%\n```\n",
	}
	for name, src := range cases {
		out := renderKind(t, src, RootVault)
		if !strings.Contains(out, "%%x%%") {
			t.Errorf("%s: code lost its %%%% text: %s", name, out)
		}
	}
}

func TestRenderVault_TagChip(t *testing.T) {
	out := renderKind(t, "see #project/alpha now", RootVault)
	if !strings.Contains(out, `<span class="tag">#project/alpha</span>`) {
		t.Errorf("want a #project/alpha chip, got %s", out)
	}
}

func TestRenderVault_TagNotAChip(t *testing.T) {
	cases := map[string]string{
		"after a letter": "C# is a language",
		"mid-word":       "C#sharp and a#b",
		"dash only":      "a #- b",
		"slash only":     "a #/ b",
		"underscore":     "a #_ b",
		"digits only":    "issue #123 here",
		"code span":      "a `#tag` b",
		"fence":          "```\n#tag\n```\n",
		"autolink frag":  "see http://a.example/#frag now",
		"heading":        "# Heading",
		"frontmatter":    "---\ntags: \"#x\"\n---\n\nbody\n",
	}
	for name, src := range cases {
		out := renderKind(t, src, RootVault)
		if strings.Contains(out, `class="tag"`) {
			t.Errorf("%s: %q produced a chip: %s", name, src, out)
		}
	}
}

func TestRenderVault_TagCannotInjectHTML(t *testing.T) {
	for _, src := range []string{`#a"onmouseover=x`, "#<img src=x onerror=alert(1)>"} {
		out := renderKind(t, src, RootVault)
		for _, bad := range []string{"onmouseover=\"", "onmouseover=x\"", "onerror"} {
			if strings.Contains(out, bad) {
				t.Errorf("%q: output carries %q: %s", src, bad, out)
			}
		}
		if strings.Contains(out, `class="tag"`) && !strings.Contains(out, `<span class="tag">#a</span>`) {
			t.Errorf("%q: chip text is more than #a: %s", src, out)
		}
	}
}

func TestRenderVault_CalloutAliases(t *testing.T) {
	cases := []struct{ marker, tier, label string }{
		{"[!info]", "note", "Info"},
		{"[!abstract]", "note", "Abstract"},
		{"[!summary]-", "note", "Summary"},
		{"[!bug]", "danger", "Bug"},
		{"[!success]", "tip", "Success"},
		{"[!question]", "note", "Question"},
		{"[!Quote]", "note", "Quote"},
		{"[!NOTE]", "note", "Note"},
		{"[!caution]", "danger", "Caution"},
		{"[!attention]+", "warning", "Attention"},
	}
	for _, c := range cases {
		out := renderKind(t, "> "+c.marker+"\n> body\n", RootVault)
		if !strings.Contains(out, `<blockquote class="callout `+c.tier+`">`) {
			t.Errorf("%s: want tier %s, got %s", c.marker, c.tier, out)
		}
		if !strings.Contains(out, "</svg> "+c.label+"</div>") {
			t.Errorf("%s: want label %s, got %s", c.marker, c.label, out)
		}
		// The marker, fold sign included, must not leak into the body.
		if !strings.Contains(out, "</div><p>body") {
			t.Errorf("%s: marker text left in the body: %s", c.marker, out)
		}
	}
}

func TestRenderVault_UnknownCalloutStaysBlockquote(t *testing.T) {
	out := renderKind(t, "> [!bogus]\n> body\n", RootVault)
	if strings.Contains(out, "callout") || !strings.Contains(out, "[!bogus]") {
		t.Errorf("unknown callout type was transformed: %s", out)
	}
}

// TestRenderDocs_LowercaseCalloutUnchanged pins today's docs-root rule: only
// GFM's six uppercase kinds are callouts there.
func TestRenderDocs_LowercaseCalloutUnchanged(t *testing.T) {
	for _, src := range []string{"> [!info]\n> body\n", "> [!note]\n> body\n"} {
		out := renderKind(t, src, RootDocs)
		if strings.Contains(out, "callout") {
			t.Errorf("%q became a callout in a docs root: %s", src, out)
		}
	}
	out := renderKind(t, "> [!NOTE]\n> body\n", RootDocs)
	if !strings.Contains(out, `<blockquote class="callout note">`) {
		t.Errorf("docs-root [!NOTE] lost its callout: %s", out)
	}
}

func TestRenderVault_HeadingSlugUnaffectedByInlineFlavor(t *testing.T) {
	const src = "## Plan ==now== #tag\n"
	headings, _, _, err := scanBody([]byte(src))
	if err != nil || len(headings) != 1 {
		t.Fatalf("scanBody: %v, %v", headings, err)
	}
	out := renderKind(t, src, RootVault)
	if !strings.Contains(out, `<h2 id="`+headings[0].Slug+`">`) {
		t.Errorf("rendered id differs from the scanned slug %q: %s", headings[0].Slug, out)
	}
}

func TestRenderVault_FlavorMarkupSurvivesSanitizer(t *testing.T) {
	out := renderKind(t, "==m== #t\n\n<mark onclick=\"x\">raw</mark>\n", RootVault)
	for _, want := range []string{"<mark>m</mark>", `<span class="tag">#t</span>`, "<mark>raw</mark>"} {
		if !strings.Contains(out, want) {
			t.Errorf("want %s after sanitizing, got %s", want, out)
		}
	}
	if strings.Contains(out, "onclick") {
		t.Errorf("raw <mark> kept its onclick: %s", out)
	}
}

// TestRenderVault_CommentAroundBlockStaysVisible pins keep-when-unsure for a
// "%%" that may sit inside a fence or HTML block: closing on it would orphan
// the block's own closer, and an orphaned fence swallows the rest of the
// document into a code block.
func TestRenderVault_CommentAroundBlockStaysVisible(t *testing.T) {
	cases := []struct {
		name, src string
		want      []string
	}{
		{"backtick fence", "%%\n```md\n%%\n```\n\n# Next\n\nbody", []string{`<h1 id="next">Next</h1>`, "<p>body</p>"}},
		{"tilde fence", "%%\n~~~\n%%\n~~~\n\n# Next\n\nbody", []string{`<h1 id="next">Next</h1>`, "<p>body</p>"}},
		{"html block", "%%\n<pre>\nkeep\n%%\n</pre>\n\nafter", []string{"keep", "after"}},
		{"escaped backtick before a %%", "p %%x \\`%% visible text `%%", []string{"visible text"}},
		{"escaped backtick then %%", "\\`%%", []string{"`%%"}},
		// goldmark's escape makes the first '%' literal, and the '%' after
		// it cannot open a run of exactly two, so nothing pairs.
		{"escaped opener", "\\%%a%%", []string{"%%a%%"}},
		// A run of three is not a marker, and its tail is not re-read as
		// a run of two.
		{"run of three", "a %%%x%% b", []string{"%%%x%%"}},
	}
	for _, c := range cases {
		out := renderKind(t, c.src, RootVault)
		for _, want := range c.want {
			if !strings.Contains(out, want) {
				t.Errorf("%s: want %q visible, got %s", c.name, want, out)
			}
		}
	}
}

// TestRenderVault_LongEqualsRunIsLinear guards the highlight parser's cost
// on a long '=' run: re-scanning the run from every '=' is quadratic, which
// at this length takes seconds rather than milliseconds.
func TestRenderVault_LongEqualsRunIsLinear(t *testing.T) {
	for _, c := range []string{"=", "%"} {
		src := "x " + strings.Repeat(c, 80000)
		start := time.Now()
		out := renderKind(t, src, RootVault)
		if d := time.Since(start); d > time.Second {
			t.Errorf("rendering an 80000-byte %q run took %v", c, d)
		}
		if strings.Contains(out, "<mark>") || !strings.Contains(out, strings.Repeat(c, 100)) {
			t.Errorf("a %q run did not stay literal", c)
		}
	}
}

// TestRenderVault_CalloutFoldMissLeftAlone: (?i) matches U+017F (long s)
// as 's', but ToLower leaves it alone, so the map lookup misses. The
// blockquote must come through unchanged, not as an empty-tier callout.
func TestRenderVault_CalloutFoldMissLeftAlone(t *testing.T) {
	out := renderKind(t, "> [!ſummary]\n> body\n", RootVault)
	if strings.Contains(out, "callout") || !strings.Contains(out, "[!ſummary]") {
		t.Errorf("a callout-map miss was transformed: %s", out)
	}
}

// TestScanVault_SlugsMatchRenderedIDs: a heading inside a %% block is
// neither rendered nor indexed, so goldmark's duplicate-slug suffixes agree
// between the scan and the page.
func TestScanVault_SlugsMatchRenderedIDs(t *testing.T) {
	const src = "## A\n\n%%\n## A\n%%\n\n## A\n\n## Plan %%secret%%\n"
	scan, err := scanBodyFor(RootVault, []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	var slugs []string
	for _, h := range scan.headings {
		slugs = append(slugs, h.Slug)
	}
	out := renderKind(t, src, RootVault)
	ids := regexp.MustCompile(`<h2 id="([^"]+)"`).FindAllStringSubmatch(out, -1)
	var rendered []string
	for _, m := range ids {
		rendered = append(rendered, m[1])
	}
	want := []string{"a", "a-1", "plan"}
	if strings.Join(slugs, ",") != strings.Join(want, ",") || strings.Join(rendered, ",") != strings.Join(want, ",") {
		t.Errorf("scan slugs %v, rendered ids %v, want both %v", slugs, rendered, want)
	}
}

// TestScanVault_CommentsNotIndexed: links, headings and block ids inside a
// comment stay out of a vault doc's index entry, and the title skips both a
// commented-out H1 and a same-line comment span.
func TestScanVault_CommentsNotIndexed(t *testing.T) {
	const src = "%%\n# Secret title\n[[hidden]] [x](gone.md)\n## Hidden heading\nq ^hid\n%%\n\n# Meeting %%private%%\n\n[[shown]] %%[[inline]]%%\n\npara ^blk\n\na %%x\ny ^inl\nz%% b\n"
	p := filepath.Join(t.TempDir(), "n.md")
	if err := os.WriteFile(p, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	meta, err := scanDocFor(RootVault, p, "n.md")
	if err != nil {
		t.Fatal(err)
	}
	if meta.Title != "Meeting" {
		t.Errorf("title = %q, want Meeting", meta.Title)
	}
	var paths []string
	for _, l := range meta.Links {
		paths = append(paths, l.Path)
	}
	if strings.Join(paths, ",") != "shown" {
		t.Errorf("links = %v, want only shown", paths)
	}
	for _, h := range meta.Headings {
		if strings.Contains(h.Text, "Hidden") || strings.Contains(h.Text, "Secret") || strings.Contains(h.Slug, "private") || strings.Contains(h.Text, "private") {
			t.Errorf("comment reached a heading: %+v", h)
		}
	}
	if strings.Join(meta.BlockIDs, ",") != "blk" {
		t.Errorf("block ids = %v, want only blk", meta.BlockIDs)
	}
}

// TestRenderVault_CommentsGoldmarkPairs: "%%" is a goldmark delimiter, so
// the parse decides every boundary. A closed code span inside a comment is
// hidden with it, a "%%" inside a code span is not the closer, a backtick
// with no closer is literal text inside the comment, and spaced, link-text
// and heading comments all pair.
func TestRenderVault_CommentsGoldmarkPairs(t *testing.T) {
	hidden := []struct{ name, src, gone, kept string }{
		{"inline", "keep %%TODO rename `foo` later%% keep", "foo", "keep"},
		{"comment-only paragraph", "%%a `bee` c%%\n\nafter", "bee", "after"},
		{"percent inside code", "x %%a `%%` bee%% y", "bee", "y"},
		{"unclosed backtick", "x %%a `bee%% y", "bee", "y"},
		{"spaced", "%% spaced bee %%\n\nafter", "bee", "after"},
		{"link text", "a [see %%bee%% it](x.md) e", "bee", `<a href="x.md" rel="nofollow">see  it</a>`},
		{"heading", "## Head %%bee%% end\n", "bee", ">Head  end</h2>"},
		{"across a soft break", "a %%bee\nbee%% d", "bee", "d"},
	}
	for _, c := range hidden {
		out := renderKind(t, c.src, RootVault)
		if strings.Contains(out, c.gone) || strings.Contains(out, "%%") || !strings.Contains(out, c.kept) {
			t.Errorf("%s: want %q hidden and %q kept, got %s", c.name, c.gone, c.kept, out)
		}
	}
	out := renderKind(t, "## Code %%a `bee` c%%\n", RootVault)
	if !strings.Contains(out, `<h2 id="code">`) {
		t.Errorf("heading id carries comment text: %s", out)
	}
	// A paragraph holding only a comment leaves no empty <p> behind.
	out = renderKind(t, "%%only%%\n\nafter", RootVault)
	if strings.Contains(out, "<p></p>") || strings.Contains(out, "only") {
		t.Errorf("comment-only paragraph left markup or text: %s", out)
	}
}

// TestScanVault_HeadingIDParity checks, exhaustively over every string of
// length 1-5 made of '%', '`', '\\', 'a', ' ', '=', '~', '#', '[' and '$',
// that a "## " heading's
// scanned slug equals its rendered id: the heading-id transformer runs in
// both instances, and this pins that nothing else feeds either side.
func TestScanVault_HeadingIDParity(t *testing.T) {
	alphabet := []byte("%`\\a =~#[$")
	idPattern := regexp.MustCompile(`<h2 id="([^"]*)"`)
	checked := 0
	var walk func(prefix []byte)
	walk = func(prefix []byte) {
		if len(prefix) > 0 {
			src := "## " + string(prefix) + "\n"
			scan, err := scanBodyFor(RootVault, []byte(src))
			if err != nil || len(scan.headings) != 1 {
				t.Fatalf("%q: scan gave %v, %v", src, scan.headings, err)
			}
			out := renderKind(t, src, RootVault)
			m := idPattern.FindStringSubmatch(out)
			if m == nil || m[1] != scan.headings[0].Slug {
				t.Fatalf("%q: scan slug %q, rendered %s", src, scan.headings[0].Slug, out)
			}
			checked++
		}
		if len(prefix) == 5 {
			return
		}
		for _, c := range alphabet {
			walk(append(prefix, c))
		}
	}
	walk(nil)
	if checked < 100000 {
		t.Fatalf("only %d strings checked", checked)
	}
}

// TestScanVault_TableCellLinksMatchRender: in a GFM table each cell is
// parsed on its own, so a "%%" cannot reach across a '|', and an autolink
// consumes the "%%" inside it. The vault scan must draw the same boundaries:
// a link is indexed exactly when it renders.
func TestScanVault_TableCellLinksMatchRender(t *testing.T) {
	const src = "| h1 | h2 |\n|---|---|\n| a %%x | [see](Other.md) y%% |\n| %%c [gone](Gone.md) d%% | e |\n\nsee www.a.example/%%x [live](Live.md) y%%\n"
	scan, err := scanBodyFor(RootVault, []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	indexed := map[string]bool{}
	for _, l := range scan.links {
		indexed[l.Path] = true
	}
	out := renderKind(t, src, RootVault)
	for _, target := range []string{"Other.md", "Gone.md", "Live.md"} {
		rendered := strings.Contains(out, `href="`+target+`"`)
		if indexed[target] != rendered {
			t.Errorf("%s: indexed = %v, rendered = %v (%s)", target, indexed[target], rendered, out)
		}
	}
	if !indexed["Other.md"] || !indexed["Live.md"] {
		t.Errorf("fixture no longer exercises a live link beside %%%%: %v", scan.links)
	}
}

// TestScanVault_OverCapTitleDropsComment: past the scan cap there is no
// whole-document parse, so the title line is parsed on its own.
func TestScanVault_OverCapTitleDropsComment(t *testing.T) {
	p := filepath.Join(t.TempDir(), "big.md")
	src := "# Big %%secret%% note\n\n" + strings.Repeat("word ", maxScanBytes/5+10)
	if err := os.WriteFile(p, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	meta, err := scanDocFor(RootVault, p, "big.md")
	if err != nil {
		t.Fatal(err)
	}
	if meta.Title != "Big  note" {
		t.Errorf("title = %q, want %q", meta.Title, "Big  note")
	}
}

// TestScanVault_ParserSetParity: the vault scan and render parse with one
// constructor, so every inline construct that can hold or cross a "%%"
// (==, ~~, _, #tag, [[…]], $…$) pairs it the same way in both. Each case
// checks scan slugs against rendered ids, and that the indexed link set
// equals the set the page shows (renderedLinks) and the case's expected set.
func TestScanVault_ParserSetParity(t *testing.T) {
	cases := []struct {
		src   string
		links []string
	}{
		{"## ==a %%b== c%%\n", nil},
		{"## ~~a %%b~~ c%%\n", nil},
		{"## _x %%y #t_ z%%\n", nil},
		{"## a %%b [[c%%]] d\n", []string{"c%%"}},
		{"## $a %%$ b %%\n", nil},
		{"==a %%b== c%% d [[Hidden]] %%\n", nil},
		{"~~a %%b [Target](target.md)~~ c%%\n", []string{"target.md"}},
		{"$x %% y$ z %% [[L]]\n", []string{"L"}},
		{"#t %%x [[T1]]%% [[T2]] ==y %%z== [w](w.md)%%\n", []string{"T2", "w.md"}},
		{"## [[H1]] %%x [[H2]] ~~y%%~~ #t %%\n", []string{"H1"}},
	}
	idPattern := regexp.MustCompile(`<h[1-6] id="([^"]*)"`)
	for _, c := range cases {
		scan, err := scanBodyFor(RootVault, []byte(c.src))
		if err != nil {
			t.Fatal(err)
		}
		out := renderKind(t, c.src, RootVault)
		var slugs, ids []string
		for _, h := range scan.headings {
			slugs = append(slugs, h.Slug)
		}
		for _, m := range idPattern.FindAllStringSubmatch(out, -1) {
			ids = append(ids, m[1])
		}
		if strings.Join(slugs, ",") != strings.Join(ids, ",") {
			t.Errorf("%q: scan slugs %v, rendered ids %v", c.src, slugs, ids)
		}
		var indexed []string
		for _, l := range scan.links {
			indexed = append(indexed, l.Path)
		}
		rendered := renderedLinks(t, c.src, out)
		sort.Strings(indexed)
		want := append([]string(nil), c.links...)
		sort.Strings(want)
		if strings.Join(indexed, ",") != strings.Join(rendered, ",") || strings.Join(rendered, ",") != strings.Join(want, ",") {
			t.Errorf("%q: indexed %v, rendered %v, want %v (%s)", c.src, indexed, rendered, want, out)
		}
	}
}

// renderedLinks returns, sorted, the link targets a vault page shows: each
// markdown link's href from the rendered HTML, plus each wikilink the
// render pipeline's own parse keeps outside a comment (a comment's subtree
// never renders).
func renderedLinks(t *testing.T, src, out string) []string {
	t.Helper()
	var links []string
	for _, m := range regexp.MustCompile(`href="([^"]*)"`).FindAllStringSubmatch(out, -1) {
		links = append(links, m[1])
	}
	source := []byte(src)
	doc := markdownVaultPlain.Parser().Parse(text.NewReader(source))
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		if n.Kind() == kindCommentSpan {
			return ast.WalkSkipChildren, nil
		}
		if wl, ok := n.(*wikilink.Node); ok {
			links = append(links, wikilinkRef(wl, source).Path)
		}
		return ast.WalkContinue, nil
	})
	sort.Strings(links)
	return links
}

// TestRenderVault_WikilinkShowsSource: a vault wikilink rendered without a
// resolver is parsed (so the page and the index agree on what it consumes)
// and shows as its own escaped source text, never as a link.
func TestRenderVault_WikilinkShowsSource(t *testing.T) {
	out := renderKind(t, "see [[My Note#Part|label]] and ![[pic.png]] and [[a<b]]\n", RootVault)
	for _, want := range []string{"[[My Note#Part|label]]", "![[pic.png]]", "[[a&lt;b]]"} {
		if !strings.Contains(out, want) {
			t.Errorf("want %q in %s", want, out)
		}
	}
	if strings.Contains(out, "<a ") || strings.Contains(out, "<img") {
		t.Errorf("a wikilink became markup: %s", out)
	}
	// Rendered with no resolver, a link fails closed as an unresolved miss;
	// an embed stays bare source.
	if !strings.Contains(out, `<span class="wikilink wikilink-miss" title="Broken link (unresolved)">[[My Note#Part|label]]</span>`) {
		t.Errorf("an unresolved wikilink is not marked as a miss: %s", out)
	}
}

// TestScanVault_BlockIDInRemovedCommentParagraph: a paragraph holding only
// a comment is removed from the tree, and a block-id marker inside that
// comment must stay out of the index.
func TestScanVault_BlockIDInRemovedCommentParagraph(t *testing.T) {
	for _, src := range []string{
		"> %%x\n> text ^blk\n> more%%\n",
		"- a\n\n  %%x\n  text ^blk2\n  more%%\n",
		"%%x\n<b>\ntext ^blk3\n%%\n",
		"para ^keep\n",
		// Mermaid and math fences are code, though the render pipeline's
		// transformers give them their own node kinds.
		"```mermaid\nA ^mer\n```\n\n```math\nx ^mth\n```\n\n$$\ny ^dd\n$$\n",
	} {
		scan, err := scanBodyFor(RootVault, []byte(src))
		if err != nil {
			t.Fatal(err)
		}
		ids := scanBlockIDs([]byte(src), scan.masked, scan.hidden)
		want := ""
		if strings.Contains(src, "^keep") {
			want = "keep"
		}
		if strings.Join(ids, ",") != want {
			t.Errorf("%q: block ids %v, want %q", src, ids, want)
		}
	}
}

// TestResolveVault_HeadingMatchNormalized: a vault heading link matches
// when the fragment, as written or as rendered, and the heading's rendered
// Text agree under foldHeadingKey, so a link written with the heading's
// markup or without it resolves. Comment text is in neither.
func TestResolveVault_HeadingMatchNormalized(t *testing.T) {
	vault := t.TempDir()
	if err := os.MkdirAll(filepath.Join(vault, ".obsidian"), 0o750); err != nil {
		t.Fatal(err)
	}
	const note = "# Note\n\n## a ==b==\n\n## e *f*\n\n## use `foo()`\n\n## see [[Other]]\n\n" +
		"## [d](e.md) g\n\n## c ~~d~~\n\n## $`q`$\n\n## x %%c%% y\n\n## see #tag\n"
	for name, body := range map[string]string{"Note.md": note, "Linker.md": "# Linker\n", "Other.md": "# Other\n"} {
		if err := os.WriteFile(filepath.Join(vault, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	idx, err := NewIndex([]string{vault})
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	label := idx.Roots()[0].Label
	from, ok := idx.Find(label, "Linker.md")
	if !ok {
		t.Fatal("Linker.md not indexed")
	}
	// "[[Note#see #tag]]" is not in the list: matchFragment reads every
	// '#' in a fragment as Obsidian's nested-heading separator, so that link
	// names a heading "tag" under "see ". The heading resolves by its slug.
	for _, target := range []string{
		"Note#a ==b==", "Note#a b", "Note#e f", "Note#e *f*", "Note#use foo()", "Note#use `foo()`",
		"Note#see Other", "Note#see [[Other]]", "Note#d g", "Note#c ~~d~~", "Note#c d",
		"Note#$`q`$", "Note#x y", "Note#X  Y", "Note#see-tag",
	} {
		if _, miss := idx.ResolveLink(&from, target); miss != MissNone {
			t.Errorf("[[%s]]: miss %v", target, miss)
		}
	}
	if _, miss := idx.ResolveLink(&from, "Note#x c y"); miss == MissNone {
		t.Errorf("[[Note#x c y]] resolved: comment text reached the heading")
	}
	doc, _ := idx.Find(label, "Note.md")
	for _, h := range doc.Headings {
		if strings.Contains(h.Text, "%%") || strings.Contains(h.Text, "c y") {
			t.Errorf("comment reached heading text %q", h.Text)
		}
	}
}

// TestResolveVault_HeadingMatchKeepsLiterals: vault heading matching folds
// only case and whitespace, so a literal "_ * = ~ |" inside a heading's
// text must be matched as written, while a backslash escape or an entity
// matches the character it renders as. (A fragment equal to the heading's
// slug still matches by slug, so each heading here has a slug no dropped
// character can reach.) A heading that renders empty is reached by no text.
func TestResolveVault_HeadingMatchKeepsLiterals(t *testing.T) {
	vault := t.TempDir()
	if err := os.MkdirAll(filepath.Join(vault, ".obsidian"), 0o750); err != nil {
		t.Fatal(err)
	}
	const note = "# Note\n\n## snake_case\n\n## 1_000\n\n## 2*3 n\n\n## a = b\n\n## x\\|y z\n\n## foo\\_bar\n\n## Q &amp; A\n\n## %%hidden%%\n"
	for name, body := range map[string]string{"Note.md": note, "Linker.md": "# Linker\n"} {
		if err := os.WriteFile(filepath.Join(vault, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	idx, err := NewIndex([]string{vault})
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	from, ok := idx.Find(idx.Roots()[0].Label, "Linker.md")
	if !ok {
		t.Fatal("Linker.md not indexed")
	}
	for _, target := range []string{"Note#snakecase", "Note#1000", "Note#23 n", "Note#a  b", "Note#xy z", "Note#%%c%%"} {
		if _, miss := idx.ResolveLink(&from, target); miss == MissNone {
			t.Errorf("[[%s]] resolved to a heading its text does not name", target)
		}
	}
	for _, target := range []string{
		"Note#Snake_Case", "Note#1_000", "Note#2*3 n", "Note#a = b", "Note#x|y z",
		"Note#foo_bar", "Note#foo\\_bar", "Note#q & a", "Note#Q &amp; A",
	} {
		if _, miss := idx.ResolveLink(&from, target); miss != MissNone {
			t.Errorf("[[%s]]: miss %v", target, miss)
		}
	}
}

// newMatchVault indexes a vault holding Note.md (note) and an empty
// Linker.md, and returns the index with Linker.md to resolve from.
func newMatchVault(t *testing.T, note string) (*Index, Doc) {
	t.Helper()
	vault := t.TempDir()
	if err := os.MkdirAll(filepath.Join(vault, ".obsidian"), 0o750); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"Note.md": note, "Linker.md": "# Linker\n"} {
		if err := os.WriteFile(filepath.Join(vault, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	idx, err := NewIndex([]string{vault})
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	from, ok := idx.Find(idx.Roots()[0].Label, "Linker.md")
	if !ok {
		t.Fatal("Linker.md not indexed")
	}
	return idx, from
}

// TestResolveVault_HeadingMatchRenderedFragment pins both match paths. A
// fragment written with markup reaches the heading its rendered text names;
// an escaped-markup heading is reached by the fragment as written, which
// renders differently; a fragment that renders to nothing reaches nothing;
// and a fragment over maxRenderedFragment is never parsed.
func TestResolveVault_HeadingMatchRenderedFragment(t *testing.T) {
	idx, from := newMatchVault(t, "# Note\n\n## x\n\n## a \\*b\\*\n\n## %%all comment%%\n")
	for _, target := range []string{"Note#[x](y)", "Note#`x`", "Note#%%c%% x", "Note#a *b*"} {
		if _, miss := idx.ResolveLink(&from, target); miss != MissNone {
			t.Errorf("[[%s]]: miss %v", target, miss)
		}
	}
	long := "Note#**x**" + strings.Repeat(" ", maxRenderedFragment)
	for _, target := range []string{"Note#<b>", "Note#%%c%%", long} {
		if _, miss := idx.ResolveLink(&from, target); miss == MissNone {
			t.Errorf("[[%.40q]] resolved", target)
		}
	}
	if _, miss := idx.ResolveLink(&from, "Note#**x**"+strings.Repeat(" ", maxRenderedFragment-len("**x**"))); miss != MissNone {
		t.Errorf("a fragment at the cap was not rendered: miss %v", miss)
	}
}

// TestScanVault_HeadingTextRendered: Heading.Text is the text the page
// shows: an entity resolved, an escape dropped, a code span kept verbatim,
// and an autolink or bare URL showing its label.
func TestScanVault_HeadingTextRendered(t *testing.T) {
	for src, want := range map[string]string{
		"## Q &amp; A\n":              "Q & A",
		"## a \\*b\\*\n":              "a *b*",
		"## `a\\_b &amp;`\n":          "a\\_b &amp;",
		"## see <https://x.io/a>\n":   "see https://x.io/a",
		"## see https://x.io/a now\n": "see https://x.io/a now",
		"## mail a@b.io\n":            "mail a@b.io",
	} {
		scan, err := scanBodyFor(RootVault, []byte(src))
		if err != nil || len(scan.headings) != 1 {
			t.Fatalf("%q: %v, %v", src, scan.headings, err)
		}
		if got := scan.headings[0].Text; got != want {
			t.Errorf("%q: Text %q, want %q", src, got, want)
		}
	}
}

// TestFragmentText_MarkupFreeRendersAsWritten backs matchFragment's fast
// path: a fragment holding none of fragmentMarkupBytes renders as written,
// so skipping its parse loses no match.
func TestFragmentText_MarkupFreeRendersAsWritten(t *testing.T) {
	for _, f := range []string{
		"Step 1: Install", "see https://x.io/a now", "www.x.io", "mail a@b.io",
		"a + b - c", "1. first", "> quote", "(parens) {braces} 'q' \"dq\"", "C#", "x ^blk", "  spaced   out ",
	} {
		if strings.ContainsAny(f, fragmentMarkupBytes) {
			t.Fatalf("%q holds a markup byte", f)
		}
		if got := foldHeadingKey(fragmentText(f)); got != foldHeadingKey(f) {
			t.Errorf("%q renders as %q", f, got)
		}
	}
}

// TestResolveDocs_HeadingMatchNotNormalized pins the docs-root rule: an
// anchor matches the exact slug only, so a fragment that would match under
// the vault's foldHeadingKey matching still misses.
func TestResolveDocs_HeadingMatchNotNormalized(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{"Note.md": "# Note\n\n## a ==b==\n", "Linker.md": "# Linker\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	idx, err := NewIndexWithOptions([]string{dir}, IndexOptions{RootKinds: map[string]RootKind{dir: RootDocs}})
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	if idx.Roots()[0].Kind != RootDocs {
		t.Fatal("fixture is not a docs root")
	}
	from, _ := idx.Find(idx.Roots()[0].Label, "Linker.md")
	if _, miss := idx.ResolveLink(&from, "Note.md#a b"); miss == MissNone {
		t.Errorf("docs root resolved a normalized heading fragment")
	}
	if _, miss := idx.ResolveLink(&from, "Note.md#a-b"); miss != MissNone {
		t.Errorf("docs root lost its exact-slug match: %v", miss)
	}
}

// TestScanVault_TitleUnchanged pins the vault title against 02d1145's rule:
// the first H1's first line, as written, with comment ranges cut and the
// ends trimmed.
func TestScanVault_TitleUnchanged(t *testing.T) {
	p := filepath.Join(t.TempDir(), "n.md")
	if err := os.WriteFile(p, []byte("# Title ==x== *y* %%c%% end\n\nbody\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	meta, err := scanDocFor(RootVault, p, "n.md")
	if err != nil {
		t.Fatal(err)
	}
	if want := "Title ==x== *y*  end"; meta.Title != want {
		t.Errorf("title %q, want %q", meta.Title, want)
	}
}

// TestScan_LongSetextHeadingIsLinear: a 5000-line setext H1 must scan in
// time proportional to its size, in both root kinds. The bound is relative
// to a same-size document with no heading, best of several runs, so a slow
// CI machine slows both sides alike; a per-line re-walk of the heading is
// about a hundred times the baseline.
func TestScan_LongSetextHeadingIsLinear(t *testing.T) {
	heading := []byte(strings.Repeat("word line\n", 5000) + "===\n")
	plain := []byte(strings.Repeat("word line\n", 5000))
	best := func(kind RootKind, src []byte) time.Duration {
		fastest := time.Duration(1<<63 - 1)
		for range 5 {
			start := time.Now()
			if _, err := scanBodyFor(kind, src); err != nil {
				t.Fatal(err)
			}
			if d := time.Since(start); d < fastest {
				fastest = d
			}
		}
		return fastest
	}
	for _, kind := range []RootKind{RootDocs, RootVault} {
		base, got := best(kind, plain), best(kind, heading)
		if got > 10*base+20*time.Millisecond {
			t.Errorf("kind %v: setext heading scan %v against a %v baseline", kind, got, base)
		}
	}
}

// TestScanDocs_HeadingTextUnchanged pins the docs-root Text rule: inline
// nodes flattened to their text, as before vault roots got source text.
func TestScanDocs_HeadingTextUnchanged(t *testing.T) {
	headings, _, _, err := scanBody([]byte("## a *b* `c` [d](e.md) ==f== #g\n"))
	if err != nil || len(headings) != 1 {
		t.Fatalf("scanBody: %v, %v", headings, err)
	}
	if want := "a b c d ==f== #g"; headings[0].Text != want {
		t.Errorf("docs heading text %q, want %q", headings[0].Text, want)
	}
}

// TestRenderVault_PercentInsideWikilinkIsNotACloser: a wikilink is
// consumed before its '%' is seen, as a code span is, so a "%%" inside
// "[[…]]" neither opens nor closes a comment.
func TestRenderVault_PercentInsideWikilinkIsNotACloser(t *testing.T) {
	out := renderKind(t, "%% s [[x %% y]] z\n", RootVault)
	for _, want := range []string{"%% s", "[[x %% y]]", "z"} {
		if !strings.Contains(out, want) {
			t.Errorf("want %q visible, got %s", want, out)
		}
	}
}

// TestRenderVault_WordsSkipComments: a vault page's reading estimate counts
// only what the page shows. Inline comments, a block comment (opener and
// closer lines included), and a line holding only a comment all drop out;
// a docs root, which renders %% as text, still counts it.
func TestRenderVault_WordsSkipComments(t *testing.T) {
	const src = "---\ntitle: x y z\n---\n# Title %%one two%%\n\nshown%%glued%%word %%three%%\n\n%%\nfour five six\n%%\n\n%%seven eight%%\n\nlast\n"
	vault, err := RenderDocFor(RootVault, []byte(src), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	// "#", "Title", "shownword", "last".
	if vault.Words != 4 {
		t.Errorf("vault words = %d, want 4", vault.Words)
	}
	docs, err := RenderDocFor(RootDocs, []byte(src), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := countWords([]byte(src)); docs.Words != want {
		t.Errorf("docs words = %d, want the raw count %d", docs.Words, want)
	}
}
