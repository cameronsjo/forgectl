package docs

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
)

func TestRewriteLocalImageURLs_RewritesOnlyContainedRelativeImages(t *testing.T) {
	rendered := `<svg viewBox="0 0 10 10"><linearGradient gradientUnits="userSpaceOnUse"></linearGradient></svg>` +
		`<p><img src="../images/architecture.svg#focus" alt="local">` +
		`<img src="https://example.com/tracker.png" alt="remote">` +
		`<img src="data:image/png;base64,abc" alt="inline">` +
		`<img src="../../../escape.png" alt="escape"></p>`

	got, err := RewriteLocalImageURLs(rendered, "docs", "guide/setup/readme.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, `<svg viewBox="0 0 10 10"><linearGradient gradientUnits="userSpaceOnUse"></linearGradient></svg>`) {
		t.Errorf("rewrite changed unrelated inline SVG markup: %s", got)
	}
	wantQuery := url.Values{"doc": {"guide/setup/readme.md"}, "path": {"guide/images/architecture.svg"}}.Encode()
	if !strings.Contains(got, `/media/docs?`+strings.ReplaceAll(wantQuery, "&", "&amp;")+`#focus`) {
		t.Errorf("rewritten HTML missing local media URL: %s", got)
	}
	for _, want := range []string{
		`src="https://example.com/tracker.png"`,
		`src="data:image/png;base64,abc"`,
		`src="../../../escape.png"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rewritten HTML changed %q: %s", want, got)
		}
	}
}

func TestServer_RelativeMarkdownImageIsServed(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "guide", "readme.md"), "# Guide\n\n![diagram](../images/architecture.svg#detail)\n")
	writeFile(t, filepath.Join(dir, "images", "architecture.svg"), `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 10 10"><circle cx="5" cy="5" r="4"/></svg>`)
	idx, err := NewIndex([]string{dir})
	if err != nil {
		t.Fatal(err)
	}
	label := idx.Roots()[0].Label
	h := testHandler(idx)

	docRec := httptest.NewRecorder()
	h.ServeHTTP(docRec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/doc/"+label+"/guide/readme.md", nil))
	if docRec.Code != http.StatusOK {
		t.Fatalf("doc status = %d", docRec.Code)
	}
	wantQuery := url.Values{"doc": {"guide/readme.md"}, "path": {"images/architecture.svg"}}.Encode()
	mediaURL := "/media/" + label + "?" + wantQuery
	if !strings.Contains(docRec.Body.String(), strings.ReplaceAll(mediaURL, "&", "&amp;")+"#detail") {
		t.Fatalf("doc body missing rewritten media URL %q: %s", mediaURL, docRec.Body.String())
	}

	mediaRec := httptest.NewRecorder()
	h.ServeHTTP(mediaRec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, mediaURL, nil))
	if mediaRec.Code != http.StatusOK {
		t.Fatalf("media status = %d, body: %s", mediaRec.Code, mediaRec.Body.String())
	}
	if got := mediaRec.Header().Get("Content-Type"); got != "image/svg+xml" {
		t.Errorf("Content-Type = %q, want image/svg+xml", got)
	}
	if got := mediaRec.Header().Get("Content-Security-Policy"); !strings.Contains(got, "sandbox") {
		t.Errorf("SVG CSP = %q, want sandbox", got)
	}
}

func TestServer_MediaRequiresDocumentReference(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "readme.md"), "# Guide\n\nNo image here.\n")
	writeFile(t, filepath.Join(dir, "secret.png"), "not really a png")
	idx, err := NewIndex([]string{dir})
	if err != nil {
		t.Fatal(err)
	}
	label := idx.Roots()[0].Label
	query := url.Values{"doc": {"readme.md"}, "path": {"secret.png"}}.Encode()
	rec := httptest.NewRecorder()
	testHandler(idx).ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/media/"+label+"?"+query, nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestServer_SingleFileRootMayServeItsReferencedSiblingImage(t *testing.T) {
	dir := t.TempDir()
	doc := filepath.Join(dir, "readme.md")
	writeFile(t, doc, "# Guide\n\n![sample](sample.png)\n")
	writeFile(t, filepath.Join(dir, "sample.png"), "png fixture")
	idx, err := NewIndex([]string{doc})
	if err != nil {
		t.Fatal(err)
	}
	label := idx.Roots()[0].Label
	query := url.Values{"doc": {"readme.md"}, "path": {"sample.png"}}.Encode()
	rec := httptest.NewRecorder()
	testHandler(idx).ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/media/"+label+"?"+query, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
}

func TestServer_MediaUnderExcludedDirectoryIsRejected(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "readme.md"), "# Guide\n\n![hidden](.private/image.png)\n")
	writeFile(t, filepath.Join(dir, ".private", "image.png"), "png fixture")
	idx, err := NewIndex([]string{dir})
	if err != nil {
		t.Fatal(err)
	}
	label := idx.Roots()[0].Label
	query := url.Values{"doc": {"readme.md"}, "path": {".private/image.png"}}.Encode()
	rec := httptest.NewRecorder()
	testHandler(idx).ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/media/"+label+"?"+query, nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}
