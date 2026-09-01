package docs

// Sidenav tree contract:
//   [x] Happy: a root's docs render as a nested .tree--static (details per directory)
//   [x] Happy: the current doc's leaf carries aria-current and its ancestor dirs are open
//   [x] Happy: a directory summary carries its leaf count
//   [x] Sad: a non-current directory renders closed
//   [x] Happy: the Recent group stays a flat link list

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testTreeIndex(t *testing.T) (*Index, string) {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "README.md"), "# Top\n")
	if err := os.MkdirAll(filepath.Join(dir, "commands"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "adr"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "commands", "env.md"), "# env\n")
	writeFile(t, filepath.Join(dir, "commands", "launch.md"), "# launch\n")
	writeFile(t, filepath.Join(dir, "adr", "0001-thing.md"), "# 0001\n")
	idx, err := NewIndex([]string{dir})
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	return idx, idx.Roots()[0].Label
}

func getBody(t *testing.T, h http.Handler, path string) string {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200", path, rec.Code)
	}
	return rec.Body.String()
}

func TestShell_SidenavIsStaticTree(t *testing.T) {
	idx, label := testTreeIndex(t)
	body := getBody(t, testHandler(idx), "/doc/"+label+"/commands/env.md")

	for _, want := range []string{
		`<ul class="tree tree--static"`,
		`<summary class="tree__row">`,
		`class="tree__twisty"`,
		`tree__leaf`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("sidenav missing %q in body:\n%s", want, body)
		}
	}
}

func TestShell_CurrentLeafMarkedAndAncestorsOpen(t *testing.T) {
	idx, label := testTreeIndex(t)
	body := getBody(t, testHandler(idx), "/doc/"+label+"/commands/env.md")

	if !strings.Contains(body, `aria-current="page"`) {
		t.Errorf("current leaf not marked aria-current:\n%s", body)
	}
	// The directory holding the current doc must render expanded, and one
	// that does not must render collapsed.
	commandsAt := strings.Index(body, ">commands<")
	if commandsAt < 0 {
		t.Fatalf("commands directory row missing:\n%s", body)
	}
	openDetails := `<details open>`
	segment := body[:commandsAt]
	if !strings.Contains(segment[strings.LastIndex(segment, "<details"):], openDetails) {
		t.Errorf("directory of the current doc is not open:\n%s", body)
	}
	adrAt := strings.Index(body, ">adr<")
	if adrAt < 0 {
		t.Fatalf("adr directory row missing:\n%s", body)
	}
	adrSegment := body[:adrAt]
	if strings.Contains(adrSegment[strings.LastIndex(adrSegment, "<details"):], openDetails) {
		t.Errorf("non-current directory rendered open:\n%s", body)
	}
}

func TestShell_DirectorySummaryCarriesCount(t *testing.T) {
	idx, label := testTreeIndex(t)
	body := getBody(t, testHandler(idx), "/doc/"+label+"/README.md")

	if !strings.Contains(body, `<span class="count">2</span>`) {
		t.Errorf("commands directory count missing (want 2 leaves):\n%s", body)
	}
}

func TestShell_RecentGroupStaysFlat(t *testing.T) {
	idx, label := testTreeIndex(t)
	body := getBody(t, testHandler(idx), "/doc/"+label+"/README.md")

	recentAt := strings.Index(body, `<div class="sidenav__group">Recent</div>`)
	if recentAt < 0 {
		t.Fatalf("Recent group missing:\n%s", body)
	}
	// Between the Recent heading and the next group heading there must be
	// plain links, not a tree.
	rest := body[recentAt+1:]
	nextGroup := strings.Index(rest, `<div class="sidenav__group">`)
	if nextGroup < 0 {
		t.Fatalf("no second group after Recent:\n%s", body)
	}
	recentBlock := rest[:nextGroup]
	if strings.Contains(recentBlock, "tree--static") {
		t.Errorf("Recent group rendered as a tree:\n%s", recentBlock)
	}
	if !strings.Contains(recentBlock, `data-filter-text=`) {
		t.Errorf("Recent group lost its filterable links:\n%s", recentBlock)
	}
}
