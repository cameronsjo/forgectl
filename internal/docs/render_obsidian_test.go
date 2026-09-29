package docs

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
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
		// The first "%%" after the opener has text behind it: Obsidian
		// would show that text, so the block declines rather than hide it.
		{"%%\nh\nx %% shown\n\nz", []string{"h", "shown", "z"}},
		// A one-line block with text after its closer: only the
		// delimited part may go.
		{"%%gone%% tail", []string{"tail"}},
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
		{"inline code span", "a %%b `c%%` d%% e", []string{"<code>c%%</code>", "b ", " e"}},
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
	src := "x " + strings.Repeat("=", 80000)
	start := time.Now()
	out := renderKind(t, src, RootVault)
	if d := time.Since(start); d > time.Second {
		t.Errorf("rendering an 80000-byte '=' run took %v", d)
	}
	if strings.Contains(out, "<mark>") {
		t.Errorf("an '=' run became a highlight")
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
	const src = "%%\n# Secret title\n[[hidden]] [x](gone.md)\n## Hidden heading\nq ^hid\n%%\n\n# Meeting %%private%%\n\n[[shown]] %%[[inline]]%%\n\npara ^blk\n"
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
		if strings.Contains(h.Text, "Hidden") || strings.Contains(h.Text, "Secret") || strings.Contains(h.Slug, "private") {
			t.Errorf("comment reached a heading: %+v", h)
		}
	}
	if strings.Join(meta.BlockIDs, ",") != "blk" {
		t.Errorf("block ids = %v, want only blk", meta.BlockIDs)
	}
}
