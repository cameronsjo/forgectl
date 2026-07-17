package docs

// Test plan for server.go
//
// NewHandler (Classification: API handler)
//   [x] Happy: "/" renders the shell with the empty-state
//   [x] Happy: a valid /doc/{root}/{rest} renders the doc's content
//   [x] Happy: static assets (artificer.css, artificer-theme.js, chroma.css) are served
//   [x] Unhappy (security): a traversal attempt through the HTTP route 404s
//   [x] Unhappy (security): an unknown root label 404s
//   [x] Unhappy (security): a disallowed extension under a known root 404s
//   [x] Happy: the sidenav lists the indexed doc with a matching href

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func testIndex(t *testing.T) (*Index, string) {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "welcome.md"), "# Welcome\n\nhello **world**\n")
	writeFile(t, filepath.Join(dir, "secret.env"), "API_KEY=xyz")
	idx, err := NewIndex([]string{dir})
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	return idx, idx.Roots()[0].Label
}

func TestServer_Root_RendersEmptyState(t *testing.T) {
	idx, _ := testIndex(t)
	h := NewHandler(idx)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if !strings.Contains(rec.Body.String(), "No doc selected") {
		t.Errorf("body missing empty-state copy: %s", rec.Body.String())
	}
}

func TestServer_ValidDoc_RendersContent(t *testing.T) {
	idx, label := testIndex(t)
	h := NewHandler(idx)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/doc/"+label+"/welcome.md", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "<strong>world</strong>") {
		t.Errorf("body missing rendered markdown: %s", body)
	}
	if !strings.Contains(body, "Welcome") {
		t.Errorf("body missing doc title: %s", body)
	}
}

func TestServer_StaticAssets_Served(t *testing.T) {
	idx, _ := testIndex(t)
	h := NewHandler(idx)

	for _, path := range []string{"/assets/artificer.css", "/assets/artificer-theme.js", "/assets/chroma.css"} {
		t.Run(path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
			}
			if rec.Body.Len() == 0 {
				t.Error("empty response body")
			}
		})
	}
}

// doRequestFollowingOneRedirect drives req through h and, if the response is
// a redirect (Go's stdlib ServeMux 307s a request whose path contains a
// literal "../" segment before our handler ever sees it — its own,
// additional defense-in-depth layer ahead of ours), replays the Location
// once more through the same handler. This mirrors what a real browser
// would do, so a traversal attempt is judged on its EVENTUAL outcome, not
// just the first hop.
func doRequestFollowingOneRedirect(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if loc := rec.Header().Get("Location"); (rec.Code == http.StatusMovedPermanently || rec.Code == http.StatusTemporaryRedirect) && loc != "" {
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, loc, nil))
	}
	return rec
}

func TestServer_TraversalAttempt_404s(t *testing.T) {
	idx, label := testIndex(t)
	h := NewHandler(idx)

	cases := []string{
		"/doc/" + label + "/../../../../../../etc/passwd",
		"/doc/" + label + "/..%2f..%2fetc%2fpasswd",
	}
	for _, path := range cases {
		t.Run(path, func(t *testing.T) {
			rec := doRequestFollowingOneRedirect(t, h, path)
			if rec.Code != http.StatusNotFound {
				t.Errorf("final status = %d, want %d for %q; body: %s", rec.Code, http.StatusNotFound, path, rec.Body.String())
			}
			if strings.Contains(rec.Body.String(), "root:") {
				t.Errorf("response body appears to leak /etc/passwd contents: %s", rec.Body.String())
			}
		})
	}
}

func TestServer_UnknownRoot_404s(t *testing.T) {
	idx, _ := testIndex(t)
	h := NewHandler(idx)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/doc/no-such-root/welcome.md", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestServer_DisallowedExtension_404s(t *testing.T) {
	idx, label := testIndex(t)
	h := NewHandler(idx)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/doc/"+label+"/secret.env", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestServer_Sidenav_ListsIndexedDoc(t *testing.T) {
	idx, label := testIndex(t)
	h := NewHandler(idx)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	wantHref := `href="/doc/` + label + `/welcome.md"`
	if !strings.Contains(rec.Body.String(), wantHref) {
		t.Errorf("sidenav missing %q in body: %s", wantHref, rec.Body.String())
	}
}
