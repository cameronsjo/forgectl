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
//   [x] Unhappy: a failed root is recorded in Errors (capped, terminal-safe),
//       never counted in Skipped, and its hits are kept
//   [x] Unhappy: a failure with empty stderr reports the CommandError's Err
//       (why rg never ran), and "exit N" only when Err is nil
//   [x] Unhappy: a failure in either of two roots keeps the other's hits
//   [x] Unhappy: an unparseable rg record is reported in Errors
//   [x] Unhappy: a root that hit the limit still reports rg's diagnostic
//       and unparseable records (probes P1, P2); the limit's own cancel
//       alone is not a failure
//   [x] Happy: overlapping roots return a shared doc once, first root wins
//   [x] Unhappy: rg found only in a relative PATH entry is refused
//   [x] Happy: real rg returns the same truncated prefix on every run
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
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// requireRgEnv, when set to any non-empty value, turns the real-rg test's
// skip into a failure. CI installs ripgrep and sets it.
const requireRgEnv = "FORGECTL_REQUIRE_RG"

type rgWrite func(ctx context.Context, stdout, stderr io.Writer) error

// fakeRg is a func-typed StreamingRunner: it records the argv and the
// context it was handed, then runs write against the stdout and stderr sinks.
// byPath, when it has an entry for the searched path (the last argv
// element), runs that instead, so one root can fail while another matches.
type fakeRg struct {
	calls  [][]string
	ctxs   []context.Context
	write  rgWrite
	byPath map[string]rgWrite
}

func (f *fakeRg) RunStreaming(ctx context.Context, _ io.Reader, stdout, stderr io.Writer, _ string, args ...string) error {
	f.calls = append(f.calls, slices.Clone(args))
	f.ctxs = append(f.ctxs, ctx)
	if w, ok := f.byPath[args[len(args)-1]]; ok {
		return w(ctx, stdout, stderr)
	}
	if f.write == nil {
		return nil
	}
	return f.write(ctx, stdout, stderr)
}

// rgFails is an rg run that prints diag to stderr and exits 2, after
// writing records to stdout.
func rgFails(diag string, records ...string) rgWrite {
	return func(ctx context.Context, stdout, stderr io.Writer) error {
		_ = writeAll(records...)(ctx, stdout, stderr)
		_, _ = io.WriteString(stderr, diag)
		return &forgexec.CommandError{Name: "rg", ExitCode: 2}
	}
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

// searchRootWith builds a one-root index holding the given files and
// contents.
func searchRootWith(t *testing.T, files map[string]string) *Index {
	t.Helper()
	dir := t.TempDir()
	for f, body := range files {
		writeFile(t, filepath.Join(dir, filepath.FromSlash(f)), body)
	}
	idx, err := NewIndex([]string{dir})
	if err != nil {
		t.Fatal(err)
	}
	return idx
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

func writeAll(records ...string) rgWrite {
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
	if !slices.Contains(argv, "--sort=path") {
		t.Errorf("argv lacks --sort=path (results and truncation would be nondeterministic): %q", argv)
	}
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
	// A three-byte rune repeated: every cut lands next to a multibyte rune,
	// and the match offset (5001) points into the middle of one. The line is
	// the doc's own line 2, which the snippet is re-read from.
	line := strings.Repeat("€", 3400) + "\n"
	idx := searchRootWith(t, map[string]string{"a.md": "# a\n" + line})
	root := idx.Roots()[0].Path
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
	if len(resp.Errors) != 0 {
		t.Errorf("Errors = %+v, want none: rg exit 1 means no match", resp.Errors)
	}
}

func TestSearchRootFailureIsRecorded(t *testing.T) {
	idx, _ := searchRoot(t, "a.md")
	rg := &fakeRg{write: rgFails("rg: permission denied\u001b[31m\n")}
	resp, err := (Searcher{Runner: rg, LookPath: fakeLookPath}).Search(t.Context(), idx, "x", 10)
	if err != nil {
		t.Fatalf("Search: %v, want the failure recorded in Errors", err)
	}
	if len(resp.Errors) != 1 || !strings.Contains(resp.Errors[0].Message, "permission denied") {
		t.Fatalf("Errors = %+v, want one entry naming rg's reason", resp.Errors)
	}
	if strings.ContainsRune(resp.Errors[0].Message, '\u001b') {
		t.Errorf("error message carries a raw ESC from rg's stderr: %q", resp.Errors[0].Message)
	}
	if resp.Skipped != 0 {
		t.Errorf("Skipped = %d, want 0: a failed root is an error, not a skip", resp.Skipped)
	}
}

// A run that fails with nothing on stderr (rg could not start, or was killed)
// reads exit -1, or exits non-1 (exit status 2); the CommandError's Err is the
// only place the reason lives.
func TestSearchSilentFailureReportsReason(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  *forgexec.CommandError
		want string
	}{
		{"start failure", &forgexec.CommandError{Name: "rg", ExitCode: -1, Err: errors.New("exec format error")}, "rg failed: exec format error"},
		{"real exit with empty stderr", &forgexec.CommandError{Name: "rg", ExitCode: 2, Err: errors.New("exit status 2")}, "rg failed: exit status 2"},
		{"nil Err", &forgexec.CommandError{Name: "rg", ExitCode: 2}, "rg failed: exit 2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			idx, _ := searchRoot(t, "a.md")
			rg := &fakeRg{write: func(context.Context, io.Writer, io.Writer) error { return tc.err }}
			resp, err := (Searcher{Runner: rg, LookPath: fakeLookPath}).Search(t.Context(), idx, "x", 10)
			if err != nil {
				t.Fatalf("Search: %v", err)
			}
			if len(resp.Errors) != 1 || resp.Errors[0].Message != tc.want {
				t.Errorf("Errors = %+v, want one entry %q", resp.Errors, tc.want)
			}
		})
	}
}

func TestSearchErrorMessageIsCapped(t *testing.T) {
	idx, _ := searchRoot(t, "a.md")
	rg := &fakeRg{write: rgFails(strings.Repeat("e", 4000))}
	resp, err := (Searcher{Runner: rg, LookPath: fakeLookPath}).Search(t.Context(), idx, "x", 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(resp.Errors) != 1 {
		t.Fatalf("Errors = %+v, want 1", resp.Errors)
	}
	if n := utf8.RuneCountInString(resp.Errors[0].Message); n > maxSearchErrorRunes+utf8.RuneCountInString(termsafe.TruncatedMarker) {
		t.Errorf("error message is %d runes, want at most %d plus the marker", n, maxSearchErrorRunes)
	}
}

// TestSearchPartialRootKeepsHitsAndReportsError: rg exit 2 alongside hits
// (an unreadable file next to readable ones) keeps the hits AND reports the
// root, so the failure is never silent.
func TestSearchPartialRootKeepsHitsAndReportsError(t *testing.T) {
	idx, root := searchRoot(t, "a.md")
	rg := &fakeRg{write: rgFails("rg: ./b.md: Permission denied (os error 13)\n", matchRecord(t, filepath.Join(root, "a.md"), "x", 0))}
	resp, err := (Searcher{Runner: rg, LookPath: fakeLookPath}).Search(t.Context(), idx, "x", 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(resp.Results) != 1 || len(resp.Errors) != 1 || resp.Skipped != 0 {
		t.Errorf("results %+v errors %+v skipped %d, want 1 result, 1 error, 0 skipped", resp.Results, resp.Errors, resp.Skipped)
	}
}

// TestSearchFailedRootDoesNotAbortOthers: a failure in either of two roots
// keeps the other root's hits.
func TestSearchFailedRootDoesNotAbortOthers(t *testing.T) {
	_, root1 := searchRoot(t, "a.md")
	_, root2 := searchRoot(t, "b.md")
	idx, err := NewIndex([]string{root1, root2})
	if err != nil {
		t.Fatal(err)
	}
	for _, failing := range []string{root1, root2} {
		t.Run(filepath.Base(failing), func(t *testing.T) {
			rg := &fakeRg{byPath: map[string]rgWrite{
				root1: writeAll(matchRecord(t, filepath.Join(root1, "a.md"), "x", 0)),
				root2: writeAll(matchRecord(t, filepath.Join(root2, "b.md"), "x", 0)),
			}}
			rg.byPath[failing] = rgFails("rg: unreadable\n")
			resp, err := (Searcher{Runner: rg, LookPath: fakeLookPath}).Search(t.Context(), idx, "x", 10)
			if err != nil {
				t.Fatalf("Search: %v", err)
			}
			if len(rg.calls) != 2 {
				t.Errorf("rg ran %d times, want once per root", len(rg.calls))
			}
			if len(resp.Results) != 1 || len(resp.Errors) != 1 {
				t.Fatalf("results %+v errors %+v, want the healthy root's hit and one error", resp.Results, resp.Errors)
			}
			if resp.Results[0].Root == resp.Errors[0].Root {
				t.Errorf("hit and error name the same root %q", resp.Results[0].Root)
			}
		})
	}
}

func TestSearchCountsUnparseableRecords(t *testing.T) {
	idx, root := searchRoot(t, "a.md")
	rg := &fakeRg{write: writeAll("{not json\n", matchRecord(t, filepath.Join(root, "a.md"), "x", 0))}
	resp, err := (Searcher{Runner: rg, LookPath: fakeLookPath}).Search(t.Context(), idx, "x", 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(resp.Results) != 1 || resp.Skipped != 0 {
		t.Errorf("results %+v skipped %d, want 1 result and 0 skipped", resp.Results, resp.Skipped)
	}
	if len(resp.Errors) != 1 || !strings.Contains(resp.Errors[0].Message, "1 rg output records could not be parsed") {
		t.Errorf("Errors = %+v, want the unparseable record reported", resp.Errors)
	}
}

// TestSearchTruncatedRootStillReportsFailure (probe P2): rg writes a
// diagnostic, then more hits than the limit, then fails. Hitting the limit
// must not hide the failure.
func TestSearchTruncatedRootStillReportsFailure(t *testing.T) {
	idx, root := searchRoot(t, "a.md", "b.md")
	rg := &fakeRg{write: func(ctx context.Context, stdout, stderr io.Writer) error {
		_, _ = io.WriteString(stderr, "rg: ./locked.md: Permission denied (os error 13)\n")
		return rgFails("",
			matchRecord(t, filepath.Join(root, "a.md"), "x", 0),
			matchRecord(t, filepath.Join(root, "b.md"), "x", 0),
		)(ctx, stdout, stderr)
	}}
	resp, err := (Searcher{Runner: rg, LookPath: fakeLookPath}).Search(t.Context(), idx, "x", 1)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(resp.Results) != 1 || !resp.Truncated {
		t.Fatalf("results %+v truncated=%v, want 1 truncated", resp.Results, resp.Truncated)
	}
	if len(resp.Errors) != 1 || !strings.Contains(resp.Errors[0].Message, "Permission denied") {
		t.Errorf("Errors = %+v, want rg's diagnostic reported despite truncation", resp.Errors)
	}
}

// TestSearchTruncatedRootStillCountsUnparsed (probe P1): an unparseable
// record before the limit is hit is still reported.
func TestSearchTruncatedRootStillCountsUnparsed(t *testing.T) {
	idx, root := searchRoot(t, "a.md", "b.md")
	rg := &fakeRg{write: writeAll(
		"{bad\n",
		matchRecord(t, filepath.Join(root, "a.md"), "x", 0),
		matchRecord(t, filepath.Join(root, "b.md"), "x", 0),
	)}
	resp, err := (Searcher{Runner: rg, LookPath: fakeLookPath}).Search(t.Context(), idx, "x", 1)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if !resp.Truncated {
		t.Fatalf("truncated = false, want true")
	}
	if len(resp.Errors) != 1 || !strings.Contains(resp.Errors[0].Message, "could not be parsed") {
		t.Errorf("Errors = %+v, want the unparseable record reported despite truncation", resp.Errors)
	}
}

// TestSearchTruncationAloneIsNotAFailure: the cancel Search triggers at the
// limit is not reported as an rg failure.
func TestSearchTruncationAloneIsNotAFailure(t *testing.T) {
	idx, root := searchRoot(t, "a.md", "b.md")
	rg := &fakeRg{write: func(ctx context.Context, stdout, stderr io.Writer) error {
		_ = writeAll(
			matchRecord(t, filepath.Join(root, "a.md"), "x", 0),
			matchRecord(t, filepath.Join(root, "b.md"), "x", 0),
		)(ctx, stdout, stderr)
		return &forgexec.CommandError{Name: "rg", ExitCode: -1, Err: ctx.Err()}
	}}
	resp, err := (Searcher{Runner: rg, LookPath: fakeLookPath}).Search(t.Context(), idx, "x", 1)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if !resp.Truncated || len(resp.Errors) != 0 {
		t.Errorf("truncated=%v errors=%+v, want truncated with no errors", resp.Truncated, resp.Errors)
	}
}

// TestSearchDedupesOverlappingRoots: cwd and cwd/docs both index docs/a.md;
// its hit comes back once, under the first root.
func TestSearchDedupesOverlappingRoots(t *testing.T) {
	_, dir := searchRoot(t, "docs/a.md")
	idx, err := NewIndex([]string{dir, filepath.Join(dir, "docs")})
	if err != nil {
		t.Fatal(err)
	}
	roots := idx.Roots()
	hit := matchRecord(t, filepath.Join(dir, "docs", "a.md"), "x", 0)
	rg := &fakeRg{write: writeAll(hit)}
	resp, err := (Searcher{Runner: rg, LookPath: fakeLookPath}).Search(t.Context(), idx, "x", 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(resp.Results) != 1 {
		t.Fatalf("results = %+v, want the shared doc once", resp.Results)
	}
	if resp.Results[0].Root != roots[0].Label || resp.Results[0].Path != "docs/a.md" {
		t.Errorf("result = %+v, want root %q path docs/a.md", resp.Results[0], roots[0].Label)
	}
}

func TestSearchRefusesRgInRelativePathEntry(t *testing.T) {
	idx, _ := searchRoot(t, "a.md")
	rg := &fakeRg{}
	dot := func(string) (string, error) { return "rg", osexec.ErrDot }
	_, err := (Searcher{Runner: rg, LookPath: dot}).Search(t.Context(), idx, "x", 10)
	if !errors.Is(err, ErrNoSearchBackend) || !strings.Contains(err.Error(), "relative PATH entry") {
		t.Fatalf("err = %v, want ErrNoSearchBackend naming the relative PATH entry", err)
	}
	if len(rg.calls) != 0 {
		t.Errorf("rg ran %d times from a relative PATH entry", len(rg.calls))
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
	requireRg(t)
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

// requireRg skips when rg is not on PATH, or fails when requireRgEnv says
// this environment is meant to run the real-rg tests.
func requireRg(t *testing.T) {
	t.Helper()
	if _, err := osexec.LookPath("rg"); err != nil {
		if os.Getenv(requireRgEnv) != "" {
			t.Fatalf("%s=1 but rg is not on PATH: %v", requireRgEnv, err)
		}
		t.Skipf("rg not on PATH: %v", err)
	}
}

// TestSearchRealRgStableOrder: the same query over the same tree returns the
// same truncated prefix every time. Unsorted, rg's parallel walk reorders
// hits between runs (measured: 9 distinct sets in 10 runs of --limit 3).
func TestSearchRealRgStableOrder(t *testing.T) {
	requireRg(t)
	files := make([]string, 60)
	for i := range files {
		files[i] = fmt.Sprintf("d%d/f%02d.md", i%7, i)
	}
	idx, _ := searchRoot(t, files...)
	s := Searcher{Runner: forgexec.OSRunner{}}
	var first []string
	for run := range 10 {
		resp, err := s.Search(t.Context(), idx, "body", 5)
		if err != nil {
			t.Fatalf("run %d: %v", run, err)
		}
		got := make([]string, len(resp.Results))
		for i, r := range resp.Results {
			got[i] = r.Path
		}
		if len(got) != 5 || !resp.Truncated {
			t.Fatalf("run %d: got %q truncated=%v, want 5 truncated", run, got, resp.Truncated)
		}
		if run == 0 {
			first = got
			continue
		}
		if !slices.Equal(got, first) {
			t.Fatalf("run %d returned %q, run 0 returned %q", run, got, first)
		}
	}
}
