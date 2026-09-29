package cli

// Test plan for docs_search.go
//
// newDocsSearchCmd (Classification: cobra command)
//   [x] Happy: --json emits the SearchResponse keys, results always an array
//   [x] Unhappy: rg missing under --json leaves stdout empty, writes exactly
//       one JSON error object to stderr, and exits 2
//   [x] Security: a snippet carrying an ESC sequence reaches stdout escaped
//   [x] Unhappy: an empty query is rejected before rg runs

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"testing"

	forgexec "github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/module"
)

// searchRunner is a Runner that also streams: RunStreaming writes stdout
// verbatim to the command's stdout sink and counts its calls.
type searchRunner struct {
	*forgexec.FakeRunner
	stdout string
	calls  int
}

func (r *searchRunner) RunStreaming(_ context.Context, _ io.Reader, stdout, _ io.Writer, _ string, _ ...string) error {
	r.calls++
	_, _ = io.WriteString(stdout, r.stdout)
	return nil
}

// docsSearchFixture makes a temp cwd holding page.md, with the default-root
// extras cleared, and returns page.md's canonical path.
func docsSearchFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "page.md"), []byte("# Page\nneedle here\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	t.Setenv(cadenceFieldReportsEnv, "")
	canonical, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(canonical, "page.md")
}

func rgMatchLine(t *testing.T, path, line string) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"type": "match",
		"data": map[string]any{
			"path":        map[string]any{"text": path},
			"lines":       map[string]any{"text": line},
			"line_number": 2,
			"submatches":  []map[string]any{{"start": 0, "end": 1}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(b) + "\n"
}

func stubSearchLookPath(t *testing.T, fn func(string) (string, error)) {
	t.Helper()
	prev := docsSearchLookPath
	docsSearchLookPath = fn
	t.Cleanup(func() { docsSearchLookPath = prev })
}

func runDocsSearch(t *testing.T, runner *searchRunner, args ...string) (string, string, error) {
	t.Helper()
	cmd := newDocsSearchCmd(module.Deps{Runner: runner})
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(context.Background())
	return stdout.String(), stderr.String(), err
}

func TestDocsSearchJSONShape(t *testing.T) {
	page := docsSearchFixture(t)
	stubSearchLookPath(t, func(string) (string, error) { return "/usr/bin/rg", nil })
	runner := &searchRunner{FakeRunner: &forgexec.FakeRunner{}, stdout: rgMatchLine(t, page, "needle here\n")}

	stdout, _, err := runDocsSearch(t, runner, "--json", "needle")
	if err != nil {
		t.Fatalf("docs search: %v", err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("stdout is not a JSON object: %v\n%s", err, stdout)
	}
	for _, k := range []string{"backend", "query", "results", "truncated", "skipped"} {
		if _, ok := got[k]; !ok {
			t.Errorf("response lacks %q: %s", k, stdout)
		}
	}
	var results []map[string]json.RawMessage
	if err := json.Unmarshal(got["results"], &results); err != nil || len(results) != 1 {
		t.Fatalf("results = %s (err %v), want one entry", got["results"], err)
	}
	for _, k := range []string{"root", "path", "title", "line", "snippet"} {
		if _, ok := results[0][k]; !ok {
			t.Errorf("result lacks %q: %s", k, got["results"])
		}
	}
}

func TestDocsSearchNoBackendJSONError(t *testing.T) {
	docsSearchFixture(t)
	stubSearchLookPath(t, func(string) (string, error) { return "", osexec.ErrNotFound })
	runner := &searchRunner{FakeRunner: &forgexec.FakeRunner{}}

	stdout, stderr, err := runDocsSearch(t, runner, "--json", "needle")
	if code := ExitCode(err); code != 2 {
		t.Errorf("exit code = %d (err %v), want 2", code, err)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty", stdout)
	}
	dec := json.NewDecoder(strings.NewReader(stderr))
	var obj struct {
		Error string `json:"error"`
		Code  int    `json:"code"`
	}
	if err := dec.Decode(&obj); err != nil {
		t.Fatalf("stderr is not a JSON object: %v\n%s", err, stderr)
	}
	if obj.Code != 2 || !strings.Contains(obj.Error, "install ripgrep") {
		t.Errorf("error object = %+v, want code 2 naming ripgrep", obj)
	}
	if dec.More() {
		t.Errorf("stderr holds more than one JSON value: %s", stderr)
	}
	if runner.calls != 0 {
		t.Errorf("rg ran %d times without a resolved binary", runner.calls)
	}
}

func TestDocsSearchHumanOutputEscapesControlBytes(t *testing.T) {
	page := docsSearchFixture(t)
	stubSearchLookPath(t, func(string) (string, error) { return "/usr/bin/rg", nil })
	runner := &searchRunner{FakeRunner: &forgexec.FakeRunner{}, stdout: rgMatchLine(t, page, "needle \u001b[31mred\n")}

	stdout, _, err := runDocsSearch(t, runner, "needle")
	if err != nil {
		t.Fatalf("docs search: %v", err)
	}
	if strings.ContainsRune(stdout, '\u001b') {
		t.Errorf("a raw ESC reached stdout: %q", stdout)
	}
	if !strings.Contains(stdout, "page.md:2") {
		t.Errorf("stdout = %q, want a page.md:2 line", stdout)
	}
}

func TestDocsSearchRejectsEmptyQuery(t *testing.T) {
	docsSearchFixture(t)
	stubSearchLookPath(t, func(string) (string, error) { return "/usr/bin/rg", nil })
	runner := &searchRunner{FakeRunner: &forgexec.FakeRunner{}}

	_, _, err := runDocsSearch(t, runner, "  ")
	if err == nil {
		t.Fatal("docs search accepted a whitespace-only query")
	}
	if runner.calls != 0 {
		t.Errorf("rg ran %d times for an empty query", runner.calls)
	}
}
