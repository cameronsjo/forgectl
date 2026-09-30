package docs

// Test plan for search_qmd.go
//
// Searcher.Search with Backend = SearchBackendQMD (Classification: ops layer
// — subprocess + security gate over untrusted output)
//   [x] Security: the argv is `search --json --full-path -n <window> -- <q>`,
//       run by the absolute qmd path; never `query`
//   [x] Security: out-of-root, stale (in-root but not indexed), excluded-dir,
//       "../" escapes (absolute and "./"-relative), qmd:// URIs, and bare
//       relative paths are dropped and counted in Skipped; an absolute and a
//       "./"-relative indexed doc are kept, with the Index's title
//   [x] Security: stdout with a banner line before the JSON, a second JSON
//       value after it, a bare null, or an object fails closed with
//       ErrSearchBackendFailed and no results
//   [x] Unhappy: a non-zero exit fails with qmd's stderr, never the query
//   [x] Unhappy: qmd missing, or found only in a relative PATH entry, wraps
//       ErrNoSearchBackend and runs nothing
//   [x] Happy: a hit past the limit sets Truncated; so does a full -n window
//       even when fewer than limit hits survive the gate
//   [x] Happy: qmdWindow is limit*10, capped at 1000, never below limit+1,
//       and never past maxQMDRows (no overflow at math.MaxInt)
//   [x] Security: the snippet is re-read from the doc at the lines the
//       "@@ -start,count @@" header names (or the hit's line), never qmd's
//       own snippet text (forgectl#743)
//   [x] Happy: overlapping roots return a shared doc once, first root wins
//   [x] Unhappy: an unknown backend is refused before anything runs
//   [x] Unhappy: output past maxQMDOutputBytes fails closed

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	osexec "os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	forgexec "github.com/cameronsjo/forgectl/internal/exec"
)

// fakeQMD is a StreamingRunner standing in for qmd: it records the binary
// and argv, writes stdout and stderr, and returns err.
type fakeQMD struct {
	name   string
	args   []string
	calls  int
	stdout string
	stderr string
	err    error
}

func (f *fakeQMD) RunStreaming(_ context.Context, _ io.Reader, stdout, stderr io.Writer, name string, args ...string) error {
	f.calls++
	f.name = name
	f.args = slices.Clone(args)
	_, _ = io.WriteString(stdout, f.stdout)
	_, _ = io.WriteString(stderr, f.stderr)
	return f.err
}

func qmdLookPath(string) (string, error) { return "/usr/local/bin/qmd", nil }

// qmdJSON renders rows the way `qmd search --json` does: one indented array.
func qmdJSON(t *testing.T, rows ...map[string]any) string {
	t.Helper()
	if rows == nil {
		rows = []map[string]any{}
	}
	b, err := json.MarshalIndent(rows, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return string(b) + "\n"
}

func qmdRow(file string, line int) map[string]any {
	return map[string]any{
		"score":   1.5,
		"file":    file,
		"line":    line,
		"title":   "qmd's own title, never used",
		"snippet": "@@ -1,2 @@ (0 before, 1 after)\nneedle here\nmore",
	}
}

func qmdSearcher(r *fakeQMD, cwd string) Searcher {
	return Searcher{
		Runner:   r,
		LookPath: qmdLookPath,
		Backend:  SearchBackendQMD,
		Getwd:    func() (string, error) { return cwd, nil },
	}
}

func TestQMDSearchArgv(t *testing.T) {
	idx, _ := searchRoot(t, "a.md")
	r := &fakeQMD{stdout: "[]\n"}
	if _, err := qmdSearcher(r, t.TempDir()).Search(context.Background(), idx, "--version", 7); err != nil {
		t.Fatal(err)
	}
	if r.name != "/usr/local/bin/qmd" {
		t.Errorf("ran %q, want the absolute qmd path", r.name)
	}
	want := []string{"search", "--json", "--full-path", "-n", "70", "--", "--version"}
	if !slices.Equal(r.args, want) {
		t.Errorf("argv = %q, want %q", r.args, want)
	}
}

func TestQMDSearchGatesEveryHit(t *testing.T) {
	idx, root := searchRoot(t, "a.md", "sub/b.md", "node_modules/x.md")
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "o.md"), "# o\nneedle\n")
	// A file created after the index was built: in the root, on disk, but
	// not indexed, like a qmd index that knows a doc forgectl does not.
	writeFile(t, filepath.Join(root, "stale.md"), "# stale\nneedle\n")
	escapeName := filepath.Base(outside)
	r := &fakeQMD{stdout: qmdJSON(t,
		qmdRow(filepath.Join(root, "a.md"), 2),
		qmdRow(filepath.Join(outside, "o.md"), 2),
		qmdRow(filepath.Join(root, "stale.md"), 2),
		qmdRow(filepath.Join(root, "node_modules", "x.md"), 2),
		qmdRow(root+"/../"+escapeName+"/o.md", 2),
		qmdRow("./../"+escapeName+"/o.md", 2),
		qmdRow("qmd://notes/a.md", 2),
		qmdRow("sub/b.md", 2),
		qmdRow("./sub/b.md", 3),
	)}

	resp, err := qmdSearcher(r, root).Search(context.Background(), idx, "needle", 50)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, h := range resp.Results {
		got = append(got, h.Path)
	}
	if !slices.Equal(got, []string{"a.md", "sub/b.md"}) {
		t.Fatalf("results = %q, want [a.md sub/b.md]", got)
	}
	if resp.Skipped != 7 {
		t.Errorf("Skipped = %d, want 7", resp.Skipped)
	}
	if resp.Backend != SearchBackendQMD {
		t.Errorf("Backend = %q, want %q", resp.Backend, SearchBackendQMD)
	}
	if resp.Results[0].Title != "a.md" {
		t.Errorf("Title = %q, want the Index's title", resp.Results[0].Title)
	}
	if resp.Results[1].Line != 3 {
		t.Errorf("Line = %d, want 3", resp.Results[1].Line)
	}
	if resp.Truncated {
		t.Error("Truncated set with room to spare")
	}
}

func TestQMDSearchFailsClosedOnImpureOutput(t *testing.T) {
	idx, root := searchRoot(t, "a.md")
	good := qmdJSON(t, qmdRow(filepath.Join(root, "a.md"), 2))
	cases := map[string]string{
		"banner before JSON": "Config that came with a checkout is not trusted by default.\n" + good,
		"second value after": good + good,
		"bare null":          "null\n",
		"object":             `{"file":"` + filepath.Join(root, "a.md") + `"}`,
		"empty":              "",
	}
	for name, out := range cases {
		t.Run(name, func(t *testing.T) {
			resp, err := qmdSearcher(&fakeQMD{stdout: out}, root).Search(context.Background(), idx, "needle", 50)
			if !errors.Is(err, ErrSearchBackendFailed) {
				t.Fatalf("err = %v, want ErrSearchBackendFailed", err)
			}
			if len(resp.Results) != 0 {
				t.Errorf("results = %v, want none", resp.Results)
			}
		})
	}
}

func TestQMDSearchNonZeroExit(t *testing.T) {
	idx, _ := searchRoot(t, "a.md")
	args := []string{"search", "--json", "--", "secretquery"}
	r := &fakeQMD{stderr: "no index found\n", err: &forgexec.CommandError{Name: "qmd", Args: args, ExitCode: 1}}
	_, err := qmdSearcher(r, t.TempDir()).Search(context.Background(), idx, "secretquery", 5)
	if !errors.Is(err, ErrSearchBackendFailed) {
		t.Fatalf("err = %v, want ErrSearchBackendFailed", err)
	}
	if !strings.Contains(err.Error(), "no index found") || strings.Contains(err.Error(), "secretquery") {
		t.Errorf("err = %q, want qmd's stderr and not the query", err)
	}

	r = &fakeQMD{err: &forgexec.CommandError{Name: "qmd", Args: args, ExitCode: 3}}
	_, err = qmdSearcher(r, t.TempDir()).Search(context.Background(), idx, "secretquery", 5)
	if err == nil || !strings.Contains(err.Error(), "exit 3") || strings.Contains(err.Error(), "secretquery") {
		t.Errorf("silent failure err = %v, want it to name exit 3", err)
	}

	// #926: Err's text is redacted as CommandError.Error() redacts it.
	// Mutation: drop redact.Text around cmdErr.Err.Error() in qmdFailure.
	r = &fakeQMD{err: &forgexec.CommandError{Name: "qmd", Args: args, ExitCode: -1, Err: errors.New(`exec: "https://u:qmdtok@example.invalid/qmd": permission denied`)}}
	_, err = qmdSearcher(r, t.TempDir()).Search(context.Background(), idx, "secretquery", 5)
	if err == nil || strings.Contains(err.Error(), "qmdtok") || !strings.Contains(err.Error(), "[redacted]") {
		t.Errorf("never-ran err = %v, want Err's text withheld", err)
	}
}

func TestQMDSearchMissingBinary(t *testing.T) {
	idx, _ := searchRoot(t, "a.md")
	for name, look := range map[string]func(string) (string, error){
		"absent":   func(string) (string, error) { return "", osexec.ErrNotFound },
		"relative": func(string) (string, error) { return "./qmd", osexec.ErrDot },
	} {
		t.Run(name, func(t *testing.T) {
			r := &fakeQMD{}
			s := qmdSearcher(r, t.TempDir())
			s.LookPath = look
			_, err := s.Search(context.Background(), idx, "q", 5)
			if !errors.Is(err, ErrNoSearchBackend) || !strings.Contains(err.Error(), "qmd") {
				t.Errorf("err = %v, want ErrNoSearchBackend naming qmd", err)
			}
			if r.calls != 0 {
				t.Errorf("qmd ran %d times", r.calls)
			}
		})
	}
}

func TestQMDSearchTruncation(t *testing.T) {
	idx, root := searchRoot(t, "a.md", "b.md")
	a, b := filepath.Join(root, "a.md"), filepath.Join(root, "b.md")

	resp, err := qmdSearcher(&fakeQMD{stdout: qmdJSON(t, qmdRow(a, 2), qmdRow(b, 2))}, root).Search(context.Background(), idx, "q", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Results) != 1 || !resp.Truncated {
		t.Errorf("limit 1 over 2 hits: %d results, Truncated %v; want 1, true", len(resp.Results), resp.Truncated)
	}

	// limit 1 asks for a window of 10. Ten rows, nine of them outside every
	// root: one result, but qmd may have ranked more in-root hits below.
	rows := []map[string]any{qmdRow(a, 2)}
	for range 9 {
		rows = append(rows, qmdRow("/nowhere/x.md", 1))
	}
	resp, err = qmdSearcher(&fakeQMD{stdout: qmdJSON(t, rows...)}, root).Search(context.Background(), idx, "q", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Results) != 1 || !resp.Truncated {
		t.Errorf("full window: %d results, Truncated %v; want 1, true", len(resp.Results), resp.Truncated)
	}
}

func TestQMDWindow(t *testing.T) {
	for limit, want := range map[int]int{1: 10, 5: 50, 99: 990, 100: 1000, 999: 1000, 1000: 1001, 5000: 5001, maxQMDRows: maxQMDRows, math.MaxInt: maxQMDRows} {
		if got := qmdWindow(limit); got != want {
			t.Errorf("qmdWindow(%d) = %d, want %d", limit, got, want)
		}
	}
}

// forgectl#743: the snippet is the doc's own lines, re-read through the
// Index at the lines qmd's "@@ -start,count @@" header names, never qmd's
// snippet text, which comes from qmd's index and can be stale or another
// file's.
func TestQMDSnippetIsReReadFromTheDoc(t *testing.T) {
	idx, root := searchRoot(t, "a.md")
	writeFile(t, filepath.Join(root, "a.md"), "# a\n  needle here\nmore\nnot this\n")
	idx, err := idx.Rebuild()
	if err != nil {
		t.Fatal(err)
	}
	row := qmdRow(filepath.Join(root, "a.md"), 2)
	row["snippet"] = "@@ -2,2 @@ (1 before, 1 after)\nOUTSIDE TEXT\nFROM QMD"
	resp, err := qmdSearcher(&fakeQMD{stdout: qmdJSON(t, row)}, root).Search(context.Background(), idx, "q", 5)
	if err != nil {
		t.Fatal(err)
	}
	if got := resp.Results[0].Snippet; got != "needle here more" {
		t.Errorf("Snippet = %q, want lines 2-3 of the doc, trimmed and folded", got)
	}

	// Without a header, the hit's own line is read alone.
	row["snippet"] = "OUTSIDE TEXT"
	row["line"] = 4
	resp, err = qmdSearcher(&fakeQMD{stdout: qmdJSON(t, row)}, root).Search(context.Background(), idx, "q", 5)
	if err != nil {
		t.Fatal(err)
	}
	if got := resp.Results[0].Snippet; got != "not this" {
		t.Errorf("headerless Snippet = %q, want line 4 of the doc", got)
	}
}

func TestQMDSnippetLines(t *testing.T) {
	for in, want := range map[string][3]int{
		"@@ -3,1 @@ (2 before, 0 after)\n  needle here \n": {3, 1, 1},
		"@@ -12,4 @@ (11 before, 9 after)":                 {12, 4, 1},
		"  needle here\t":                                  {0, 0, 0},
		"@@ -0,1 @@ (0 before, 0 after)\nx":                {0, 0, 0},
		"@@ -2 @@\nx":                                      {0, 0, 0},
		"@@ -a,b @@\nx":                                    {0, 0, 0},
	} {
		first, count, ok := qmdSnippetLines(in)
		got := [3]int{first, count, 0}
		if ok {
			got[2] = 1
		}
		if got != want {
			t.Errorf("qmdSnippetLines(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestQMDSearchDedupesOverlappingRoots(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "docs", "a.md"), "# a\nneedle\n")
	idx, err := NewIndex([]string{dir, filepath.Join(dir, "docs")})
	if err != nil {
		t.Fatal(err)
	}
	roots := idx.Roots()
	abs := filepath.Join(roots[0].Path, "docs", "a.md")
	resp, err := qmdSearcher(&fakeQMD{stdout: qmdJSON(t, qmdRow(abs, 2), qmdRow(abs, 2))}, dir).Search(context.Background(), idx, "q", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Results) != 1 || resp.Results[0].Root != roots[0].Label {
		t.Errorf("results = %+v, want one under the first root %q", resp.Results, roots[0].Label)
	}
}

func TestSearchUnknownBackend(t *testing.T) {
	idx, _ := searchRoot(t, "a.md")
	r := &fakeQMD{}
	_, err := Searcher{Runner: r, LookPath: qmdLookPath, Backend: "elastic"}.Search(context.Background(), idx, "q", 5)
	if err == nil || r.calls != 0 {
		t.Errorf("err = %v, calls = %d; want an error and nothing run", err, r.calls)
	}
}

func TestQMDSearchOversizedOutput(t *testing.T) {
	idx, root := searchRoot(t, "a.md")
	big := qmdJSON(t, qmdRow(filepath.Join(root, "a.md"), 2)) + strings.Repeat(" ", maxQMDOutputBytes)
	_, err := qmdSearcher(&fakeQMD{stdout: big}, root).Search(context.Background(), idx, "q", 5)
	if !errors.Is(err, ErrSearchBackendFailed) {
		t.Errorf("err = %v, want ErrSearchBackendFailed", err)
	}
}

// Compile-time proof the fake satisfies the seam.
var _ forgexec.StreamingRunner = (*fakeQMD)(nil)
