package docs

import (
	"strings"
	"testing"
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
