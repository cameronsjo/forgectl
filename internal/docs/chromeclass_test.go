package docs

import (
	"path/filepath"
	"regexp"
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
