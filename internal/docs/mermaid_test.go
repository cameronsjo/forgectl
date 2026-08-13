package docs

// Test plan for mermaid.go (and render.go's inline-SVG allowlist)
//
// mermaid fence transform (Classification: core logic)
//   [x] Happy: a ```mermaid fence renders as <pre class="mermaid">
//   [x] Happy: the diagram source survives verbatim through the entities
//              (mermaid.js reads textContent, so --> must round-trip)
//   [x] Happy: a NON-mermaid fence keeps its chroma highlighting untouched
//   [x] Happy: two mermaid fences in one document both convert (the transform
//              collects before mutating, so neither is skipped)
//   [x] Happy: an info string of "mermaid" plus extra words still matches
//   [x] Unhappy: a look-alike info string ("mermaidjs", "Mermaid") does NOT
//              convert — it stays an ordinary code block
//   [x] Unhappy (security): a diagram label containing markup cannot break out
//              of the <pre> (the escaping is what prevents this, not the
//              downstream sanitizer)
//
// inline SVG allowlist (Classification: security policy)
//   [x] Happy: a plain inline <svg> with viewBox and shapes survives
//   [x] Happy: viewBox survives specifically — pan/zoom is impossible without it
//   [x] Happy: Artificer diagram classes (.dia-node, .dia-edge) survive
//   [x] Unhappy (security): <svg onload=…> loses the handler
//   [x] Unhappy (security): <svg><script> loses the script
//   [x] Unhappy (security): <foreignObject> is stripped
//   [x] Unhappy (security): <animate> is stripped
//   [x] Unhappy (security): <image>, <use>, and an SVG <a> are stripped
//   [x] Unhappy (security): an off-document url() in fill is dropped
//   [x] Unhappy (security): a javascript: URI in a paint attribute is dropped
//   [x] Unhappy (security): the style ATTRIBUTE is not allowed through

import (
	"strings"
	"testing"
)

func renderOrFail(t *testing.T, src string) string {
	t.Helper()
	out, err := Render([]byte(src))
	if err != nil {
		t.Fatalf("Render(%q): %v", src, err)
	}
	return out
}

func TestRender_MermaidFence_BecomesMermaidPre(t *testing.T) {
	out := renderOrFail(t, "```mermaid\ngraph LR\n  A --> B\n```\n")

	if !strings.Contains(out, `<pre class="mermaid">`) {
		t.Errorf("output missing <pre class=\"mermaid\">: %s", out)
	}
	if strings.Contains(out, `language-mermaid`) {
		t.Errorf("output still carries the code-block class, so the fence was not promoted: %s", out)
	}
}

func TestRender_MermaidFence_DiagramSourceRoundTrips(t *testing.T) {
	out := renderOrFail(t, "```mermaid\ngraph LR\n  A[Start] --> B{Choice}\n```\n")

	// mermaid.js reads textContent, which decodes entities — so the arrow must
	// be present as an escaped form that decodes back to "-->", not mangled or
	// dropped.
	if !strings.Contains(out, "--&gt;") {
		t.Errorf("diagram arrow did not survive as an entity; mermaid would receive broken source: %s", out)
	}
	for _, want := range []string{"graph LR", "A[Start]", "B{Choice}"} {
		if !strings.Contains(out, want) {
			t.Errorf("diagram source missing %q: %s", want, out)
		}
	}
}

func TestRender_NonMermaidFence_KeepsHighlighting(t *testing.T) {
	out := renderOrFail(t, "```go\nfunc main() {}\n```\n")

	if strings.Contains(out, `class="mermaid"`) {
		t.Errorf("a go fence was wrongly promoted to a mermaid block: %s", out)
	}
	// chroma emits class-based token spans for a language it knows.
	if !strings.Contains(out, "<span") {
		t.Errorf("go fence lost its chroma highlighting — the mermaid renderer is claiming fences it should not: %s", out)
	}
}

func TestRender_TwoMermaidFences_BothConvert(t *testing.T) {
	out := renderOrFail(t, "```mermaid\ngraph LR\nA-->B\n```\n\ntext\n\n```mermaid\ngraph TD\nC-->D\n```\n")

	if got := strings.Count(out, `<pre class="mermaid">`); got != 2 {
		t.Errorf("mermaid block count = %d, want 2 — replacing a node mid-walk skips siblings: %s", got, out)
	}
}

func TestRender_MermaidInfoWithExtraWords_Converts(t *testing.T) {
	out := renderOrFail(t, "```mermaid title=\"flow\"\ngraph LR\nA-->B\n```\n")

	if !strings.Contains(out, `<pre class="mermaid">`) {
		t.Errorf("a mermaid fence with a trailing info word did not convert: %s", out)
	}
}

func TestRender_MermaidLookAlikeInfo_DoesNotConvert(t *testing.T) {
	for _, info := range []string{"mermaidjs", "Mermaid", "MERMAID", "mermaid-diagram"} {
		out := renderOrFail(t, "```"+info+"\ngraph LR\nA-->B\n```\n")
		if strings.Contains(out, `<pre class="mermaid">`) {
			t.Errorf("info string %q was treated as mermaid; the match must be exact: %s", info, out)
		}
	}
}

func TestRender_MermaidLabelWithMarkup_CannotBreakOut(t *testing.T) {
	// A node label carrying a closing tag plus a script. The renderer's escaping
	// is the control being tested here — not bluemonday, which runs afterwards.
	out := renderOrFail(t, "```mermaid\ngraph LR\n  A[</pre><script>alert(1)</script>] --> B\n```\n")

	if strings.Contains(out, "<script") {
		t.Errorf("a mermaid label injected a live script element: %s", out)
	}
	if strings.Contains(out, "</pre><") {
		t.Errorf("a mermaid label closed the <pre> early: %s", out)
	}
	if !strings.Contains(out, "&lt;/pre&gt;") {
		t.Errorf("the label's markup was not escaped as expected: %s", out)
	}
}

func TestRender_InlineSVG_Survives(t *testing.T) {
	out := renderOrFail(t, `<svg viewBox="0 0 100 50"><rect x="1" y="2" width="10" height="20"/><path d="M0 0L10 10"/><circle cx="5" cy="5" r="3"/></svg>`+"\n")

	for _, want := range []string{"<svg", "<rect", "<path", "<circle"} {
		if !strings.Contains(out, want) {
			t.Errorf("inline SVG lost %q: %s", want, out)
		}
	}
}

func TestRender_InlineSVG_OnlyCanonicalNamespaceSurvives(t *testing.T) {
	tests := []struct {
		name      string
		xmlns     string
		wantXMLNS bool
	}{
		{name: "missing"},
		{name: "canonical", xmlns: ` xmlns="http://www.w3.org/2000/svg"`, wantXMLNS: true},
		{name: "empty", xmlns: ` xmlns=""`},
		{name: "attacker", xmlns: ` xmlns="https://attacker.example/svg"`},
		{name: "xhtml", xmlns: ` xmlns="http://www.w3.org/1999/xhtml"`},
		{name: "mixed case", xmlns: ` xmlns="http://www.w3.org/2000/SVG"`},
		{name: "padded", xmlns: ` xmlns=" http://www.w3.org/2000/svg "`},
		{name: "duplicate alternative", xmlns: ` xmlns="https://attacker.example/svg" xmlns="http://www.w3.org/2000/svg"`},
		{name: "canonical before duplicate alternative", xmlns: ` xmlns="http://www.w3.org/2000/svg" xmlns="https://attacker.example/svg"`},
		{name: "canonical before malformed duplicate", xmlns: ` xmlns="http://www.w3.org/2000/svg" xmlns`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := `<svg` + tt.xmlns + ` viewBox="0 0 10 10"><rect x="1" y="2" width="3" height="4"/></svg>` + "\n"
			out := renderOrFail(t, src)
			hasXMLNS := strings.Contains(out, `xmlns="`)
			if hasXMLNS != tt.wantXMLNS {
				t.Errorf("xmlns presence = %v, want %v: %s", hasXMLNS, tt.wantXMLNS, out)
			}
			for _, want := range []string{"<svg", "<rect", `viewbox="0 0 10 10"`, `width="3"`, `height="4"`} {
				if !strings.Contains(strings.ToLower(out), strings.ToLower(want)) {
					t.Errorf("rejected namespace removed safe SVG feature %q: %s", want, out)
				}
			}
		})
	}
}

// viewBox must survive the sanitizer, but NOT necessarily with its authored
// capitalization. bluemonday tokenizes through golang.org/x/net/html, which
// lowercases attribute names, so the serialized output reads viewbox=. That is
// still correct in a browser: the HTML parser's "adjust SVG attributes" step
// maps the lowercase forms of every camelCase SVG attribute (viewbox → viewBox,
// gradientunits → gradientUnits, refx → refX, …) back for foreign elements, so
// the parsed DOM has the right name and getAttribute("viewBox") finds it.
//
// The assertion is therefore case-insensitive on purpose. Pinning the exact case
// would fail on correct output; asserting only "the digits are present somewhere"
// would pass even if the attribute had been dropped and the numbers survived in
// some other attribute. This checks the name and value together, case aside.
func TestRender_InlineSVG_ViewBoxSurvives(t *testing.T) {
	out := renderOrFail(t, `<svg viewBox="0 0 100 50"><rect width="10" height="10"/></svg>`+"\n")

	if !strings.Contains(strings.ToLower(out), `viewbox="0 0 100 50"`) {
		t.Errorf("viewBox was stripped — pan/zoom is impossible without it: %s", out)
	}
}

func TestRender_InlineSVG_ArtificerDiagramClassesSurvive(t *testing.T) {
	out := renderOrFail(t, `<svg viewBox="0 0 10 10"><rect class="dia-node dia-node--accent" width="5" height="5"/><path class="dia-edge" d="M0 0L5 5"/></svg>`+"\n")

	for _, want := range []string{`dia-node`, `dia-node--accent`, `dia-edge`} {
		if !strings.Contains(out, want) {
			t.Errorf("Artificer diagram class %q was stripped, so the diagram cannot read the --dia-* tokens: %s", want, out)
		}
	}
}

func TestRender_SVGEventHandler_Stripped(t *testing.T) {
	out := renderOrFail(t, `<svg viewBox="0 0 10 10" onload="alert(1)"><rect width="5" height="5" onclick="alert(2)"/></svg>`+"\n")

	for _, forbidden := range []string{"onload", "onclick", "alert("} {
		if strings.Contains(out, forbidden) {
			t.Errorf("output retained %q — inline-SVG event handlers must be stripped: %s", forbidden, out)
		}
	}
}

func TestRender_SVGScriptTag_Stripped(t *testing.T) {
	out := renderOrFail(t, `<svg viewBox="0 0 10 10"><script>alert(1)</script><rect width="5" height="5"/></svg>`+"\n")

	if strings.Contains(out, "<script") || strings.Contains(out, "alert(") {
		t.Errorf("a script inside SVG survived sanitization: %s", out)
	}
}

// The payload here is deliberately <img>, not <iframe>. An <iframe> version of
// this test passes even with a policy that only DENIES the container, because
// iframe happens to sit in bluemonday's built-in skip-content set — so it proved
// nothing about our own policy. <img> is not in that set, which makes this test
// actually exercise the SkipElementsContent call.
func TestRender_SVGForeignObject_StrippedWithItsContents(t *testing.T) {
	out := renderOrFail(t, `<svg viewBox="0 0 10 10"><foreignObject width="10" height="10"><img src="http://evil.example/beacon.png"></foreignObject></svg>`+"\n")

	for _, forbidden := range []string{"foreignObject", "<img", "evil.example"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("output retained %q — denying foreignObject is not enough, its CONTENTS must go too or bluemonday hoists them into the surviving SVG: %s", forbidden, out)
		}
	}
}

// Every denied container, each with a payload that is NOT in bluemonday's own
// skip-content set, so a regression in our skip list cannot hide behind its
// defaults.
func TestRender_DeniedSVGContainers_DropTheirSubtrees(t *testing.T) {
	cases := map[string]string{
		"foreignObject/img": `<svg viewBox="0 0 10 10"><foreignObject><img src="http://evil.example/a.png"></foreignObject></svg>`,
		"foreignObject/a":   `<svg viewBox="0 0 10 10"><foreignObject><a href="http://evil.example/phish">Approve</a></foreignObject></svg>`,
		"use/img":           `<svg viewBox="0 0 10 10"><use href="#x"><img src="http://evil.example/b.png"></use></svg>`,
		"image/img":         `<svg viewBox="0 0 10 10"><image><img src="http://evil.example/c.png"></image></svg>`,
		"animate/img":       `<svg viewBox="0 0 10 10"><animate><img src="http://evil.example/e.png"></animate></svg>`,
		"filter/img":        `<svg viewBox="0 0 10 10"><filter><img src="http://evil.example/f.png"></filter></svg>`,
		"pattern/img":       `<svg viewBox="0 0 10 10"><pattern><img src="http://evil.example/g.png"></pattern></svg>`,
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			out := renderOrFail(t, src+"\n")
			if strings.Contains(out, "evil.example") {
				t.Errorf("%s: a denied container's child survived and would fire a remote request: %s", name, out)
			}
		})
	}
}

// An unclosed <title> consumes the rest of the document, and this pins what the
// sanitizer can and cannot do about that.
//
// The consumption itself is NOT preventable here: golang.org/x/net/html
// tokenizes <title> as RAW TEXT, so everything after it is already one text token
// before any policy is consulted. No AllowElements/SkipElementsContent
// combination can put it back — recovering it would mean pre-processing the
// markdown source ahead of goldmark, which is a different change than this one.
//
// What the policy DOES control is which of two bad outcomes you get. With title
// denied AND in the skip-content set, the swallowed region is DROPPED: the
// document visibly ends early. Without the skip entry it would be retained as
// title text — present in the DOM, rendered by no browser — so a reader would
// believe they had read a document that was still hiding content. Truncation is
// the better failure, and it is what this asserts.
//
// Also asserted: the swallowed region cannot inject anything. That is the part
// that would actually be dangerous.
func TestRender_SVGTitle_SwallowedContentIsDroppedNotHidden(t *testing.T) {
	src := "# Visible heading\n\n<svg viewBox=\"0 0 1 1\"><title>\n\n## Second heading\n\nA paragraph.\n"
	out := renderOrFail(t, src)

	if !strings.Contains(out, "Visible heading") {
		t.Errorf("content BEFORE the title was lost, which no tokenizer behavior justifies: %s", out)
	}
	// The swallowed region must not survive as invisible title text.
	if strings.Contains(out, "Second heading") || strings.Contains(out, "A paragraph.") {
		t.Errorf("swallowed content was retained inside <title> — present in the DOM but rendered by no browser, so the reader would think they had read the whole document: %s", out)
	}
	if strings.Contains(out, "<title") {
		t.Errorf("a <title> element survived into the output: %s", out)
	}
}

// The dangerous version of the above: markup hidden after an unclosed title must
// not come back as live elements.
func TestRender_SVGTitle_CannotSmuggleMarkup(t *testing.T) {
	out := renderOrFail(t, "<svg viewBox=\"0 0 1 1\"><title></title><script>alert(1)</script>\n\n## Still renders\n")

	if strings.Contains(out, "<script") || strings.Contains(out, "alert(") {
		t.Errorf("a script following a closed <title> survived: %s", out)
	}
	if !strings.Contains(out, "Still renders") {
		t.Errorf("a CLOSED title should not consume anything, but following content was lost: %s", out)
	}
}

// <defs> and <mask> wrap definition-only geometry. If their tags are dropped
// (which bluemonday does to an allowed element with zero surviving attributes),
// that geometry is promoted to painted shapes and the diagram renders its own
// mask as a visible rectangle.
func TestRender_AttributelessSVGContainers_KeepTheirTags(t *testing.T) {
	out := renderOrFail(t, `<svg viewBox="0 0 10 10"><defs><mask><rect width="1" height="1"/></mask></defs><g><rect width="9" height="9"/></g></svg>`+"\n")

	for _, want := range []string{"<defs", "<mask", "<g"} {
		if !strings.Contains(out, want) {
			t.Errorf("container %q lost its tag, so definition-only geometry becomes a painted shape: %s", want, out)
		}
	}
}

func TestRender_SVGAnimateElements_Stripped(t *testing.T) {
	// SMIL can retarget an attribute after sanitization has already run.
	out := renderOrFail(t, `<svg viewBox="0 0 10 10"><rect width="5" height="5"><animate attributeName="href" to="javascript:alert(1)"/></rect></svg>`+"\n")

	for _, forbidden := range []string{"<animate", "attributeName", "javascript:"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("output retained %q — SMIL animation must not survive: %s", forbidden, out)
		}
	}
}

func TestRender_SVGExternalReferenceElements_Stripped(t *testing.T) {
	cases := map[string]string{
		"image": `<svg viewBox="0 0 10 10"><image href="http://evil.example/p.png" width="10" height="10"/></svg>`,
		"use":   `<svg viewBox="0 0 10 10"><use href="http://evil.example/s.svg#i"/></svg>`,
		"a":     `<svg viewBox="0 0 10 10"><a href="javascript:alert(1)"><rect width="5" height="5"/></a></svg>`,
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			out := renderOrFail(t, src+"\n")
			if strings.Contains(out, "evil.example") || strings.Contains(out, "javascript:") {
				t.Errorf("<%s> carried an off-document or script reference through: %s", name, out)
			}
			if strings.Contains(out, "<"+name+" ") {
				t.Errorf("<%s> element survived; it is deliberately not allowlisted: %s", name, out)
			}
		})
	}
}

func TestRender_SVGOffDocumentPaintURL_Dropped(t *testing.T) {
	out := renderOrFail(t, `<svg viewBox="0 0 10 10"><rect width="5" height="5" fill="url(http://evil.example/t.png)"/></svg>`+"\n")

	if strings.Contains(out, "evil.example") {
		t.Errorf("an off-document url() in fill survived — a diagram must not be able to phone home: %s", out)
	}
	// A same-document reference is the legitimate case and must still work.
	ok := renderOrFail(t, `<svg viewBox="0 0 10 10"><defs><linearGradient id="g"><stop offset="0" stop-color="#fff"/></linearGradient></defs><rect width="5" height="5" fill="url(#g)"/></svg>`+"\n")
	if !strings.Contains(ok, `url(#g)`) {
		t.Errorf("a same-document url(#id) fill was wrongly dropped: %s", ok)
	}
}

func TestRender_SVGJavascriptURIPaint_Dropped(t *testing.T) {
	out := renderOrFail(t, `<svg viewBox="0 0 10 10"><rect width="5" height="5" fill="javascript:alert(1)" stroke="url(javascript:alert(2))"/></svg>`+"\n")

	if strings.Contains(out, "javascript:") {
		t.Errorf("a javascript: URI survived in a paint attribute: %s", out)
	}
}

func TestRender_SVGStyleAttribute_NotAllowed(t *testing.T) {
	out := renderOrFail(t, `<svg viewBox="0 0 10 10"><rect width="5" height="5" style="position:fixed;top:0;left:0;width:100vw"/></svg>`+"\n")

	if strings.Contains(out, "style=") {
		t.Errorf("the style attribute survived; a diagram must style itself through classes, not inline CSS that can overlay the page: %s", out)
	}
}
