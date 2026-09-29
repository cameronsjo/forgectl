package docs

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestServer_Katex_AssetsServed pins the KaTeX script, stylesheet and init
// script to their routes: the embedded bytes, exactly, under a strict content
// type (nosniff is on, so a wrong type is a stylesheet or script the browser
// refuses to apply).
func TestServer_Katex_AssetsServed(t *testing.T) {
	idx, _ := testIndex(t)
	h := testHandler(idx)

	for _, tc := range []struct {
		path, contentType string
		body              []byte
	}{
		{"/assets/katex/katex.min.js", "text/javascript; charset=utf-8", katexJS},
		{"/assets/katex/katex.min.css", "text/css; charset=utf-8", katexCSS},
		{"/assets/math-init.js", "text/javascript; charset=utf-8", mathInitJS},
	} {
		t.Run(tc.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, tc.path, nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d, want 200", rec.Code)
			}
			if ct := rec.Header().Get("Content-Type"); ct != tc.contentType {
				t.Errorf("Content-Type %q, want %q", ct, tc.contentType)
			}
			if len(tc.body) == 0 || !bytes.Equal(rec.Body.Bytes(), tc.body) {
				t.Errorf("body (%d bytes) is not the embedded asset (%d bytes)", rec.Body.Len(), len(tc.body))
			}
		})
	}
}

// TestServer_Katex_FontsServedWhereTheCSSLooks walks every woff2 url(…) in
// katex.min.css, resolved against the stylesheet's own route, and requires
// each to be served as font/woff2 with the embedded bytes. A missing font
// does not fail visibly: KaTeX falls back to a system face and the math
// renders with the wrong glyph metrics.
func TestServer_Katex_FontsServedWhereTheCSSLooks(t *testing.T) {
	idx, _ := testIndex(t)
	h := testHandler(idx)

	matches := regexp.MustCompile(`url\((fonts/[^)]+\.woff2)\)`).FindAllStringSubmatch(string(katexCSS), -1)
	if len(matches) != 20 {
		t.Fatalf("katex.min.css names %d woff2 fonts, want the 20 katex 0.18.9 ships", len(matches))
	}
	for _, m := range matches {
		path := "/assets/katex/" + m[1]
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("%s: status %d, want 200 — katex.min.css references a font the reader does not serve", path, rec.Code)
			continue
		}
		if ct := rec.Header().Get("Content-Type"); ct != "font/woff2" {
			t.Errorf("%s: Content-Type %q, want font/woff2", path, ct)
		}
		want, err := katexFonts.ReadFile("assets/katex/" + m[1])
		if err != nil || !bytes.Equal(rec.Body.Bytes(), want) {
			t.Errorf("%s: body is not the embedded font", path)
		}
	}
}

// TestServer_Katex_OnlyVendoredFontsResolve: the font route serves the
// embedded woff2 set and nothing else — not the woff/ttf fallbacks the CSS
// also names (never vendored), not the other font set, not a sibling file.
func TestServer_Katex_OnlyVendoredFontsResolve(t *testing.T) {
	idx, _ := testIndex(t)
	h := testHandler(idx)
	for _, path := range []string{
		"/assets/katex/fonts/nope.woff2",
		"/assets/katex/fonts/KaTeX_Main-Regular.woff",
		"/assets/katex/fonts/KaTeX_Main-Regular.ttf",
		"/assets/katex/fonts/..%2fkatex.min.css",
		"/assets/katex/fonts/..%2f..%2fprovenance-katex.json",
		"/assets/katex/fonts/jetbrains-mono-400.woff2",
		"/assets/assets/fonts/KaTeX_Main-Regular.woff2",
		"/assets/katex/provenance-katex.json",
		"/assets/provenance-katex.json",
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", path, rec.Code)
		}
	}
}

// TestShell_LoadsKatex pins the page wiring: the stylesheet, and the two
// deferred scripts in dependency order (deferred scripts run in document
// order, so katex.min.js must come before math-init.js).
func TestShell_LoadsKatex(t *testing.T) {
	idx, label := testIndex(t)
	h := testHandler(idx)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/doc/"+label+"/welcome.md", nil))
	page := rec.Body.String()

	if !strings.Contains(page, `<link rel="stylesheet" href="/assets/katex/katex.min.css"/>`) {
		t.Error("shell does not link katex.min.css")
	}
	lib := strings.Index(page, `<script src="/assets/katex/katex.min.js" defer></script>`)
	initJS := strings.Index(page, `<script src="/assets/math-init.js" defer></script>`)
	if lib < 0 || initJS < 0 {
		t.Fatalf("shell is missing a KaTeX script tag (katex.min.js at %d, math-init.js at %d)", lib, initJS)
	}
	if initJS < lib {
		t.Error("math-init.js is loaded before katex.min.js; it would run with katex undefined")
	}
}

// katexProvenance is the shape of assets/provenance-katex.json.
type katexProvenance struct {
	Files []struct {
		Path    string `json:"path"`
		Version string `json:"version"`
		License string `json:"license"`
		SHA256  string `json:"sha256"`
		Bytes   int    `json:"bytes"`
	} `json:"files"`
}

// TestKatexProvenance holds the embedded KaTeX to provenance-katex.json in
// both directions: every recorded file is embedded with the recorded sha256
// and size, and every embedded file is recorded. A re-vendor that forgets to
// regenerate the provenance, a hand edit to a vendored file, or a font added
// without a record all fail here.
func TestKatexProvenance(t *testing.T) {
	raw, err := os.ReadFile("assets/provenance-katex.json")
	if err != nil {
		t.Fatal(err)
	}
	var prov katexProvenance
	if err := json.Unmarshal(raw, &prov); err != nil {
		t.Fatal(err)
	}

	embedded := map[string][]byte{
		"katex/katex.min.js":  katexJS,
		"katex/katex.min.css": katexCSS,
	}
	err = fs.WalkDir(katexFonts, ".", func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() {
			return walkErr
		}
		body, readErr := katexFonts.ReadFile(path)
		embedded[strings.TrimPrefix(path, "assets/")] = body
		return readErr
	})
	if err != nil {
		t.Fatal(err)
	}

	recorded := map[string]bool{}
	for _, f := range prov.Files {
		recorded[f.Path] = true
		if f.Version != "0.18.9" || f.License != "MIT" {
			t.Errorf("%s: version %q license %q, want 0.18.9 MIT", f.Path, f.Version, f.License)
		}
		body, ok := embedded[f.Path]
		if !ok {
			t.Errorf("%s: recorded in provenance but not embedded", f.Path)
			continue
		}
		sum := sha256.Sum256(body)
		if got := hex.EncodeToString(sum[:]); got != f.SHA256 {
			t.Errorf("%s: sha256 %s, provenance records %s", f.Path, got, f.SHA256)
		}
		if len(body) != f.Bytes {
			t.Errorf("%s: %d bytes, provenance records %d", f.Path, len(body), f.Bytes)
		}
	}
	for path := range embedded {
		if !recorded[path] {
			t.Errorf("%s: embedded but has no provenance record", path)
		}
	}
	if len(prov.Files) != 22 {
		t.Errorf("provenance records %d files, want 22 (script, stylesheet, 20 fonts)", len(prov.Files))
	}
}

// TestServer_CopyJS_ServedAndLinked pins the rich-copy script (forgectl#588) to
// its route and to the shell. script-src 'self' forbids inline script, so an
// unlinked or unserved file means copy silently falls back to Chromium's
// theme-styled HTML.
func TestServer_CopyJS_ServedAndLinked(t *testing.T) {
	idx, _ := testIndex(t)
	h := testHandler(idx)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/assets/copy.js", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/javascript; charset=utf-8" {
		t.Errorf("Content-Type %q", ct)
	}
	if len(copyJS) == 0 || !bytes.Equal(rec.Body.Bytes(), copyJS) {
		t.Error("body is not the embedded copy.js")
	}
	// Static guard: one addEventListener call whose first argument is "copy"
	// or 'copy', whatever the spacing. The runtime guard is the Playwright
	// check that instruments EventTarget.addEventListener on a live page.
	if n := len(regexp.MustCompile(`addEventListener\(\s*["']copy["']`).FindAllString(string(copyJS), -1)); n != 1 {
		t.Errorf("copy.js registers %d copy listeners, want exactly 1", n)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil))
	if !strings.Contains(rec.Body.String(), `<script src="/assets/copy.js" defer></script>`) {
		t.Error("shell does not link /assets/copy.js")
	}
}
