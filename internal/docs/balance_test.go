package docs

import (
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/net/html"
)

// strayCloserSources are documents whose HTML is not balanced, each ending
// in a paragraph carrying the marker the tests look for inside .doc-body.
// The svg cases are balanced to a tokenizer but not to a browser: the HTML
// tag inside the <svg> breaks out of foreign content, and an open <table>
// left behind swallows the shell's closers.
var strayCloserSources = map[string]string{
	"stray div":             "</div>\n\nMARKER",
	"stray section div":     "</section></div>\n\nMARKER",
	"unclosed div":          "<div>\n\nMARKER",
	"nested stray closers":  "<div><section>\n\n</section></div></div></div>\n\nMARKER",
	"shell ancestors":       "</main></div></div></body></html>\n\nMARKER",
	"misnested p and div":   "<p>x<div>y</p></div>\n\nMARKER",
	"stray in callout":      "> [!NOTE]\n> </div></blockquote>\n> body\n\nMARKER",
	"unclosed in list":      "- <div>item\n- two\n\nMARKER",
	"svg self-closed table": "<svg><table/></svg>\n\nMARKER",
	"svg open table":        "<svg><table>\n\nMARKER",
	"svg p breakout":        "<svg><p>x</p></svg>\n\nMARKER",
}

// wellNested reports whether s passes balanceFragment's own test.
func wellNested(t *testing.T, s string) bool {
	t.Helper()
	nodes, err := parseBody(s)
	if err != nil {
		t.Fatalf("parseBody: %v", err)
	}
	return fragmentWellNested(s, nodes)
}

// TestRender_UnbalancedHTML_IsBalanced renders every unbalanced source in
// both root kinds and requires the full output — frontmatter, body and
// callout markup — to be well nested, with the marker still present.
//
// Mutation: in renderWith, drop the balanceFragment call around the
// sanitized body — every case goes red ("</div>" survives, "<div>" stays
// open).
func TestRender_UnbalancedHTML_IsBalanced(t *testing.T) {
	for name, src := range strayCloserSources {
		for _, kind := range []RootKind{RootDocs, RootVault} {
			out, err := render([]byte(src), kind)
			if err != nil {
				t.Fatalf("%s: render: %v", name, err)
			}
			if !wellNested(t, out) {
				t.Errorf("%s (kind %v): output is not well nested:\n%s", name, kind, out)
			}
			if !strings.Contains(out, "MARKER") {
				t.Errorf("%s (kind %v): marker lost:\n%s", name, kind, out)
			}
		}
	}
}

// TestRender_StrayDivDropped pins the issue's own case: the stray "</div>"
// is gone from the output, and the paragraph after it survives.
//
// Mutation: make fragmentWellNested return true unconditionally — the input
// then comes back unchanged with its "</div>" and this goes red.
func TestRender_StrayDivDropped(t *testing.T) {
	out, err := Render([]byte("</div>\n\nx"))
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if strings.Contains(out, "</div>") || !strings.Contains(out, "<p>x</p>") {
		t.Fatalf("render = %q, want the stray </div> dropped and <p>x</p> kept", out)
	}
}

// TestRender_SVGBreakout_LeavesSVG pins the tree a browser builds when an
// HTML tag appears inside SVG: the <svg> closes and the HTML element follows
// it, rather than nesting inside it as an SVG element of the same name
// (compared against Chromium's DOM with Playwright).
//
// Mutation: in parseBody, build the tree with html.ParseFragment in a <div>
// context instead of html.Parse on the wrapped document — x/net then skips
// the breakout, "<svg><table/></svg>" reads as well nested, comes back
// unchanged, and this goes red.
func TestRender_SVGBreakout_LeavesSVG(t *testing.T) {
	for _, src := range []string{
		"<svg><table/></svg>\n\nMARKER",
		"<svg><table>\n\nMARKER",
		"<svg><p>x</p></svg>\n\nMARKER",
	} {
		out, err := Render([]byte(src))
		if err != nil {
			t.Fatalf("%q: render: %v", src, err)
		}
		if !strings.Contains(out, "<svg></svg>") || strings.Contains(out, "<svg><table") || strings.Contains(out, "<svg><p") {
			t.Errorf("%q: HTML tag left inside svg, want it broken out:\n%s", src, out)
		}
		if !strings.Contains(out, "MARKER") || !wellNested(t, out) {
			t.Errorf("%q: marker lost or output not well nested:\n%s", src, out)
		}
	}
}

// TestRender_DeepNesting_NotEscaped requires a document nested past x/net's
// 512-open-element parse limit to render as markup, not as escaped text:
// balancing gives up on it and serves the sanitized body unchanged.
//
// Mutation: in balancePasses, return html.EscapeString(sanitized) when
// parseBody fails — each case then renders as "&lt;ul&gt;…" text and this
// goes red.
func TestRender_DeepNesting_NotEscaped(t *testing.T) {
	var list strings.Builder
	for i := range 300 {
		_, _ = list.WriteString(strings.Repeat("  ", i) + "- x\n")
	}
	for _, tc := range []struct {
		name, src, tag string
		want           int
	}{
		{"list 300 deep", list.String(), "<ul>", 300},
		{"520 blockquotes", strings.Repeat(">", 520) + " q\n", "<blockquote>", 520},
		{"600 unclosed b", strings.Repeat("<b>", 600) + "z\n", "<b>", 600},
	} {
		out, err := Render([]byte(tc.src))
		if err != nil {
			t.Fatalf("%s: render: %v", tc.name, err)
		}
		if got := strings.Count(out, tc.tag); got != tc.want || strings.Contains(out, "&lt;") {
			t.Errorf("%s: %d %s tags (want %d), escaped=%v", tc.name, got, tc.tag, tc.want, strings.Contains(out, "&lt;"))
		}
	}
}

// TestBalanceFragment_WellNestedIsByteIdentical requires balanced markup of
// every shape the pipeline emits — void elements, self-closing SVG children,
// tables with explicit sections, entities, a callout — to come back as the
// very same bytes, not a re-serialization of them.
//
// Mutation: make fragmentWellNested return false unconditionally — every
// case is then re-serialized (entities and void tags change spelling, SVG
// children gain end tags) and this goes red.
func TestBalanceFragment_WellNestedIsByteIdentical(t *testing.T) {
	for _, s := range []string{
		"",
		"<p>a<br>b<img src=\"x.png\" alt=\"&#34;q&#34;\"></p>\n<hr>\n",
		"<p>it&#39;s &amp; &lt;b&gt;</p>\n",
		`<svg viewBox="0 0 2 2"><g><path d="M0 0"/><rect width="1" height="1"/></g><linearGradient x1="0"><stop offset="0"/></linearGradient></svg>`,
		"<table>\n<thead>\n<tr>\n<th>a</th>\n</tr>\n</thead>\n<tbody>\n<tr>\n<td>b</td>\n</tr>\n</tbody>\n</table>\n",
		"<pre tabindex=\"0\" class=\"chroma\"><code><span class=\"line\"><span class=\"cl\">x\n</span></span></code></pre>\n",
		"<ul>\n<li><input checked=\"\" disabled=\"\" type=\"checkbox\"> done</li>\n</ul>\n",
		"<details><summary>s</summary><p>b</p></details>\n",
	} {
		if got := balanceFragment(s); got != s {
			t.Errorf("balanceFragment changed well-nested input\n in: %q\nout: %q", s, got)
		}
	}
	callout, err := Render([]byte("> [!NOTE]\n> body\n"))
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if got := balanceFragment(callout); got != callout {
		t.Errorf("balanceFragment changed a rendered callout\n in: %q\nout: %q", callout, got)
	}
}

// TestBalanceFragment_RandomTagSoup feeds sanitized random tag soup — start,
// end and self-closing tags of block, inline, table, list, void and SVG
// elements in any order — through balancePasses and requires every result to
// be well nested, idempotent, and reached by rebuilding rather than by the
// escaped last resort. 300k such documents converged within 2 passes when
// this was written; the assertion allows maxBalancePasses.
//
// Mutations: make hoistVoidChildren return at once — a "</br>" inside SVG
// then leaves a <br> with children that html.Render refuses, the escape
// fallback fires, and this goes red. Drop the `&& inForeign` guard in
// sourceTags (read every self-closing tag as closed) — an HTML "<div/>",
// which a browser leaves open, then reads as balanced and this goes red.
func TestBalanceFragment_RandomTagSoup(t *testing.T) {
	rng := rand.New(rand.NewSource(596)) //nolint:gosec // G404: deterministic test fixture, not crypto
	tags := []string{
		"div", "section", "p", "span", "b", "i", "a", "blockquote", "ul", "ol", "li",
		"dl", "dt", "dd", "table", "thead", "tbody", "tr", "td", "th", "caption",
		"pre", "code", "br", "img", "hr", "svg", "path", "g", "text", "tspan",
		"details", "summary", "h2", "h3", "em", "strong", "s", "del", "sub", "sup",
	}
	for doc := range 20000 {
		var b strings.Builder
		for range 1 + rng.Intn(40) {
			tag := tags[rng.Intn(len(tags))]
			switch rng.Intn(4) {
			case 0:
				_, _ = b.WriteString("<" + tag + ">")
			case 1:
				_, _ = b.WriteString("</" + tag + ">")
			case 2:
				_, _ = b.WriteString("<" + tag + "/>")
			default:
				_, _ = b.WriteString("t ")
			}
		}
		in := sanitizer.Sanitize(b.String())
		out, passes := balancePasses(in)
		if passes < 0 {
			t.Fatalf("doc %d: balance fell back to escaping\nsrc: %q\n in: %q", doc, b.String(), in)
		}
		if !wellNested(t, out) {
			t.Fatalf("doc %d: output is not well nested\nsrc: %q\n in: %q\nout: %q", doc, b.String(), in, out)
		}
		if again := balanceFragment(out); again != out {
			t.Fatalf("doc %d: balanceFragment is not idempotent\n out: %q\nagain: %q", doc, out, again)
		}
	}
}

// TestRender_UnbalancedCallout_KeepsChrome pins the order balanceFragment
// runs in: before transformCallouts. A document that needs rebuilding is
// re-sanitized, and the policy strips the callout icon's aria-hidden; built
// after the rebuild, the callout keeps it.
//
// Mutation: in renderWith, apply balanceFragment to transformCallouts'
// output instead of before it — the stray "</div>" forces a rebuild, the
// re-sanitize strips aria-hidden from the icon, and this goes red.
func TestRender_UnbalancedCallout_KeepsChrome(t *testing.T) {
	for _, kind := range []RootKind{RootDocs, RootVault} {
		out, err := render([]byte("> [!NOTE]\n> </div>\n> body\n"), kind)
		if err != nil {
			t.Fatalf("render: %v", err)
		}
		if !strings.Contains(out, `<blockquote class="callout note">`) || !strings.Contains(out, `aria-hidden="true"`) {
			t.Errorf("kind %v: callout chrome lost:\n%s", kind, out)
		}
		if strings.Contains(out, "</div>\n") || !wellNested(t, out) {
			t.Errorf("kind %v: stray closer survived:\n%s", kind, out)
		}
	}
}

// TestServer_UnbalancedDoc_StaysInsideDocBody serves each unbalanced source
// through the real shell template and parses the page with the HTML5 tree
// builder — the algorithm a browser runs — to check the DOM a reader gets:
// .doc-body holds the document's marker, and the sidebar and status bar sit
// outside it.
//
// Chromium agrees: on main a stray "</div>" or "</main>" leaves the marker
// outside .doc-body, while an unclosed "<div>" stays contained because the
// shell's "</main>" shuts it (checked with Playwright against docs serve).
// That an unclosed element closes inside the fragment itself is
// TestRender_UnbalancedHTML_IsBalanced's job.
//
// Mutation: in renderWith, drop the balanceFragment call — the stray-closer
// cases then end .doc-body before the marker and this goes red.
func TestServer_UnbalancedDoc_StaysInsideDocBody(t *testing.T) {
	for _, vault := range []bool{false, true} {
		dir := t.TempDir()
		if vault {
			if err := os.MkdirAll(filepath.Join(dir, ".obsidian"), 0o750); err != nil {
				t.Fatal(err)
			}
		}
		names := map[string]string{}
		i := 0
		for name, src := range strayCloserSources {
			file := "d" + string(rune('a'+i)) + ".md"
			i++
			names[file] = name
			writeFile(t, filepath.Join(dir, file), src)
		}
		idx, err := NewIndex([]string{dir})
		if err != nil {
			t.Fatalf("NewIndex: %v", err)
		}
		h := testHandler(idx)
		label := idx.Roots()[0].Label
		for file, name := range names {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/doc/"+label+"/"+file, nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("%s: status %d", name, rec.Code)
			}
			page, err := html.Parse(strings.NewReader(rec.Body.String()))
			if err != nil {
				t.Fatalf("%s: parse page: %v", name, err)
			}
			body := findByClass(page, "doc-body")
			if body == nil {
				t.Fatalf("%s (vault %v): no .doc-body in page", name, vault)
			}
			if !strings.Contains(textOf(body), "MARKER") {
				t.Errorf("%s (vault %v): .doc-body lost the document's content: %q", name, vault, textOf(body))
			}
			for _, class := range []string{"docs-nav", "statusbar"} {
				n := findByClass(page, class)
				if n == nil {
					t.Fatalf("%s: no .%s in page", name, class)
				}
				if isAncestor(body, n) {
					t.Errorf("%s (vault %v): .%s is inside .doc-body", name, vault, class)
				}
			}
			if main := findByClass(page, "surface-document"); main == nil || !isAncestor(main, body) {
				t.Errorf("%s (vault %v): .doc-body escaped main.surface-document", name, vault)
			}
		}
	}
}

func findByClass(n *html.Node, class string) *html.Node {
	if n.Type == html.ElementNode {
		for _, a := range n.Attr {
			if a.Key == "class" && strings.Contains(" "+a.Val+" ", " "+class+" ") {
				return n
			}
		}
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if f := findByClass(c, class); f != nil {
			return f
		}
	}
	return nil
}

func isAncestor(anc, n *html.Node) bool {
	for p := n.Parent; p != nil; p = p.Parent {
		if p == anc {
			return true
		}
	}
	return false
}

func textOf(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			_, _ = b.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return b.String()
}
