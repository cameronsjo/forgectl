package docs

// Test plan for server.go
//
// NewHandler (Classification: API handler)
//   [x] Happy: "/" renders the shell with the empty-state
//   [x] Happy: a valid /doc/{root}/{rest} renders the doc's content
//   [x] Happy: static assets (artificer.css, artificer-theme.js, reload.js, chroma.css) are served
//   [x] Unhappy (security): a traversal attempt through the HTTP route 404s
//   [x] Unhappy (security): an unknown root label 404s
//   [x] Unhappy (security): a disallowed extension under a known root 404s
//   [x] Happy: the sidenav lists the indexed doc with a matching href
//   [x] Happy: every response carries X-Content-Type-Options: nosniff
//   [x] Happy (security): every response carries the Content-Security-Policy
//   [x] Unhappy (security): the shell contains NO inline <script> — every
//       <script> tag carries a src=. The CSP sets script-src 'self', so an
//       inline script would be blocked in the browser and silent to Go; this
//       is the test that keeps the policy and the markup honest together
//   [x] Unhappy (security): every asset the shell references is same-origin —
//       no absolute or protocol-relative URL survives default-src 'self'
//
// handleLocate (Classification: security-sensitive — membership disclosure)
//   [x] Unhappy: a missing "path" query param 400s
//   [x] Unhappy: a path that cannot be resolved (does not exist on disk) 404s
//   [x] Unhappy: a real, resolvable path that was never indexed 404s
//   [x] Happy: an indexed doc's absolute path 200s with {root, rel, title}
//   [x] Unhappy (security): a file under an excluded directory (.trash/) 404s
//              EVEN THOUGH it exists on disk — membership disclosure must
//              follow the same exclusion the walk applies, not raw
//              filesystem existence

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
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

// testHandler wraps idx in the Store/Broker pair NewHandler now takes, for the
// majority of tests that exercise a static index and never touch live reload.
// Tests that DO care about swapping or streaming build their own Store/Broker
// so they can reach them.
func testHandler(idx *Index) http.Handler {
	return NewHandler(NewStore(idx), NewBroker())
}

func TestServer_Root_RendersEmptyState(t *testing.T) {
	idx, _ := testIndex(t)
	h := testHandler(idx)

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
	h := testHandler(idx)

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
	h := testHandler(idx)

	for _, path := range []string{"/assets/artificer.css", "/assets/artificer-theme.js", "/assets/reload.js", "/assets/chroma.css", "/assets/sidenav-filter.js", "/assets/reader.css", "/assets/reader-shell.js", "/assets/reader-settings.js"} {
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

func TestServer_ShellIncludesPersistedReadingControls(t *testing.T) {
	idx, _ := testIndex(t)
	rec := httptest.NewRecorder()
	testHandler(idx).ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil))

	body := rec.Body.String()
	for _, want := range []string{
		`data-reader-setting="bodyFont"`,
		`data-reader-setting="headingFont"`,
		`data-reader-setting="codeFont"`,
		`data-reader-setting="fontSize"`,
		`data-reader-setting="lineHeight"`,
		`data-reader-setting="measure"`,
		`src="/assets/reader-settings.js"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("shell missing %q", want)
		}
	}
}

func TestServer_ShellUsesReadingFirstNavigation(t *testing.T) {
	idx, label := testIndex(t)
	rec := httptest.NewRecorder()
	testHandler(idx).ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/doc/"+label+"/welcome.md", nil))

	body := rec.Body.String()
	for _, want := range []string{
		`data-docs-nav-toggle`,
		`aria-controls="docs-navigation"`,
		`aria-expanded="false"`,
		`data-docs-nav aria-hidden="true"`,
		`data-docs-nav-scrim`,
		`src="/assets/reader-shell.js"`,
		`class="reader-toolbar__title">Welcome</span>`,
		`class="theme-toggle theme-toggle--inline"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("shell missing %q", want)
		}
	}
	if strings.Contains(body, `class="split-pane"`) {
		t.Error("shell still uses a persistent or stacking split-pane navigator")
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
	h := testHandler(idx)

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
	h := testHandler(idx)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/doc/no-such-root/welcome.md", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestServer_DisallowedExtension_404s(t *testing.T) {
	idx, label := testIndex(t)
	h := testHandler(idx)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/doc/"+label+"/secret.env", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestServer_Sidenav_ListsIndexedDoc(t *testing.T) {
	idx, label := testIndex(t)
	h := testHandler(idx)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	wantHref := `href="/doc/` + label + `/welcome.md"`
	if !strings.Contains(rec.Body.String(), wantHref) {
		t.Errorf("sidenav missing %q in body: %s", wantHref, rec.Body.String())
	}
}

func TestServer_Locate_MissingPath_400s(t *testing.T) {
	idx, _ := testIndex(t)
	h := testHandler(idx)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/locate", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestServer_Locate_UnresolvablePath_404s(t *testing.T) {
	idx, _ := testIndex(t)
	h := testHandler(idx)

	q := url.Values{"path": []string{filepath.Join(t.TempDir(), "never-created.md")}}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/locate?"+q.Encode(), nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestServer_Locate_PathNotInIndex_404s(t *testing.T) {
	idx, _ := testIndex(t)
	h := testHandler(idx)

	// A real file that exists on disk but was never indexed (a different,
	// unconfigured directory) must still 404 — handleLocate answers
	// membership in THIS index, not "does this path exist on disk".
	dir := t.TempDir()
	outside := filepath.Join(dir, "outside.md")
	writeFile(t, outside, "# Outside")
	resolved, err := filepath.EvalSymlinks(outside)
	if err != nil {
		t.Fatal(err)
	}

	q := url.Values{"path": []string{resolved}}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/locate?"+q.Encode(), nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestServer_Locate_IndexedDoc_200sWithRootRelTitle(t *testing.T) {
	idx, label := testIndex(t)
	h := testHandler(idx)

	doc, ok := idx.Find(label, "welcome.md")
	if !ok {
		t.Fatal("fixture doc \"welcome.md\" not found in the index")
	}

	q := url.Values{"path": []string{doc.AbsPath}}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/locate?"+q.Encode(), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var got locateResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.Root != label || got.Rel != "welcome.md" || got.Title != "Welcome" {
		t.Errorf("locate response = %+v, want {Root:%q Rel:welcome.md Title:Welcome}", got, label)
	}
}

// TestServer_Locate_ExcludedDir_404sEvenThoughFileExistsOnDisk is the
// handleLocate sibling of TestIndex_Resolve_ExcludedDir_NotServableByDirectURL:
// a file that genuinely exists under a directory walkRoot deliberately skips
// (.trash) must not be locatable, even by a caller who already knows its
// exact absolute path. Confirming membership for such a file would leak that
// something is hidden there — a stranger who already possesses the path
// learns whether the reader is quietly aware of it.
func TestServer_Locate_ExcludedDir_404sEvenThoughFileExistsOnDisk(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "kept.md"), "# Kept")
	trashFile := filepath.Join(dir, ".trash", "deleted-secret.md")
	writeFile(t, trashFile, "# Should never be located")

	idx, err := NewIndex([]string{dir})
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	h := testHandler(idx)

	resolved, err := filepath.EvalSymlinks(trashFile)
	if err != nil {
		t.Fatal(err)
	}

	q := url.Values{"path": []string{resolved}}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/locate?"+q.Encode(), nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d for a file under an excluded directory — handleLocate must not disclose membership for files walkRoot deliberately skipped, even though the file genuinely exists on disk", rec.Code, http.StatusNotFound)
	}
}

func TestServer_ResponsesCarryNoSniffHeader(t *testing.T) {
	idx, label := testIndex(t)
	h := testHandler(idx)

	paths := []string{"/", "/doc/" + label + "/welcome.md", "/assets/artificer.css"}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
			if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
				t.Errorf("X-Content-Type-Options = %q, want %q", got, "nosniff")
			}
		})
	}
}

// TestServer_ResponsesCarryCSPHeader covers every response SHAPE the handler
// produces, not just the rendered page: the shell, a doc, a static asset, a
// JSON payload, and a 404. securityHeaders wraps the whole mux precisely so
// none of these is an exception, and this route table is what holds that claim
// to account — a header applied in renderShell alone would pass a shell-only
// test and leave the other four uncovered.
func TestServer_ResponsesCarryCSPHeader(t *testing.T) {
	idx, label := testIndex(t)
	h := testHandler(idx)

	doc, ok := idx.Find(label, "welcome.md")
	if !ok {
		t.Fatal("fixture doc \"welcome.md\" not found in the index")
	}
	locateQuery := url.Values{"path": []string{doc.AbsPath}}.Encode()

	paths := []string{
		"/",
		"/doc/" + label + "/welcome.md",
		"/assets/artificer.css",
		locatePath + "?" + locateQuery,      // a 200 JSON payload
		"/doc/" + label + "/no-such-doc.md", // a 404
	}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
			got := rec.Header().Get("Content-Security-Policy")
			if got != contentSecurityPolicy {
				t.Errorf("Content-Security-Policy = %q, want %q — the policy is set on the whole mux, so assets and JSON carry it as well as the rendered shell", got, contentSecurityPolicy)
			}
		})
	}
}

// scriptTag matches an opening <script ...> tag and captures its attributes, so
// a test can ask whether the tag loads a file or carries a body.
var scriptTag = regexp.MustCompile(`(?i)<script([^>]*)>`)

// scriptSrcAttr matches a real src attribute on a tag, anchored to an attribute
// boundary. A substring search for "src=" would also match data-src= or
// integrity-src=, which would let an inline <script> slip past the check that
// exists specifically to catch one.
var scriptSrcAttr = regexp.MustCompile(`(?i)(^|\s)src\s*=`)

// TestServer_ShellHasNoInlineScript is what keeps the CSP honest. script-src
// 'self' forbids inline script execution, so an inline <script> added to the
// shell later would compile, serve, and test green in Go while failing only as
// a violation in a browser console nobody is watching. Asserting the property
// in the served HTML puts the failure back in the test run.
//
// The check is structural rather than a search for known snippets: every
// <script> opening tag must carry a src attribute. A tag without one has a body,
// and a body is inline script regardless of what it contains.
func TestServer_ShellHasNoInlineScript(t *testing.T) {
	idx, label := testIndex(t)
	h := testHandler(idx)

	for _, path := range []string{"/", "/doc/" + label + "/welcome.md"} {
		t.Run(path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
			body := rec.Body.String()

			tags := scriptTag.FindAllStringSubmatch(body, -1)
			if len(tags) == 0 {
				t.Fatalf("no <script> tags found in %s at all — the shell links several, so this assertion is no longer testing what it thinks it is", path)
			}
			for _, tag := range tags {
				if !scriptSrcAttr.MatchString(tag[1]) {
					t.Errorf("inline <script%s> in %s: the handler sends script-src 'self', which blocks inline script in the browser and reports nothing to Go. Move the code into internal/docs/assets/ and serve it, the way assets/sidenav-filter.js is", tag[1], path)
				}
			}
		})
	}
}

// assetRef captures the value of every src= and href= attribute in the shell,
// in all three HTML quoting forms.
//
// Matching only double-quoted values would make this test quietly selective:
// a single-quoted or unquoted reference added later would not match, so it
// would never be examined and the test would still pass. The alternation makes
// "not matched" mean "not a reference" rather than "not a form we happen to
// parse". Group 1 is the raw value with any quotes, and groups 2/3/4 are the
// double-quoted, single-quoted, and bare contents respectively.
var assetRef = regexp.MustCompile(`(?i)\s(?:src|href)\s*=\s*("([^"]*)"|'([^']*)'|([^\s>]+))`)

// assetRefValue pulls the unquoted value out of an assetRef match, choosing the
// branch by how the raw match starts rather than by "first non-empty group" —
// the latter would mis-handle a legitimately empty value like href="".
func assetRefValue(ref []string) string {
	switch {
	case strings.HasPrefix(ref[1], `"`):
		return ref[2]
	case strings.HasPrefix(ref[1], "'"):
		return ref[3]
	default:
		return ref[4]
	}
}

// TestServer_ShellReferencesOnlySameOriginAssets pins the other half of the
// policy: default-src 'self' means an absolute or protocol-relative URL in the
// shell would simply not load. The reader is deliberately network-free — every
// asset is vendored and embedded — so a reference that leaves this origin is
// both a broken page and a third-party request from opening a local document.
func TestServer_ShellReferencesOnlySameOriginAssets(t *testing.T) {
	idx, label := testIndex(t)
	h := testHandler(idx)

	for _, path := range []string{"/", "/doc/" + label + "/welcome.md"} {
		t.Run(path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))

			refs := assetRef.FindAllStringSubmatch(rec.Body.String(), -1)
			if len(refs) == 0 {
				t.Fatalf("no src=/href= references found in %s — the shell links stylesheets and scripts, so this assertion is no longer testing what it thinks it is", path)
			}
			for _, ref := range refs {
				got := assetRefValue(ref)
				switch {
				case got == "":
					// href="" is a same-document reference; it cannot be off-origin.
				case strings.HasPrefix(got, "//"):
					t.Errorf("protocol-relative reference %q in %s: it inherits the page's scheme but not its origin, so it is off-origin and default-src 'self' blocks it", got, path)
				case strings.Contains(got, "://"):
					t.Errorf("absolute reference %q in %s: the reader vendors every asset and makes no network calls, and default-src 'self' blocks this", got, path)
				case !strings.HasPrefix(got, "/") && !strings.HasPrefix(got, "#"):
					t.Errorf("reference %q in %s is neither root-relative nor a fragment; keep shell references unambiguous so this test can tell same-origin from not", got, path)
				}
			}
		})
	}
}
