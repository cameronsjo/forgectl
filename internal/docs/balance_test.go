package docs

import (
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
// balanceDeep contains it without the tree builder.
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

// deepSources nest past x/net's parse limit, so balancePasses hands them to
// balanceDeep, and each carries a stray closer aimed at the shell — "</div>"
// survives the sanitizer; "</main>" does not — followed by the text ESC.
func deepSources() map[string]string {
	var list strings.Builder
	for i := range 300 {
		_, _ = list.WriteString(strings.Repeat("  ", i) + "- x\n")
	}
	const esc = "\n\n</div></main>ESC\n"
	return map[string]string{
		"600 unclosed b":            strings.Repeat("<b>", 600) + "z" + esc,
		"closer before 600 b":       "</div></main>ESC\n\n" + strings.Repeat("<b>", 600) + "z\n",
		"list 300 deep":             list.String() + esc,
		"520 blockquotes":           strings.Repeat(">", 520) + " q" + esc,
		"closer inside quotes":      strings.Repeat(">", 520) + " q </div></main>ESC\n",
		"600 unclosed div":          strings.Repeat("<div>", 600) + "z" + esc,
		"600 div, 610 closers":      strings.Repeat("<div>", 600) + "z" + strings.Repeat("</div>", 610) + "ESC\n",
		"li closes div under 600 b": strings.Repeat("<b>", 600) + "<ul><li><div><li>x</div></ul>ESC\n",
	}
}

// shellPage places body where the shell does — html > body > div > div >
// main > div.doc-body — with shell content after it, as the served page has.
func shellPage(body string) string {
	return `<!DOCTYPE html><html><head></head><body><div id="shell"><div class="content-grid"><main class="surface-document"><div class="doc-body">` +
		body + `</div><div class="home">HOME</div></main><aside class="outline">OUTL</aside></div><footer class="statusbar">SENTINEL</footer></div></body></html>`
}

// requireContained parses body inside shellPage with the HTML5 tree builder
// and fails unless .doc-body holds every character of the document's text
// and none of the shell's content after it.
func requireContained(t *testing.T, label, body string) {
	t.Helper()
	page, err := html.Parse(strings.NewReader(shellPage(body)))
	if err != nil {
		t.Fatalf("%s: parse page: %v", label, err)
	}
	doc := findByClass(page, "doc-body")
	if doc == nil {
		t.Fatalf("%s: no .doc-body", label)
	}
	for _, class := range []string{"home", "outline", "statusbar"} {
		if n := findByClass(page, class); n == nil || isAncestor(doc, n) {
			t.Errorf("%s: .%s missing or inside .doc-body\nbody: %q", label, class, body)
		}
	}
	if main := findByClass(page, "surface-document"); main == nil || !isAncestor(main, doc) {
		t.Errorf("%s: .doc-body escaped main\nbody: %q", label, body)
	}
	frag, err := html.Parse(strings.NewReader(body))
	if err != nil {
		t.Fatalf("%s: parse body: %v", label, err)
	}
	if got, want := strings.Join(strings.Fields(textOf(doc)), " "), strings.Join(strings.Fields(textOf(frag)), " "); got != want {
		t.Errorf("%s: .doc-body text %q, want the document's %q\nbody: %q", label, got, want, body)
	}
}

// TestRender_DeepStrayCloser_Contained renders each deep source and checks
// what the tree builder cannot: the served body never names a shell
// ancestor in an end tag and is balanced tag for tag, so no closer in it
// can reach .doc-body's ancestors. The rendered page itself is too deep for
// x/net to build; Chromium, which caps its tree at 512 levels instead of
// refusing it, kept every case's ESC and content inside .doc-body and the
// status bar outside it (Playwright, against the served page shape).
//
// Mutations: in balancePasses, return sanitized unchanged when parseBody
// fails (main's behaviour) — every case keeps a "</div>" and this goes red.
// In balanceDeep, drop the div rename — the "</div>"s the stack matches come
// back and this goes red.
func TestRender_DeepStrayCloser_Contained(t *testing.T) {
	for name, src := range deepSources() {
		out, err := Render([]byte(src))
		if err != nil {
			t.Fatalf("%s: render: %v", name, err)
		}
		if _, passes := balancePasses(string(sanitizer.SanitizeBytes([]byte(out)))); passes != passesDeep {
			t.Errorf("%s: balancePasses = %d on the rendered body, want the deep fallback (%d)", name, passes, passesDeep)
		}
		for _, closer := range []string{"</div>", "</main>", "</body>", "</html>"} {
			if strings.Contains(out, closer) {
				t.Errorf("%s: output keeps %s", name, closer)
			}
		}
		if !strings.Contains(out, "ESC") {
			t.Errorf("%s: content after the stray closer lost", name)
		}
		if err := tagsBalanced(out); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// tagsBalanced reports the first end tag in s that does not close the
// innermost open element, or elements left open at the end. Void elements,
// and self-closing tags inside SVG or MathML, open nothing.
func tagsBalanced(s string) error {
	var stack []string
	foreign := 0
	z := html.NewTokenizer(strings.NewReader(s))
	for {
		switch tt := z.Next(); tt {
		case html.ErrorToken:
			if len(stack) > 0 {
				return fmt.Errorf("%d elements left open, innermost <%s>", len(stack), stack[len(stack)-1])
			}
			return nil
		case html.StartTagToken, html.SelfClosingTagToken:
			raw, _ := z.TagName()
			name := string(raw)
			if name == "svg" || name == "math" {
				foreign++
			}
			if tt == html.SelfClosingTagToken && foreign > 0 || foreign == 0 && voidElements[name] {
				if name == "svg" || name == "math" {
					foreign--
				}
				continue
			}
			stack = append(stack, name)
		case html.EndTagToken:
			raw, _ := z.TagName()
			name := string(raw)
			if len(stack) == 0 || stack[len(stack)-1] != name {
				return fmt.Errorf("</%s> does not close the innermost open element (stack depth %d)", name, len(stack))
			}
			stack = stack[:len(stack)-1]
			if name == "svg" || name == "math" {
				foreign--
			}
		}
	}
}

// TestBalanceDeep_ContainsShallowSoup runs balanceDeep on inputs the tree
// builder can still parse — its containment argument does not depend on
// depth — and checks the page x/net builds around each: the unbalanced
// sources, the li-closes-div shape, SVG breakouts, and random tag soup of
// every element the policy allows plus the ones it strips (raw-text, shell
// and frameset tags), each ending in a stray "</div></main>".
//
// Mutations: in balanceDeep, drop the div rename — "<ul><li><div><li>x</div>"
// keeps a "</div>" whose <div> the second <li> already closed, it closes
// .doc-body, and this goes red. Skip the closeTo(0) at the end of input — an
// unclosed <table> keeps .doc-body open and this goes red. Skip breakOut — a
// <table> inside <svg> is left open and this goes red.
func TestBalanceDeep_ContainsShallowSoup(t *testing.T) {
	cases := []string{
		"<ul><li><div><li>x</div></ul>ESC",
		"<dl><dd><div><dt>x</div></dl>ESC",
		"<table><tr><td><div><td>x</div></table>ESC",
		"<svg><g><table><td>x</svg>ESC",
		"<math><mi><table>x</math>ESC",
		"<svg><foreignObject><div>x</div></foreignObject></svg>ESC",
		"<textarea></div></textarea>ESC",
		"<main>m</main></body></html>ESC",
	}
	for _, src := range strayCloserSources {
		cases = append(cases, src)
	}
	rng := rand.New(rand.NewSource(623)) //nolint:gosec // G404: deterministic test fixture, not crypto
	tags := []string{
		"div", "section", "p", "span", "b", "i", "a", "blockquote", "ul", "ol", "li",
		"dl", "dt", "dd", "table", "thead", "tbody", "tr", "td", "th", "caption", "col",
		"pre", "code", "br", "img", "hr", "svg", "path", "g", "text", "math", "mi",
		"foreignObject", "desc", "details", "summary", "h2", "h3", "em", "ruby", "rt",
		"font color=red", "button", "select", "main", "body", "html", "textarea",
		"title", "script", "template", "object", "frameset",
	}
	for range 3000 {
		var b strings.Builder
		for range 1 + rng.Intn(60) {
			tag := tags[rng.Intn(len(tags))]
			switch rng.Intn(5) {
			case 0, 1:
				_, _ = b.WriteString("<" + tag + ">")
			case 2:
				_, _ = b.WriteString("</" + strings.Fields(tag)[0] + ">")
			case 3:
				_, _ = b.WriteString("<" + tag + "/>")
			default:
				_, _ = b.WriteString("t ")
			}
		}
		cases = append(cases, b.String()+"</div></main>ESC")
	}
	for i, src := range cases {
		requireContained(t, fmt.Sprintf("case %d %q", i, src), balanceDeep(src))
	}
}

// TestBalanceDeep_OutputIsSanitizerFixedPoint requires balanceDeep's output
// on sanitized input to survive the sanitizer unchanged: the re-sanitize in
// balancePasses is a backstop, and what the fallback writes — the renamed
// <section>, its closers, escaped text — is already what the policy emits.
//
// Mutations: in balanceDeep, rename div to "center" (a tag the policy strips)
// instead of "section" — the re-sanitize then removes it and this goes red.
// Write z.Text() without html.EscapeString — an escaped "&lt;b&gt;" comes
// back as a tag and this goes red.
func TestBalanceDeep_OutputIsSanitizerFixedPoint(t *testing.T) {
	srcs := []string{}
	for _, src := range deepSources() {
		out, err := Render([]byte(src))
		if err != nil {
			t.Fatal(err)
		}
		srcs = append(srcs, string(sanitizer.SanitizeBytes([]byte(out))))
	}
	srcs = append(srcs, sanitizer.Sanitize(strings.Repeat("<div class=\"x\">", 600)+"&lt;b&gt; it&#39;s &amp; <svg viewBox=\"0 0 1 1\"><g><path d=\"M0 0\"/></g></svg>"))
	rng := rand.New(rand.NewSource(62)) //nolint:gosec // G404: deterministic test fixture, not crypto
	tags := []string{"div", "section", "p", "b", "li", "ul", "table", "td", "svg", "g", "path", "br", "img", "pre", "code", "h2"}
	for range 300 {
		var b strings.Builder
		for range 600 + rng.Intn(100) {
			tag := tags[rng.Intn(len(tags))]
			switch rng.Intn(4) {
			case 0, 1:
				_, _ = b.WriteString("<" + tag + ">")
			case 2:
				_, _ = b.WriteString("</" + tag + ">")
			default:
				_, _ = b.WriteString("&lt;t&gt; ")
			}
		}
		srcs = append(srcs, sanitizer.Sanitize(b.String()))
	}
	for i, in := range srcs {
		out := balanceDeep(in)
		if again := sanitizer.Sanitize(out); again != out {
			t.Fatalf("case %d: balanceDeep output changes under the sanitizer\n out: %.300q\nagain: %.300q", i, out, again)
		}
	}
}

// TestBalanceDeep_Linear bounds balanceDeep on about 1MB of adversarial
// nesting: 170k open <b>, then 170k end tags naming nothing open. A stack
// search per end tag is 170k squared steps (minutes); the count lookup is
// linear (well under a second).
//
// Mutation: in balanceDeep, drop the count[name] == 0 early continue and
// search the stack for every end tag (guarding i >= 0) — this goes red on
// the time bound.
func TestBalanceDeep_Linear(t *testing.T) {
	src := strings.Repeat("<b>", 170_000) + "z" + strings.Repeat("</i>", 170_000)
	start := time.Now()
	out := balanceDeep(src)
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("balanceDeep took %v on %d bytes", d, len(src))
	}
	if strings.Contains(out, "</i>") || strings.Count(out, "</b>") != 170_000 {
		t.Fatalf("stray </i> kept or <b> left open")
	}
}
