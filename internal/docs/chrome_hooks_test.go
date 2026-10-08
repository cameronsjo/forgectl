package docs

// Test plan for the reader's chrome lookups (forgectl#617, #583)
//
// The reader's JS and CSS find chrome elements by a data-fc attribute, never
// by id. A document can produce any id (a heading slug, or raw HTML), so an id
// lookup can be captured by content. A doc cannot plant a data-fc: the
// sanitizer strips every data-* attribute, and mermaid-init.js scrubs data-fc
// from rendered diagrams after render, because mermaid's own sanitizer keeps
// data-* (forgectl#643).
//
//   [x] Happy: a doc whose headings slug to every chrome id leaves each
//              data-fc hook on exactly one element, the chrome's
//   [x] Edge:  raw-HTML data-fc in a doc is stripped
//   [x] Happy: reload.js, nav-toggle.js and sidenav-filter.js never call
//              getElementById on a chrome id, and every data-fc they query
//              exists in the shell
//   [x] Happy: the shell CSS does not style chrome by id
//   [x] Happy: every DOM lookup in the reader's scripts (querySelector,
//              querySelectorAll, closest, matches, getElementsBy*) is either
//              rooted at a data-fc hook or on a reviewed list of content-level
//              lookups with its reason, so a new class- or tag-based chrome
//              lookup fails here (forgectl#643). doc-main and doc-body are
//              content roots: a lookup UNDER them reaches the doc's own
//              elements, so it needs a listed reason too
//   [x] Edge:  a lookup the scan cannot resolve (a variable selector, a
//              wrapper's parameter) is listed too, and a wrapper's call sites
//              are checked in its place
//   [x] Edge:  a listed lookup the scripts no longer make fails, so the list
//              cannot rot into a blanket pass
//   [x] Happy: the shell has no inline <script> the scan would miss
//   [x] Happy: the raw-HTML chrome classes and tags a doc can plant survive
//              the sanitizer or not as this file assumes, and none carries a
//              data-fc hook
//   [x] Happy: the live-status item keeps the full text in a title and wraps
//              the host in a span the 480px rule can hide
//
// Known limits of the lookup scan, accepted rather than solved:
//   - contentLookups is keyed by selector, not call site, so a reviewed
//     selector reused elsewhere in the same script passes unreviewed;
//   - it sees only dotted method calls: bracket access
//     (document["querySelector"](…)), an aliased or bound method, or a
//     selector built outside a string constant and + escapes it.
// The browser half of this is scripts/verify-reader-chrome.mjs, which also
// covers the client-side mermaid render a Go test cannot see.

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

var chromeIDs = []string{
	"shell", "nav-toggle", "drawer-scrim", "docs-nav", "doc-filter",
	"filter-empty", "doc-main", "live-status", "doc-missing",
}

func chromePage(t *testing.T, md string) string {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "hostile.md"), md)
	idx, err := NewIndex([]string{dir})
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	h := testHandler(idx)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/doc/"+idx.Roots()[0].Label+"/hostile.md", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	// Markup only: the inline <style> names the hooks in its selectors.
	page := rec.Body.String()
	return page[strings.Index(page, "<body>"):]
}

func TestChrome_DocContentCannotCaptureOrForgeAHook(t *testing.T) {
	var md strings.Builder
	md.WriteString("# hostile\n\n")
	for _, id := range chromeIDs {
		md.WriteString("## " + strings.ReplaceAll(id, "-", " ") + "\n\n")
		md.WriteString(`<span id="` + id + `" data-fc="` + id + `">forged</span>` + "\n\n")
	}
	body := chromePage(t, md.String())

	for _, id := range chromeIDs {
		// The collision is real: content carries the chrome's id...
		if id != "doc-missing" && strings.Count(body, `id="`+id+`"`) < 2 {
			t.Errorf("fixture no longer collides on id %q; the test proves nothing", id)
		}
		// ...but the hook is unforgeable: at most the chrome's own element
		// (doc-missing is created by reload.js, so the server never emits it).
		want := 1
		if id == "doc-missing" {
			want = 0
		}
		if got := strings.Count(body, `data-fc="`+id+`"`); got != want {
			t.Errorf(`data-fc=%q appears %d times, want %d`, id, got, want)
		}
	}
}

func chromeRead(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Clean(rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

// chromeScripts is every first-party script the shell loads. The vendored
// ones (mermaid.min.js, KaTeX, Artificer) are not listed: they find their
// targets by data-* attributes or by the elements this repo's scripts hand
// them.
var chromeScripts = []string{
	"copy.js", "math-init.js", "mermaid-init.js", "nav-toggle.js",
	"reload.js", "sidenav-filter.js", "svg-panzoom.js",
}

func TestChrome_ScriptsLookUpByDataFcNotID(t *testing.T) {
	// An h2 gives the page an outline, so the outline hooks are present.
	body := chromePage(t, "# x\n\n## y\n")
	hook := regexp.MustCompile(`\[data-fc="([a-z-]+)"\]`)
	for _, f := range chromeScripts {
		src := chromeRead(t, filepath.Join("assets", f))
		for _, id := range chromeIDs {
			for _, q := range []string{`getElementById("` + id + `")`, `getElementById('` + id + `')`, `banner.id = "` + id + `"`} {
				if strings.Contains(src, q) {
					t.Errorf("%s uses the forgeable %s", f, q)
				}
			}
		}
		for _, m := range hook.FindAllStringSubmatch(src, -1) {
			if m[1] == "doc-missing" {
				continue // created by reload.js itself
			}
			if !strings.Contains(body, `data-fc="`+m[1]+`"`) {
				t.Errorf("%s queries [data-fc=%q], which the shell does not carry", f, m[1])
			}
		}
	}
}

func TestChrome_CSSDoesNotStyleChromeByID(t *testing.T) {
	tmpl := chromeRead(t, filepath.Join("templates", "shell.html.tmpl"))
	style := tmpl[:strings.Index(tmpl, "</style>")]
	for _, id := range chromeIDs {
		if strings.Contains(style, "#"+id+" ") || strings.Contains(style, "#"+id+"{") {
			t.Errorf("shell CSS selects #%s; content can carry that id and be styled as chrome", id)
		}
	}
}

func TestChrome_LiveStatusHostIsHideableAndKeepsATooltip(t *testing.T) {
	body := chromePage(t, "# x\n")
	if !strings.Contains(body, `<span class="live-status__host"> · `) {
		t.Errorf("live-status host is not in its own span")
	}
	if !regexp.MustCompile(`data-fc="live-status"[^>]* title="serving · [^"]+"`).MatchString(body) {
		t.Errorf("live-status lacks a title carrying the full text")
	}
	tmpl := chromeRead(t, filepath.Join("templates", "shell.html.tmpl"))
	if !regexp.MustCompile(`@media \(max-width: 480px\) \{\s*\.live-status__host \{ display: none; \}`).MatchString(tmpl) {
		t.Errorf("no <=480px rule hides the host")
	}
}

// contentLookups is every DOM lookup in chromeScripts that is not rooted at a
// data-fc hook, keyed by script and then by selector, with the reason a doc
// cannot use it to reach chrome. A selector the scan cannot resolve is keyed
// by its raw source text.
//
// A doc can carry any class and some tags (see
// TestChrome_PlantedChromeSurvivesOnlyAsContent), so a lookup that finds
// CHROME by class or tag can return the doc's copy. Before adding an entry,
// make sure the lookup finds document content by design or runs on a root
// that was itself found by data-fc. A lookup for chrome gets a data-fc hook in
// the shell instead.
var contentLookups = map[string]map[string]string{
	"copy.js": {
		"pre.mermaid[data-mermaid-source]": "content: diagrams in the copied selection",
		".math":                            "content: formulas in the copied selection",
		".embed":                           "content: the frame mermaid-init.js wraps around a diagram",
		".embed-bar":                       "runs on the detached clone of the selection",
		".dia-viewport, .dia-stage":        "runs on the detached clone of the selection",
		"svg":                              "runs on the detached clone of the selection",
		"*":                                "runs on the detached clone of the selection",
		"span":                             "runs on the detached clone of the selection",
	},
	"math-init.js": {
		".math":              "content: the formulas it renders",
		".math .katex-error": "content: parse errors in the formulas it rendered, recolored on a theme change; a doc-forged one only changes color",
	},
	"mermaid-init.js": {
		"pre.mermaid [data-fc], pre.mermaid [data-forgectl-notice], pre.mermaid [data-forgectl-props]": "scrubs hooks forged inside rendered diagrams",
		"pre.mermaid":         "content: the diagrams it renders; diagram focus: the diagrams under the doc-body hook, or the one inside an .embed",
		"pre.mermaid [class]": "scrubs chrome class names off rendered diagram elements",
		".embed":              "content: the frame it wraps around a diagram; diagram focus: up from the focused control, kept only inside the doc-body hook",
		".dia-viewport":       "scoped to an .embed this script created",
		".embed-reset":        "diagram focus: inside a doc-body diagram's .embed",
		"a":                   "diagram focus: links inside one doc-body diagram",
	},
	"reload.js": {
		":is(h1,h2,h3,h4,h5,h6)[id]":  "content: headings, on the doc-main root",
		`"#" + CSS.escape(anchor.id)`: "content: a heading, on the doc-main root",
		"details":                     "on a sidenav or doc-body root found by data-fc, or up from one",
		":scope > summary .label":     "under a <details> from a data-fc root",
		".label":                      "under the focused element or a <summary>",
		"[data-fc]":                   "focus restore: the nearest hook above the focused control, recorded as its region",
		`'[data-fc="' + CSS.escape(key.region) + '"]'`: "focus restore: the region recorded from a real hook",
		`"#" + CSS.escape(key.id)`:                     "focus restore: inside the recorded region",
		"a[href]":                                      "focus restore: inside the recorded region",
		"summary":                                      "focus restore: inside the recorded region",
		".live-dot":                                    "under the live-status data-fc hook",
		".live-status__text":                           "under the live-status data-fc hook",
	},
	"sidenav-filter.js": {
		"li":                  "up from a link or folder under the sidenav data-fc hook",
		"a[data-filter-text]": "under a sidenav node; data-filter-text is server-set and the sanitizer strips data-*",
	},
	"svg-panzoom.js": {
		`[data-fc="doc-main"] svg:not([aria-hidden="true"])`: "content: inline and rendered SVG in the doc pane",
		".katex":     "content: KaTeX output inside the doc",
		".dia-stage": "scoped to a viewport this script created",
	},
}

// selectorWrappers names each script's helpers that take a selector and
// query the document with it, and which argument (0-based) the selector is.
// Their call sites are scanned like the query methods themselves, and a
// lookup inside the named function's own body that passes that parameter
// straight through is not flagged. The exemption is keyed by function name,
// so a lookup on a parameter of the same name in any other function is
// still flagged (forgectl#718).
var selectorWrappers = map[string]map[string]int{
	"reload.js":         {"replace": 0, "within": 1},
	"sidenav-filter.js": {"all": 0},
}

var (
	domQueryCall = regexp.MustCompile(`\.(querySelector|querySelectorAll|closest|matches|getElementsByTagName|getElementsByClassName|getElementsByName)\(`)
	jsStringVar  = regexp.MustCompile(`(?m)^\s*var ([A-Z_]+) = (?:"([^"]*)"|'([^']*)');`)
)

// jsArg returns the source text of the n-th (0-based) argument of the call
// whose argument list starts at src[i], or "" when the call has fewer. It
// skips string literals, so a paren or comma inside a selector string does
// not end an argument.
func jsArg(src string, i, n int) string {
	depth := 0
	for j := i; j < len(src); j++ {
		switch c := src[j]; c {
		case '"', '\'', '`':
			for j++; j < len(src) && src[j] != c; j++ {
				if src[j] == '\\' {
					j++
				}
			}
		case '(', '[':
			depth++
		case ')', ']':
			if depth == 0 {
				if n == 0 {
					return strings.TrimSpace(src[i:j])
				}
				return ""
			}
			depth--
		case ',':
			if depth == 0 {
				if n == 0 {
					return strings.TrimSpace(src[i:j])
				}
				n--
				i = j + 1
			}
		}
	}
	return ""
}

// jsResolve evaluates a selector expression made of string literals and the
// file's string constants joined by +. ok is false for anything else.
func jsResolve(expr string, consts map[string]string) (sel string, ok bool) {
	var b strings.Builder
	for _, part := range strings.Split(expr, "+") {
		part = strings.TrimSpace(part)
		switch {
		case len(part) >= 2 && (part[0] == '"' || part[0] == '\'') && part[len(part)-1] == part[0]:
			b.WriteString(part[1 : len(part)-1])
		case consts[part] != "":
			b.WriteString(consts[part])
		default:
			return "", false
		}
	}
	return b.String(), true
}

// contentRoots are the hooks whose subtree is the document itself. The hook
// alone is chrome, but a selector descending from it reaches doc content.
var contentRoots = []string{`[data-fc="doc-main"]`, `[data-fc="doc-body"]`}

// dataFcRooted reports whether every comma-separated selector in sel starts
// with a compound carrying a data-fc attribute, so it can only match inside
// (or on) a shell element, and none descends from a content root. Commas
// inside brackets or parens do not split.
func dataFcRooted(sel string) bool {
	depth := 0
	start := 0
	var parts []string
	for i := 0; i < len(sel); i++ {
		switch sel[i] {
		case '(', '[':
			depth++
		case ')', ']':
			depth--
		case ',':
			if depth == 0 {
				parts = append(parts, sel[start:i])
				start = i + 1
			}
		}
	}
	parts = append(parts, sel[start:])
	for _, p := range parts {
		p = strings.TrimSpace(p)
		depth = 0
		end := len(p)
		for i := 0; i < len(p) && end == len(p); i++ {
			switch p[i] {
			case '(', '[':
				depth++
			case ')', ']':
				depth--
			case ' ', '>', '+', '~':
				if depth == 0 {
					end = i
				}
			}
		}
		if !strings.Contains(p[:end], `[data-fc="`) {
			return false
		}
		for _, root := range contentRoots {
			if end < len(p) && strings.Contains(p[:end], root) {
				return false
			}
		}
	}
	return true
}

type chromeLookup struct {
	key    string // the resolved selector, or the raw expression
	rooted bool
	line   int
	// passThrough: the lookup is inside a selectorWrappers function and
	// queries with that function's selector parameter, which the wrapper's
	// call sites supply and the scan checks there.
	passThrough bool
}

// jsBlockEnd returns the index of the brace closing the block that opens at
// src[open], or -1 when it never closes. It skips string literals, // and
// /* */ comments, and regex literals, so a brace in any of them does not
// count: a comment reading "returns {}" inside a wrapper would otherwise end
// its body early and hand the exemption to code after it.
func jsBlockEnd(src string, open int) int {
	depth := 0
	for j := open; j < len(src); j++ {
		switch c := src[j]; {
		case c == '"' || c == '\'' || c == '`':
			for j++; j < len(src) && src[j] != c; j++ {
				if src[j] == '\\' {
					j++
				}
			}
		case c == '/' && j+1 < len(src) && src[j+1] == '/':
			for j < len(src) && src[j] != '\n' {
				j++
			}
		case c == '/' && j+1 < len(src) && src[j+1] == '*':
			end := strings.Index(src[j+2:], "*/")
			if end < 0 {
				return -1
			}
			j += end + 3
		case c == '/' && jsRegexCanStart(src[:j]):
			inClass := false
			for j++; j < len(src) && src[j] != '\n'; j++ {
				switch src[j] {
				case '\\':
					// Step over the escaped character, so an escaped
					// slash (/^\/{/) does not end the literal.
					j++
					continue
				case '[':
					inClass = true
				case ']':
					inClass = false
				}
				if src[j] == '/' && !inClass {
					break
				}
			}
		case c == '{':
			depth++
		case c == '}':
			depth--
			if depth == 0 {
				return j
			}
		}
	}
	return -1
}

// jsRegexCanStart reports whether a '/' after before opens a regex literal
// rather than dividing: true after an operator, an opening bracket, or a
// keyword that takes an expression (return, case, else, ...), false after a
// value. A keyword counts only as a whole word, so admin / 2 divides.
//
// A '/' after ')' is always read as division. That is wrong for a regex
// opening a statement after an if, for or while head (if (x) /re/.test(s)),
// and telling the two apart needs the parser's paren tracking; the reader's
// scripts never write one, and TestJSBlockEnd pins the division reading.
func jsRegexCanStart(before string) bool {
	t := strings.TrimRight(before, " \t\r\n")
	if t == "" {
		return true
	}
	if strings.IndexByte("(,=:[!&|?{};+-*%<>~^", t[len(t)-1]) >= 0 {
		return true
	}
	for _, kw := range jsRegexKeywords {
		if !strings.HasSuffix(t, kw) {
			continue
		}
		rest := t[:len(t)-len(kw)]
		if rest == "" || !isJSIdentByte(rest[len(rest)-1]) {
			return true
		}
	}
	return false
}

// jsRegexKeywords are the keywords a regex literal can follow.
var jsRegexKeywords = []string{
	"await", "case", "delete", "do", "else", "in", "instanceof", "new",
	"return", "throw", "typeof", "void", "yield",
}

func isJSIdentByte(c byte) bool {
	return c == '_' || c == '$' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// wrapperBody is the source range of a selector wrapper's body and the name
// of its selector parameter.
type wrapperBody struct {
	start, end int
	param      string
}

// wrapperBodies finds each wrapper's `function name(params) {` declaration.
func wrapperBodies(src string, wrappers map[string]int) []wrapperBody {
	var out []wrapperBody
	for wrapper, n := range wrappers {
		decl := regexp.MustCompile(`function ` + wrapper + `\(([^)]*)\)\s*\{`)
		for _, m := range decl.FindAllStringSubmatchIndex(src, -1) {
			params := strings.Split(src[m[2]:m[3]], ",")
			if n >= len(params) {
				continue
			}
			open := m[1] - 1
			if end := jsBlockEnd(src, open); end > 0 {
				out = append(out, wrapperBody{start: open, end: end, param: strings.TrimSpace(params[n])})
			}
		}
	}
	return out
}

// scanChromeLookups returns every DOM query call in src, and every call to the
// script's selector wrappers in place of the query inside them. wrapperCalls
// counts the latter per wrapper, so a renamed wrapper cannot pass unseen.
func scanChromeLookups(src string, wrappers map[string]int) (out []chromeLookup, wrapperCalls map[string]int) {
	consts := map[string]string{}
	for _, m := range jsStringVar.FindAllStringSubmatch(src, -1) {
		consts[m[1]] = m[2] + m[3]
	}
	bodies := wrapperBodies(src, wrappers)
	add := func(argStart, n int) {
		expr := jsArg(src, argStart, n)
		l := chromeLookup{key: expr, line: strings.Count(src[:argStart], "\n") + 1}
		if sel, ok := jsResolve(expr, consts); ok {
			l.key, l.rooted = sel, dataFcRooted(sel)
		}
		for _, b := range bodies {
			if argStart > b.start && argStart < b.end && expr == b.param {
				l.passThrough = true
			}
		}
		out = append(out, l)
	}
	for _, m := range domQueryCall.FindAllStringIndex(src, -1) {
		add(m[1], 0)
	}
	wrapperCalls = map[string]int{}
	for wrapper, n := range wrappers {
		call := regexp.MustCompile(`(?:^|[^.\w])` + wrapper + `\(`)
		for _, m := range call.FindAllStringIndex(src, -1) {
			if strings.HasSuffix(src[:m[1]], "function "+wrapper+"(") {
				continue
			}
			wrapperCalls[wrapper]++
			add(m[1], n)
		}
	}
	return out, wrapperCalls
}

func TestChrome_ScriptsFindChromeOnlyByDataFc(t *testing.T) {
	for _, f := range chromeScripts {
		src := chromeRead(t, filepath.Join("assets", f))
		lookups, calls := scanChromeLookups(src, selectorWrappers[f])
		for w := range selectorWrappers[f] {
			if calls[w] == 0 {
				t.Errorf("%s: no call to its selector wrapper %s(); update selectorWrappers", f, w)
			}
		}
		if len(lookups) == 0 {
			t.Errorf("%s: the scan found no DOM lookups; it has stopped seeing them", f)
		}
		seen := map[string]bool{}
		for _, l := range lookups {
			if l.rooted || l.passThrough {
				continue
			}
			seen[l.key] = true
			if _, ok := contentLookups[f][l.key]; !ok {
				t.Errorf("%s:%d looks up %q, which is not rooted at a data-fc hook. "+
					"A doc can carry any class and some tags, so a chrome lookup "+
					"by class or tag can return the doc's copy (forgectl#643). "+
					"Give the chrome element a data-fc hook, or, if this finds "+
					"document content by design, list it in contentLookups with why.",
					f, l.line, l.key)
			}
		}
		for key := range contentLookups[f] {
			if !seen[key] {
				t.Errorf("%s: contentLookups lists %q, which the script no longer looks up; drop the entry", f, key)
			}
		}
	}
	for f := range contentLookups {
		if !slices.Contains(chromeScripts, f) {
			t.Errorf("contentLookups names %s, which is not in chromeScripts", f)
		}
	}
}

// The scan reads served script files only. An inline <script> would escape
// it (and the CSP's script-src 'self' blocks one anyway).
func TestChrome_ShellHasNoInlineScript(t *testing.T) {
	tmpl := chromeRead(t, filepath.Join("templates", "shell.html.tmpl"))
	tags := regexp.MustCompile(`<script\b[^>]*>`).FindAllString(tmpl, -1)
	if len(tags) == 0 {
		t.Fatal("no <script> tags found; the scan has stopped seeing them")
	}
	for _, tag := range tags {
		if !strings.Contains(tag, ` src="`) {
			t.Errorf("inline script %q in the shell; the chrome lookup guard only scans served assets", tag)
		}
	}
}

// Pins what a doc's planted chrome turns into. The tags the sanitizer keeps
// survive as content (so a tag-based lookup would still find them), their
// chrome classes are stripped (forgectl#700, chromeclass.go), and no planted
// element carries a data-fc hook. The class strip makes the data-fc guard
// above a second wall for class lookups rather than the only one; if the
// sanitizer starts keeping data-*, the guard's whole premise is gone.
func TestChrome_PlantedChromeSurvivesOnlyAsContent(t *testing.T) {
	planted := []string{
		`<aside class="outline" data-fc="outline">p1</aside>`,
		`<details class="outline-inline" data-fc="outline-inline"><summary>p2</summary>x</details>`,
		`<div class="sidenav" data-fc="sidenav"><div class="sidenav__group">p3</div></div>`,
		`<div class="doc-body" data-fc="doc-body">p4</div>`,
		`<main class="surface-document" data-fc="doc-main">p5</main>`,
		`<nav class="sidenav" data-fc="sidenav">p6</nav>`,
		`<footer class="statusbar" data-fc="statusbar">p7</footer>`,
	}
	// The h2 gives the page an outline, so the shell carries both outline hooks.
	body := chromePage(t, "# hostile\n\n## real\n\n"+strings.Join(planted, "\n\n")+"\n")
	start := strings.Index(body, `data-fc="doc-body">`)
	end := strings.Index(body, "</main>")
	if start < 0 || end < start {
		t.Fatalf("no doc-body in the page")
	}
	doc := body[start+len(`data-fc="doc-body">`) : end]
	for _, want := range []string{`<aside>p1</aside>`, `<details><summary>p2</summary>`, `<div><div>p3</div></div>`, `<div>p4</div>`} {
		if !strings.Contains(doc, want) {
			t.Errorf("doc content lost %q, or kept a chrome class on it", want)
		}
	}
	for _, m := range classAttr.FindAllStringSubmatch(doc, -1) {
		for _, tok := range strings.Fields(m[1]) {
			if isChromeClass(tok) {
				t.Errorf("planted chrome class %q survived in the doc body", tok)
			}
		}
	}
	// The planted aside precedes the shell's outline in document order, so a
	// tag-based querySelector would return the doc's copy.
	if strings.Index(body, `<aside>p1</aside>`) > strings.Index(body, `<aside class="outline" data-fc=`) {
		t.Errorf("the planted aside no longer precedes the shell's outline")
	}
	if strings.Contains(doc, "data-fc") {
		t.Errorf("a planted data-fc survived the sanitizer:\n%s", doc)
	}
	for _, hook := range []string{"doc-main", "sidenav", "doc-body", "outline", "outline-inline", "statusbar"} {
		if got := strings.Count(body, `data-fc="`+hook+`"`); got != 1 {
			t.Errorf(`data-fc=%q appears %d times, want the shell's one`, hook, got)
		}
	}
}

// A helper that is not a registered wrapper gets no pass for naming its
// parameter like one (forgectl#718): find(".outline") below reaches chrome
// by class, and its querySelector(sel) must be flagged.
func TestChrome_WrapperPassThroughIsKeyedByFunction(t *testing.T) {
	src := "function find(sel) { return document.querySelector(sel); }\n" +
		"find(\".outline\");\n" +
		"function all(sel) { return document.querySelectorAll(sel); }\n" +
		"all('[data-fc=\"sidenav\"] a');\n"
	lookups, calls := scanChromeLookups(src, map[string]int{"all": 0})
	if calls["all"] != 1 {
		t.Fatalf("all() call sites = %d, want 1", calls["all"])
	}
	var flagged, passed []int
	for _, l := range lookups {
		switch {
		case l.passThrough:
			passed = append(passed, l.line)
		case !l.rooted:
			flagged = append(flagged, l.line)
		}
	}
	if !slices.Equal(flagged, []int{1}) {
		t.Errorf("unrooted lookups on lines %v, want [1] (find's querySelector(sel))", flagged)
	}
	if !slices.Equal(passed, []int{3}) {
		t.Errorf("pass-through lookups on lines %v, want [3] (all's querySelectorAll(sel))", passed)
	}
}

// A brace inside a comment or a regex literal in a wrapper's body must not
// end the body early and cost the wrapper its own pass-through lookup.
func TestChrome_WrapperBodySkipsCommentsAndRegexes(t *testing.T) {
	src := "function all(sel) {\n" +
		"  // a stray } in a comment\n" +
		"  /* and { another } here */\n" +
		"  var re = /[}]\\}/;\n" +
		"  return document.querySelectorAll(sel);\n" +
		"}\n" +
		"all('[data-fc=\"sidenav\"] a');\n"
	lookups, _ := scanChromeLookups(src, map[string]int{"all": 0})
	for _, l := range lookups {
		if l.line == 5 && !l.passThrough {
			t.Errorf("querySelectorAll(sel) on line 5 lost its pass-through: the body scan ended early")
		}
		if !l.rooted && !l.passThrough {
			t.Errorf("line %d: unrooted lookup %q", l.line, l.key)
		}
	}
}

// jsBlockEnd finds a block's closing brace past regex literals, including
// ones with an escaped slash or after a keyword, and still reads a slash
// after a value as division (forgectl#759).
func TestJSBlockEnd(t *testing.T) {
	for _, tc := range []struct {
		name, src string
		want      int // index of the closing brace, or -1
	}{
		{"escaped slash in a regex", `{ var r = /^\/{/; }`, 18},
		{"escaped slash then a class", `{ r = /\/[{]/; }`, 15},
		{"escape at the end of input", `{ r = /\`, -1},
		{"regex after case", `{ switch (c) { case /{/.test(s): } }`, 35},
		{"regex after else", `{ if (a) {} else /{/.test(s); }`, 30},
		{"regex after void", `{ void /{/; }`, 12},
		{"regex after in", `{ k in /{/; }`, 12},
		{"regex after return", `{ return /{/; }`, 14},
		{"division after an identifier ending in a keyword", `{ y = admin / 2; w = { a: 1 }; }`, 31},
		{"division after a closing paren", `{ y = (a) / 2; w = { a: 1 }; }`, 29},
	} {
		if got := jsBlockEnd(tc.src, 0); got != tc.want {
			t.Errorf("%s: jsBlockEnd(%q) = %d, want %d", tc.name, tc.src, got, tc.want)
		}
	}
}
