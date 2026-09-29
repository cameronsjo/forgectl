package docs

// Test plan for the status-bar trust badges (server.go renderShell)
//
//   [x] Unhappy: a passed stale_after badges both the properties block and the
//       status bar; a future one badges neither
//   [x] Unhappy: status: deprecated badges the status bar
//   [x] Sad: a hostile stale_after never reaches the title attribute raw

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

const staleBadgeMarkup = `class="trust-badge trust-badge--stale"`

// trustPage indexes one x.md with frontmatter fm and returns its rendered
// page and the page's status bar.
func trustPage(t *testing.T, fm string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "x.md"), "---\n"+fm+"---\n\n# X\n")
	idx, err := NewIndex([]string{dir})
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	rec := httptest.NewRecorder()
	testHandler(idx).ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet,
		"/doc/"+idx.Roots()[0].Label+"/x.md", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	i := strings.Index(body, `<footer class="statusbar"`)
	if i < 0 {
		t.Fatalf("no status bar in:\n%s", body)
	}
	footer := body[i:]
	footer = footer[:strings.Index(footer, "</footer>")]
	return body, footer
}

func TestServer_StaleBadgeInPropsAndStatusBar(t *testing.T) {
	withTrustNow(t, trustTestNow)

	body, footer := trustPage(t, "stale_after: 2026-09-28T12:00:00Z\n")
	// Count markup, not the class name: the shell's CSS names it too.
	if n := strings.Count(body, staleBadgeMarkup); n != 2 {
		t.Errorf("stale badges = %d, want 2 (props + status bar)", n)
	}
	if n := strings.Count(footer, staleBadgeMarkup); n != 1 {
		t.Errorf("status bar stale badges = %d, want 1:\n%s", n, footer)
	}

	body, _ = trustPage(t, "stale_after: 2026-09-30T12:00:00Z\n")
	if strings.Contains(body, staleBadgeMarkup) {
		t.Error("future stale_after was badged")
	}
}

func TestServer_DeprecatedBadgeInStatusBar(t *testing.T) {
	withTrustNow(t, trustTestNow)

	_, footer := trustPage(t, "status: deprecated\n")
	if !strings.Contains(footer, "trust-badge--deprecated") {
		t.Errorf("status bar has no deprecated badge:\n%s", footer)
	}
}

func TestServer_StaleTitleEscaped(t *testing.T) {
	// No RFC 3339 value can carry markup, so evalTrust never hands the
	// template one. Execute the shell directly to pin that the title
	// attribute is autoescaped anyway, should that upstream filter change.
	var b strings.Builder
	data := shellData{DocPath: "r/x.md", Trust: trustState{Stale: true, StaleAfter: `x"><b>`}}
	if err := shellTemplate.Execute(&b, data); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(b.String(), `x"><b>`) {
		t.Errorf("raw markup reached the status bar title")
	}
	if !strings.Contains(b.String(), `title="stale since x&#34;&gt;&lt;b&gt;"`) {
		t.Errorf("escaped title missing from the status bar")
	}
}
