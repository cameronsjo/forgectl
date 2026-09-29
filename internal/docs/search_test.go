package docs

// Test plan for search.go
//
// Searcher.Search (Classification: ops layer — subprocess + security gate)
//   [x] Security: the rg argv carries --no-config, and "--" sits immediately
//       before the query, which sits immediately before the path
//   [x] Security: hits outside the root, under node_modules, or under .git
//       are dropped and counted in Skipped
//   [x] Unhappy: a path rg reports only as base64 bytes is skipped, not
//       decoded into a result
//   [x] Happy: the limit stops the run: limit results, Truncated set, the
//       child context cancelled
//   [x] Unhappy: a record over maxRgRecordBytes is dropped and counted; the
//       next record still parses
//   [x] Happy: a snippet from a long line is capped and valid UTF-8
//   [x] Happy: rg exit 1 is an empty success with a non-nil Results
//   [x] Unhappy: rg exit 2 with no hits is an error
//   [x] Unhappy: rg missing on PATH wraps ErrNoSearchBackend
//   [x] Security: real rg under a RIPGREP_CONFIG_PATH holding --follow
//       returns no symlinked file, and a --version query is searched as text
//
// ValidateQuery (Classification: input validation)
//   [x] Unhappy: empty, whitespace, oversized, NUL, and invalid UTF-8 queries

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	osexec "os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	forgexec "github.com/cameronsjo/forgectl/internal/exec"
)

// requireRgEnv, when set to any non-empty value, turns the real-rg test's
// skip into a failure. CI installs ripgrep and sets it.
const requireRgEnv = "FORGECTL_REQUIRE_RG"

// fakeRg is a func-typed StreamingRunner: it records the argv and the
// context it was handed, then runs write against the stdout and stderr sinks.
type fakeRg struct {
	calls [][]string
	ctxs  []context.Context
	write func(ctx context.Context, stdout, stderr io.Writer) error
}

func (f *fakeRg) RunStreaming(ctx context.Context, _ io.Reader, stdout, stderr io.Writer, _ string, args ...string) error {
	f.calls = append(f.calls, slices.Clone(args))
	f.ctxs = append(f.ctxs, ctx)
	if f.write == nil {
		return nil
	}
	return f.write(ctx, stdout, stderr)
}

func fakeLookPath(string) (string, error) { return "/usr/bin/rg", nil }

// searchRoot builds a one-root index holding the named markdown files.
func searchRoot(t *testing.T, files ...string) (*Index, string) {
	t.Helper()
	dir := t.TempDir()
	for _, f := range files {
		p := filepath.Join(dir, filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("# "+f+"\nbody\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	idx, err := NewIndex([]string{dir})
	if err != nil {
		t.Fatal(err)
	}
	return idx, idx.Roots()[0].Path
}

// matchRecord renders one rg --json match record for path.
func matchRecord(t *testing.T, path, line string, start int) string {
	t.Helper()
	rec := map[string]any{
		"type": "match",
		"data": map[string]any{
			"path":        map[string]any{"text": path},
			"lines":       map[string]any{"text": line},
			"line_number": 2,
			"submatches":  []map[string]any{{"match": map[string]any{"text": "x"}, "start": start, "end": start + 1}},
		},
	}
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	return string(b) + "\n"
}

func writeAll(records ...string) func(context.Context, io.Writer, io.Writer) error {
	return func(_ context.Context, stdout, _ io.Writer) error {
		for _, r := range records {
			_, _ = io.WriteString(stdout, r)
		}
		return nil
	}
}

func TestSearchArgvEndsOptionsBeforeQuery(t *testing.T) {
	idx, root := searchRoot(t, "a.md")
	rg := &fakeRg{}
	if _, err := (Searcher{Runner: rg, LookPath: fakeLookPath}).Search(t.Context(), idx, "--version", 10); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(rg.calls) != 1 {
		t.Fatalf("rg ran %d times, want 1", len(rg.calls))
	}
	argv := rg.calls[0]
	if !slices.Contains(argv, "--no-config") {
		t.Errorf("argv lacks --no-config (RIPGREP_CONFIG_PATH could re-enable --follow): %q", argv)
	}
	n := len(argv)
	if n < 3 || argv[n-3] != "--" || argv[n-2] != "--version" || argv[n-1] != root {
		t.Errorf("argv must end [-- <query> <root>], got %q", argv)
	}
	if i := slices.Index(argv, "--"); i != n-3 {
		t.Errorf("first -- at %d, want %d: %q", i, n-3, argv)
	}
}

func TestSearchDropsHitsOutsideIndex(t *testing.T) {
	idx, root := searchRoot(t, "a.md", "node_modules/n.md", ".git/x.md")
	rg := &fakeRg{write: writeAll(
		matchRecord(t, filepath.Join(root, "..", "outside", "o.md"), "x", 0),
		matchRecord(t, filepath.Join(root, "node_modules", "n.md"), "x", 0),
		matchRecord(t, filepath.Join(root, ".git", "x.md"), "x", 0),
		matchRecord(t, filepath.Join(root, "a.md"), "x", 0),
	)}
	resp, err := (Searcher{Runner: rg, LookPath: fakeLookPath}).Search(t.Context(), idx, "x", 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(resp.Results) != 1 || resp.Results[0].Path != "a.md" {
		t.Fatalf("results = %+v, want only a.md", resp.Results)
	}
	if resp.Results[0].Title != "a.md" || resp.Results[0].Line != 2 {
		t.Errorf("result = %+v, want title a.md at line 2", resp.Results[0])
	}
	if resp.Skipped != 3 {
		t.Errorf("Skipped = %d, want 3", resp.Skipped)
	}
}

func TestSearchSkipsNonUTF8PathRecord(t *testing.T) {
	idx, root := searchRoot(t, "a.md")
	raw := base64.StdEncoding.EncodeToString([]byte(filepath.Join(root, "a.md")))
	rec := fmt.Sprintf(`{"type":"match","data":{"path":{"bytes":%q},"lines":{"text":"x\n"},"line_number":1,"submatches":[]}}`+"\n", raw)
	rg := &fakeRg{write: writeAll(rec)}
	resp, err := (Searcher{Runner: rg, LookPath: fakeLookPath}).Search(t.Context(), idx, "x", 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(resp.Results) != 0 || resp.Skipped != 1 {
		t.Errorf("results = %+v skipped = %d, want none returned and 1 skipped", resp.Results, resp.Skipped)
	}
}

func TestSearchStopsAtLimit(t *testing.T) {
	names := make([]string, 10)
	for i := range names {
		names[i] = fmt.Sprintf("d%d.md", i)
	}
	idx, root := searchRoot(t, names...)
	records := make([]string, len(names))
	for i, n := range names {
		records[i] = matchRecord(t, filepath.Join(root, n), "x", 0)
	}
	// Checked inside the run: Search's own deferred cancel would make the
	// context read cancelled afterwards whether or not the limit stopped rg.
	var duringRun error
	rg := &fakeRg{write: func(ctx context.Context, stdout, stderr io.Writer) error {
		_ = writeAll(records...)(ctx, stdout, stderr)
		duringRun = ctx.Err()
		return nil
	}}
	resp, err := (Searcher{Runner: rg, LookPath: fakeLookPath}).Search(t.Context(), idx, "x", 3)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(resp.Results) != 3 || !resp.Truncated {
		t.Fatalf("got %d results truncated=%v, want 3 and truncated", len(resp.Results), resp.Truncated)
	}
	if !errors.Is(duringRun, context.Canceled) {
		t.Errorf("rg's context was not cancelled at the limit: %v", duringRun)
	}
}

func TestSearchDropsOversizedRecord(t *testing.T) {
	idx, root := searchRoot(t, "a.md", "b.md")
	big := matchRecord(t, filepath.Join(root, "a.md"), strings.Repeat("x", maxRgRecordBytes+1), 0)
	rg := &fakeRg{write: writeAll(big, matchRecord(t, filepath.Join(root, "b.md"), "x", 0))}
	resp, err := (Searcher{Runner: rg, LookPath: fakeLookPath}).Search(t.Context(), idx, "x", 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(resp.Results) != 1 || resp.Results[0].Path != "b.md" || resp.Skipped != 1 {
		t.Errorf("results = %+v skipped = %d, want only b.md and 1 skipped", resp.Results, resp.Skipped)
	}
}

func TestSearchSnippetBoundedAndRuneSafe(t *testing.T) {
	idx, root := searchRoot(t, "a.md")
	// A three-byte rune repeated: every cut lands next to a multibyte rune,
	// and the match offset (5001) points into the middle of one.
	line := strings.Repeat("€", 3400) + "\n"
	rg := &fakeRg{write: writeAll(matchRecord(t, filepath.Join(root, "a.md"), line, 5001))}
	resp, err := (Searcher{Runner: rg, LookPath: fakeLookPath}).Search(t.Context(), idx, "x", 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(resp.Results) != 1 {
		t.Fatalf("results = %+v, want 1", resp.Results)
	}
	s := resp.Results[0].Snippet
	if n := utf8.RuneCountInString(s); n > maxSnippetRunes || n == 0 {
		t.Errorf("snippet is %d runes, want 1..%d", n, maxSnippetRunes)
	}
	if !utf8.ValidString(s) {
		t.Errorf("snippet is not valid UTF-8: %q", s)
	}
}

func TestSnippetAroundCentresOnMatch(t *testing.T) {
	line := strings.Repeat("a", 1000) + "NEEDLE" + strings.Repeat("b", 1000)
	s := snippetAround(line, 1000, maxSnippetRunes)
	if !strings.Contains(s, "NEEDLE") {
		t.Errorf("snippet does not contain the match: %q", s)
	}
}

func TestSearchExitOneIsEmptySuccess(t *testing.T) {
	idx, _ := searchRoot(t, "a.md")
	rg := &fakeRg{write: func(context.Context, io.Writer, io.Writer) error {
		return &forgexec.CommandError{Name: "rg", ExitCode: 1}
	}}
	resp, err := (Searcher{Runner: rg, LookPath: fakeLookPath}).Search(t.Context(), idx, "x", 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if resp.Results == nil || len(resp.Results) != 0 {
		t.Errorf("Results = %#v, want a non-nil empty slice", resp.Results)
	}
}

func TestSearchExitTwoNoResultsErrors(t *testing.T) {
	idx, _ := searchRoot(t, "a.md")
	rg := &fakeRg{write: func(_ context.Context, _, stderr io.Writer) error {
		_, _ = io.WriteString(stderr, "rg: permission denied\u001b[31m\n")
		return &forgexec.CommandError{Name: "rg", ExitCode: 2}
	}}
	_, err := (Searcher{Runner: rg, LookPath: fakeLookPath}).Search(t.Context(), idx, "x", 10)
	if err == nil {
		t.Fatal("Search succeeded on rg exit 2 with no hits")
	}
	if strings.ContainsRune(err.Error(), '\u001b') {
		t.Errorf("error carries a raw ESC from rg's stderr: %q", err)
	}
}

func TestSearchNoRgIsErrNoSearchBackend(t *testing.T) {
	idx, _ := searchRoot(t, "a.md")
	rg := &fakeRg{}
	notFound := func(string) (string, error) { return "", osexec.ErrNotFound }
	_, err := (Searcher{Runner: rg, LookPath: notFound}).Search(t.Context(), idx, "x", 10)
	if !errors.Is(err, ErrNoSearchBackend) {
		t.Fatalf("err = %v, want ErrNoSearchBackend", err)
	}
	if len(rg.calls) != 0 {
		t.Errorf("rg ran %d times without a resolved binary", len(rg.calls))
	}
}

func TestValidateQuery(t *testing.T) {
	for _, q := range []string{"", "  \t", strings.Repeat("a", maxQueryBytes+1), "a\x00b", "\xff"} {
		if err := ValidateQuery(q); !errors.Is(err, ErrInvalidQuery) {
			t.Errorf("ValidateQuery(%q) = %v, want ErrInvalidQuery", q, err)
		}
	}
	if err := ValidateQuery("--version"); err != nil {
		t.Errorf("ValidateQuery(--version) = %v, want nil", err)
	}
}

// TestSearchRealRg runs the real binary against the two measured escapes: a
// RIPGREP_CONFIG_PATH holding --follow (which returned symlinks pointing
// outside the root when --no-config was missing), and a query that rg parses
// as a flag when "--" is missing.
func TestSearchRealRg(t *testing.T) {
	if _, err := osexec.LookPath("rg"); err != nil {
		if os.Getenv(requireRgEnv) != "" {
			t.Fatalf("%s=1 but rg is not on PATH: %v", requireRgEnv, err)
		}
		t.Skipf("rg not on PATH: %v", err)
	}
	base := t.TempDir()
	root := filepath.Join(base, "root")
	outside := filepath.Join(base, "outside")
	for _, d := range []string{root, outside, filepath.Join(outside, "dir"), filepath.Join(root, "sub")} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	write := func(p, body string) {
		t.Helper()
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(root, "a.md"), "# A\nthe --version flag\n")
	write(filepath.Join(outside, "o.md"), "SECRETOUT\n")
	write(filepath.Join(outside, "dir", "d.md"), "SECRETOUT\n")
	if err := os.Symlink(filepath.Join(outside, "o.md"), filepath.Join(root, "link.md")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink(filepath.Join(outside, "dir"), filepath.Join(root, "sub", "linkdir")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	// --follow is the measured escape; --invert-match makes a config that
	// was read visible in the --version results even though the argv's own
	// --no-follow would override the --follow line.
	cfg := filepath.Join(base, "rgrc")
	write(cfg, "--follow\n--invert-match\n")
	t.Setenv("RIPGREP_CONFIG_PATH", cfg)

	idx, err := NewIndex([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	s := Searcher{Runner: forgexec.OSRunner{}}

	resp, err := s.Search(t.Context(), idx, "--version", 10)
	if err != nil {
		t.Fatalf("Search(--version): %v", err)
	}
	if len(resp.Results) != 1 || resp.Results[0].Path != "a.md" || !strings.Contains(resp.Results[0].Snippet, "--version flag") {
		t.Errorf("--version results = %+v, want the a.md line", resp.Results)
	}

	resp, err = s.Search(t.Context(), idx, "SECRETOUT", 10)
	if err != nil {
		t.Fatalf("Search(SECRETOUT): %v", err)
	}
	// Skipped catches rg following a symlink even though the index gate
	// then drops the hit: the gate is the backstop, --no-config the fix.
	if len(resp.Results) != 0 || resp.Skipped != 0 {
		t.Errorf("rg reached files outside the root through symlinks: results %+v, skipped %d", resp.Results, resp.Skipped)
	}
}
