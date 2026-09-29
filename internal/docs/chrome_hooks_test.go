package docs

// Test plan for the reader's chrome lookups (forgectl#617, #583)
//
// The reader's JS and CSS find chrome elements by a data-fc attribute, never
// by id. A document can produce any id (a heading slug, or raw HTML), so an id
// lookup can be captured by content; the sanitizer strips every data-*
// attribute, so a doc cannot produce a data-fc.
//
//   [x] Happy: a doc whose headings slug to every chrome id leaves each
//              data-fc hook on exactly one element, the chrome's
//   [x] Edge:  raw-HTML data-fc in a doc is stripped
//   [x] Happy: reload.js, nav-toggle.js and sidenav-filter.js never call
//              getElementById on a chrome id, and every data-fc they query
//              exists in the shell
//   [x] Happy: the shell CSS does not style chrome by id
//   [x] Happy: the live-status item keeps the full text in a title and wraps
//              the host in a span the 480px rule can hide

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var chromeIDs = []string{
	"shell", "nav-toggle", "drawer-scrim", "docs-nav", "doc-filter",
	"filter-empty", "live-status", "doc-missing",
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

func TestChrome_ScriptsLookUpByDataFcNotID(t *testing.T) {
	body := chromePage(t, "# x\n")
	hook := regexp.MustCompile(`\[data-fc="([a-z-]+)"\]`)
	for _, f := range []string{"reload.js", "nav-toggle.js", "sidenav-filter.js"} {
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
