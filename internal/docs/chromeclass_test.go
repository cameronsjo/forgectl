package docs

import (
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

func TestRender_StripsChromeClassesFromAuthorHTML(t *testing.T) {
	src := strings.Join([]string{
		`<div class="scrim">SCRIM</div>`,
		``,
		`<aside class="outline note">ASIDE</aside>`,
		``,
		`<div class="statusbar"><span class="status-item trust-badge--stale">STATUS</span></div>`,
		``,
		`<div class="sidenav__group kv tree__row">KEEP</div>`,
		``,
		`<p class="outlined">NOT-A-FAMILY</p>`,
		``,
		`Prose that quotes class="statusbar" literally.`,
		``,
		"```go",
		`var x = "class=\"scrim\""`,
		"```",
		``,
	}, "\n")
	got, err := Render([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"SCRIM", "ASIDE", "STATUS", "KEEP", "NOT-A-FAMILY"} {
		if !strings.Contains(got, text) {
			t.Errorf("element %s lost; only its chrome classes should go:\n%s", text, got)
		}
	}
	for _, m := range classAttr.FindAllStringSubmatch(got, -1) {
		for _, tok := range strings.Fields(m[1]) {
			if isChromeClass(tok) {
				t.Errorf("chrome class %q survived in %q", tok, m[0])
			}
		}
	}
	for _, want := range []string{
		`<aside class="note">`,
		`<div class="kv tree__row">`,
		`<p class="outlined">`,
		`<div>SCRIM</div>`,
		`<span>STATUS</span>`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("want %s in:\n%s", want, got)
		}
	}
	// Quoted text is not an attribute, and chroma's generated classes stay.
	if !strings.Contains(got, "class=&#34;statusbar&#34;") && !strings.Contains(got, "class=&quot;statusbar&quot;") {
		t.Errorf("prose quoting a chrome class changed:\n%s", got)
	}
	if !strings.Contains(got, `<pre class="chroma">`) {
		t.Errorf("chroma's generated class went missing:\n%s", got)
	}
}

func TestIsChromeClass(t *testing.T) {
	for tok, want := range map[string]bool{
		"statusbar": true, "sidenav__group": true, "outline-item": true,
		"trust-badge--stale": true, "scrim": true, "nav-scrim": true,
		"outlined": false, "scrimmage": false, "note": false, "tree__row": false,
		"chroma": false, "language-go": false, "callout": false,
	} {
		if got := isChromeClass(tok); got != want {
			t.Errorf("isChromeClass(%q) = %v, want %v", tok, got, want)
		}
	}
}

// sharedClassFamilies are classes the shell template uses that are also
// ordinary content vocabulary: Artificer components a doc may use, with no
// chrome position or chrome meaning. "h" is outline-item's h{{.Level}}.
var sharedClassFamilies = []string{
	"container", "count", "h", "icon", "kv", "label", "meta", "note", "num",
	"opt", "path", "search", "sr-only", "table", "tree", "words",
}

func inFamilies(tok string, families []string) bool {
	for _, f := range families {
		if tok == f || (strings.HasPrefix(tok, f) && len(tok) > len(f) && (tok[len(f)] == '-' || tok[len(f)] == '_')) {
			return true
		}
	}
	return false
}

// Every class the shell uses has to be classified: a new chrome class
// added to the template without a family here fails this test.
func TestChromeClasses_CoverShellTemplate(t *testing.T) {
	tmpl := chromeRead(t, filepath.Join("templates", "shell.html.tmpl"))
	action := regexp.MustCompile(`\{\{[^}]*\}\}`)
	attr := regexp.MustCompile(`\sclass="([^"]*)"`)
	seen := 0
	for _, m := range attr.FindAllStringSubmatch(tmpl, -1) {
		for _, tok := range strings.Fields(action.ReplaceAllString(m[1], " ")) {
			seen++
			if !isChromeClass(tok) && !inFamilies(tok, sharedClassFamilies) {
				t.Errorf("shell class %q is neither a chrome family (chromeclass.go) nor shared vocabulary (sharedClassFamilies)", tok)
			}
		}
	}
	if seen < 40 {
		t.Fatalf("found only %d class tokens in the shell template; the scan is broken", seen)
	}
}

// fixedStickyExempt are classes in a position:fixed/sticky rule that stay
// usable in a doc, with the reason each cannot leave the doc pane.
var fixedStickyExempt = map[string]string{
	"table--sticky-head":   "sticky inside its own table's scroll box",
	"table--sticky-col":    "sticky inside its own table's scroll box",
	"table--responsive":    "only neutralizes the sticky-col combo (position: static)",
	"table":                "qualifier on the table--sticky-* rules",
	"theme-toggle--inline": "sets position: static",
}

// Every class in a fixed or sticky rule of a stylesheet the shell links is
// denied, so a doc cannot pin anything over the reader.
func TestChromeClasses_CoverFixedAndStickyCSS(t *testing.T) {
	tmpl := chromeRead(t, filepath.Join("templates", "shell.html.tmpl"))
	inline := regexp.MustCompile(`(?s)<style>(.*?)</style>`).FindStringSubmatch(tmpl)
	if inline == nil {
		t.Fatal("no inline <style> in the shell template")
	}
	sheets := map[string]string{"shell <style>": inline[1]}
	for _, rel := range []string{
		filepath.Join("assets", "artificer", "artificer.css"),
		filepath.Join("assets", "chroma.css"),
		filepath.Join("assets", "diagram.css"),
		filepath.Join("assets", "katex", "katex.min.css"),
	} {
		sheets[rel] = chromeRead(t, rel)
	}
	comment := regexp.MustCompile(`(?s)/\*.*?\*/`)
	pinned := regexp.MustCompile(`position\s*:\s*(fixed|sticky)`)
	class := regexp.MustCompile(`\.([A-Za-z_][\w-]*)`)
	rules := 0
	for name, css := range sheets {
		for _, chunk := range strings.Split(comment.ReplaceAllString(css, ""), "}") {
			parts := strings.Split(chunk, "{")
			if len(parts) < 2 || !pinned.MatchString(parts[len(parts)-1]) {
				continue
			}
			rules++
			for _, m := range class.FindAllStringSubmatch(parts[len(parts)-2], -1) {
				tok := m[1]
				if _, ok := fixedStickyExempt[tok]; ok || isChromeClass(tok) {
					continue
				}
				t.Errorf("%s: class %q sits in a position: fixed/sticky rule but a doc may wear it; add its family to chromeClassFamilies or exempt it with a reason", name, tok)
			}
		}
	}
	if rules < 8 {
		t.Fatalf("found only %d fixed/sticky rules; the scan is broken", rules)
	}
}

// mermaid-init.js strips the same chrome families from rendered diagrams,
// whose classes never pass through the Go strip (forgectl#745). The two
// lists must match.
func TestChromeClasses_MermaidInitMirrorsGoList(t *testing.T) {
	src := chromeRead(t, filepath.Join("assets", "mermaid-init.js"))
	m := regexp.MustCompile(`(?s)var CHROME_CLASS_FAMILIES = \[(.*?)\];`).FindStringSubmatch(src)
	if m == nil {
		t.Fatal("no CHROME_CLASS_FAMILIES array in mermaid-init.js")
	}
	var js []string
	for _, q := range regexp.MustCompile(`"([^"]+)"`).FindAllStringSubmatch(m[1], -1) {
		js = append(js, q[1])
	}
	if !slices.Equal(js, chromeClassFamilies) {
		t.Errorf("mermaid-init.js CHROME_CLASS_FAMILIES = %v\nchromeclass.go chromeClassFamilies = %v", js, chromeClassFamilies)
	}
}

// The doc pane is the containing block and paint clip for everything a doc
// renders (forgectl#745): a doc can wear Artificer's position:absolute
// classes (.tooltip, .phase::before), and without this they anchor to the
// page and paint over the chrome.
func TestShell_DocPaneContainsPositionedContent(t *testing.T) {
	tmpl := chromeRead(t, filepath.Join("templates", "shell.html.tmpl"))
	if !regexp.MustCompile(`<main\b[^>]*class="surface-document[^"]*"[^>]*data-fc="doc-main"`).MatchString(tmpl) {
		t.Fatal(`the doc pane is no longer <main class="surface-document ..." data-fc="doc-main">; the rule below targets it`)
	}
	style := regexp.MustCompile(`(?s)<style>(.*?)</style>`).FindStringSubmatch(tmpl)[1]
	rules, nested, problems := docPaneContainment(style)
	for _, p := range problems {
		t.Error(p)
	}
	// The template has four rules on the pane itself, one of them inside an
	// @media block; fewer means the scan is broken, not the CSS fixed.
	if rules < 4 || nested < 1 {
		t.Fatalf("found %d rules on the doc pane, %d inside an at-rule; the scan is broken", rules, nested)
	}
}

// Every rule on the doc pane counts, an at-rule's included, since a later
// or narrower rule overrides the base one (forgectl#759).
func TestDocPaneContainment_CatchesEveryOverride(t *testing.T) {
	base := "main.surface-document { position: relative; contain: paint; }\n"
	for _, tc := range []struct{ name, css string }{
		{"media contain none", "@media (max-width: 900px) {\n  main.surface-document { padding: 0; contain: none; }\n}"},
		{"media position static", "@media print { main.surface-document { position: static; } }"},
		{"nested at-rules", "@supports (contain: paint) { @media (min-width: 1px) { main.surface-document { contain: layout; } } }"},
		{"pseudo-class", "main.surface-document:hover { position: static; }"},
		{"selector list", "h1, main.surface-document { contain: none; }"},
		{"bare class", ".surface-document { position: sticky; }"},
		{"all reset", "main.surface-document { all: unset; }"},
		{"important", "main.surface-document { contain: size !important; }"},
		{"by id", "#doc-main { contain: none; }"},
		{"by data-fc", `main[data-fc="doc-main"] { position: static; }`},
	} {
		if _, _, problems := docPaneContainment(base + tc.css); len(problems) == 0 {
			t.Errorf("%s: no problem reported for %q", tc.name, tc.css)
		}
	}
	for _, css := range []string{
		base,
		base + "@media (max-width: 900px) { main.surface-document { padding: 0; } }",
		base + ".surface-document * { position: static; } .surface-document::before { position: absolute; }",
		base + "main.surface-document { contain: strict !important; }",
		base + ".surface-document-x { position: static; } #doc-main-x { contain: none; } [data-fc=\"doc-main-x\"] { contain: none; }",
	} {
		if _, _, problems := docPaneContainment(css); len(problems) > 0 {
			t.Errorf("%q: unexpected problems %v", css, problems)
		}
	}
	if _, _, problems := docPaneContainment("main.surface-document { padding: 0; }"); len(problems) != 2 {
		t.Errorf("a pane with neither declaration: problems = %v, want both missing", problems)
	}
}

var (
	cssComment = regexp.MustCompile(`(?s)/\*.*?\*/`)
	// cssRule matches an innermost rule, so a rule inside @media or
	// @supports matches on its own and the at-rule's prelude never does.
	cssRule = regexp.MustCompile(`([^{}]*)\{([^{}]*)\}`)
	// docPaneHook is a simple selector the pane element answers to in the
	// shell template: <main id="doc-main" class="surface-document ..."
	// data-fc="doc-main">. The pane's other classes (container,
	// container--lg) and the bare main type selector are not read: Artificer
	// uses them for other elements too, so a rule on them does not target
	// the pane alone. Nor is a hook inside a functional pseudo-class told
	// apart (:not(.surface-document) reads as on the pane).
	docPaneHook  = regexp.MustCompile(`(\.surface-document|#doc-main)([^\w-]|$)|\[data-fc=["']?doc-main["']?\]`)
	paintContain = regexp.MustCompile(`\b(paint|content|strict)\b`)
)

// docPaneContainment reads every rule on the doc pane in css, at any at-rule
// depth, and reports each declaration that would undo its containment:
// position other than relative, contain without paint, or an all reset. It
// also reports when no rule sets either one. rules counts the pane's rules,
// nested those inside an at-rule.
func docPaneContainment(css string) (rules, nested int, problems []string) {
	css = cssComment.ReplaceAllString(css, "")
	var relative, contained bool
	for _, m := range cssRule.FindAllStringSubmatchIndex(css, -1) {
		onPane := false
		for _, sel := range strings.Split(css[m[2]:m[3]], ",") {
			sel = strings.TrimSpace(sel)
			// A compound selector on the pane itself: no combinator, no
			// pseudo-element.
			if docPaneHook.MatchString(sel) && !strings.ContainsAny(sel, " \t\n>+~") && !strings.Contains(sel, "::") {
				onPane = true
			}
		}
		if !onPane {
			continue
		}
		rules++
		prefix := css[:m[0]]
		if strings.Count(prefix, "{") > strings.Count(prefix, "}") {
			nested++
		}
		selector := strings.TrimSpace(css[m[2]:m[3]])
		for _, decl := range strings.Split(css[m[4]:m[5]], ";") {
			prop, value, ok := strings.Cut(decl, ":")
			if !ok {
				continue
			}
			prop = strings.ToLower(strings.TrimSpace(prop))
			value = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(value), "!important"))
			switch prop {
			case "position":
				if value == "relative" {
					relative = true
				} else {
					problems = append(problems, fmt.Sprintf("%s sets position: %s; a doc's absolute-positioned classes anchor to the page", selector, value))
				}
			case "contain":
				if paintContain.MatchString(value) {
					contained = true
				} else {
					problems = append(problems, fmt.Sprintf("%s sets contain: %s, which has no paint containment; a doc's positioned content can paint over the chrome", selector, value))
				}
			case "all":
				problems = append(problems, fmt.Sprintf("%s sets all: %s, which resets the pane's position and containment", selector, value))
			}
		}
	}
	if !relative {
		problems = append(problems, "no rule makes main.surface-document position: relative; a doc's absolute-positioned classes anchor to the page")
	}
	if !contained {
		problems = append(problems, "no rule gives main.surface-document paint containment; a doc's positioned content can paint over the chrome")
	}
	return rules, nested, problems
}
