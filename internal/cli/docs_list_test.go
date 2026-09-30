package cli

// Test plan for docs_list.go
//
// newDocsListCmd (Classification: API handler / cobra command)
//   [x] Happy: --json emits a valid JSON array with root/path/title/modTime
//   [x] Happy: human output lists the doc's root, path, and title
//   [x] Happy: an empty root reports "no docs found" rather than an empty table
//   [x] Unhappy: a nonexistent root argument surfaces NewIndex's error, exit 2
//   [x] Security: human output escapes terminal controls in a filename and in
//       a doc's H1 (forgectl#598)
//   [x] Security: human output caps a doc's H1 at 256 runes; --json carries it
//       whole (forgectl#894)
//   [x] Happy: --limit 3 prints three rows in both the human and --json shapes
//   [x] Unhappy: a --timeout deadline under --json leaves stdout empty and
//       writes exactly one JSON error object to stderr, exit code 2
//   [x] Unhappy: a --timeout deadline without --json renders a human error
//       naming the root, exit code 2
//   [x] Unhappy: --limit rejects a negative count, exit 2
//
// deadlineRoot (Classification: helper — which root actually stalled)
//   [x] Happy: a *docspkg.WalkDeadlineError's own Root wins over the
//       caller's first-root fallback (NewIndexContext may be walking any of
//       several roots when ctx.Err() fires; it is not necessarily the first)
//   [x] Happy: an error carrying no WalkDeadlineError falls back to the
//       caller-supplied root

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/spf13/cobra"

	docspkg "github.com/cameronsjo/forgectl/internal/docs"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

func TestDocsListCmd_JSONFlag_EmitsArray(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "page.md"), []byte("# Page"), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := newDocsListCmd(module.Deps{})
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"--json", dir})

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var got []docJSON
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not a valid JSON array: %v\nstdout: %s", err, stdout.String())
	}
	if len(got) != 1 {
		t.Fatalf("got %d entries, want 1: %+v", len(got), got)
	}
	if got[0].Path != "page.md" || got[0].Title != "Page" {
		t.Errorf("entry = %+v, want Path=page.md Title=Page", got[0])
	}
}

func TestDocsListCmd_HumanOutput_ListsDoc(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "page.md"), []byte("# Page"), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := newDocsListCmd(module.Deps{})
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{dir})

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(stdout.String(), "page.md") || !strings.Contains(stdout.String(), "Page") {
		t.Errorf("human output missing doc entry: %q", stdout.String())
	}
}

func TestDocsListCmd_EmptyRoot_ReportsNoDocsFound(t *testing.T) {
	dir := t.TempDir()

	cmd := newDocsListCmd(module.Deps{})
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{dir})

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(stdout.String(), "no docs found") {
		t.Errorf("output = %q, want it to report no docs found", stdout.String())
	}
}

func TestDocsListCmd_NonexistentRoot_Errors(t *testing.T) {
	cmd := newDocsListCmd(module.Deps{})
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{filepath.Join(t.TempDir(), "missing")})

	err := cmd.ExecuteContext(context.Background())
	if err == nil {
		t.Fatal("expected an error for a nonexistent root")
	}
	// 2, not the default 1: the list could not be produced (forgectl#577).
	if got := ExitCode(err); got != 2 {
		t.Errorf("ExitCode(err) = %d, want 2", got)
	}
}

// A filename and a doc's H1 both reach the human table, and both come from
// disk, so neither may put a raw control byte on the terminal (forgectl#598).
func TestDocsListCmd_HumanOutput_EscapesTerminalControls(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "e\x1b[31mvil.md"), []byte("# Plain\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "page.md"), []byte("# Hi\x1b]0;pwned\x07\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := newDocsListCmd(module.Deps{})
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{dir})

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	out := stdout.String()
	if strings.ContainsAny(out, "\x1b\x07") {
		t.Errorf("stdout carries a raw control byte: %q", out)
	}
	for _, want := range []string{"e\\x1b[31mvil.md", "Hi\\x1b]0;pwned\\a"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout = %q, want the escaped form %q", out, want)
		}
	}
}

// docsListTextLineMaxRunes is the widest `docs list` text line
// TestPrintDocsList_TextCapsTitle can produce: the root label padded to 16,
// a space, the path padded to 48, a space, and the title's 256-rune cap plus
// its " … [truncated]" marker (14). Literal, so raising docTitleMaxRunes
// cannot raise its own bound.
const docsListTextLineMaxRunes = 16 + 1 + 48 + 1 + 256 + 14

// TestPrintDocsList_TextCapsTitle pins forgectl#894 item 1: a doc's H1 is
// capped in text output, and --json carries it whole.
//
// Mutations that turn it red: print d.Title through termsafe.SafeLine in
// printDocsList, or raise docTitleMaxRunes to 1000.
func TestPrintDocsList_TextCapsTitle(t *testing.T) {
	long := strings.Repeat("\u03c4", 5000) // Greek tau: nothing else on the line uses it
	docs := []docspkg.Doc{{RootLabel: "docs", RelPath: "a.md", AbsPath: "/r/a.md", Title: long}}

	text, _ := renderCmd(t, func(cmd *cobra.Command) error { return printDocsList(cmd, docs, false) })
	line := strings.TrimSuffix(text, "\n")
	if strings.Contains(line, "\n") {
		t.Fatalf("one doc printed more than one line: %q", text)
	}
	if n := utf8.RuneCountInString(line); n > docsListTextLineMaxRunes {
		t.Errorf("docs list line is %d runes, over %d: the title is not capped", n, docsListTextLineMaxRunes)
	}
	if !strings.HasSuffix(line, termsafe.TruncatedMarker) {
		t.Errorf("docs list line does not end in the truncation marker: head %q", line[:80])
	}
	if !strings.Contains(line, strings.Repeat("\u03c4", 128)) {
		t.Errorf("docs list line lost the title's head")
	}

	asJSON, _ := renderCmd(t, func(cmd *cobra.Command) error { return printDocsList(cmd, docs, true) })
	if !strings.Contains(asJSON, long) {
		t.Errorf("docs list --json did not carry the title whole")
	}
}

func writeDocsListFixture(t *testing.T, n int) string {
	t.Helper()
	dir := t.TempDir()
	for i := 0; i < n; i++ {
		name := filepath.Join(dir, "page"+strings.Repeat("x", i)+".md")
		if err := os.WriteFile(name, []byte("# Page"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestDocsListCmd_Limit_HumanShape_PrintsNRows(t *testing.T) {
	dir := writeDocsListFixture(t, 5)

	cmd := newDocsListCmd(module.Deps{})
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"--limit", "3", dir})

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	lines := strings.Split(strings.TrimRight(stdout.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d rows, want 3: %q", len(lines), stdout.String())
	}
}

func TestDocsListCmd_Limit_JSONShape_ParsesAsThreeElementArray(t *testing.T) {
	dir := writeDocsListFixture(t, 5)

	cmd := newDocsListCmd(module.Deps{})
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"--json", "--limit", "3", dir})

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var got []docJSON
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not a valid JSON array: %v\nstdout: %s", err, stdout.String())
	}
	if len(got) != 3 {
		t.Fatalf("got %d entries, want 3: %+v", len(got), got)
	}
}

func TestDocsListCmd_Deadline_JSON_EmptyStdoutOneStderrObjectExit2(t *testing.T) {
	dir := writeDocsListFixture(t, 5)

	cmd := newDocsListCmd(module.Deps{})
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"--json", "--timeout", "1ns", dir})

	err := cmd.ExecuteContext(context.Background())
	if err == nil {
		t.Fatal("expected a deadline error, got nil")
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want empty", stdout.String())
	}
	var obj docsErrorJSON
	dec := json.NewDecoder(&stderr)
	if decErr := dec.Decode(&obj); decErr != nil {
		t.Fatalf("stderr is not a valid JSON object: %v\nstderr: %s", decErr, stderr.String())
	}
	if dec.More() {
		t.Errorf("stderr carries more than one JSON value: %s", stderr.String())
	}
	if obj.Code != 2 {
		t.Errorf("obj.Code = %d, want 2", obj.Code)
	}
	// obj.Root is the WalkDeadlineError's own canonical root (deadlineRoot),
	// not dir as written: NewIndexContext may be walking any of several
	// caller-supplied roots when ctx.Err() fires, so the JSON error object
	// must name the one that actually stalled rather than assume it's the
	// caller's first argument.
	canonical, canonErr := docspkg.CanonicalizeRoot(dir)
	if canonErr != nil {
		t.Fatalf("CanonicalizeRoot(%q): %v", dir, canonErr)
	}
	if obj.Root != canonical {
		t.Errorf("obj.Root = %q, want %q", obj.Root, canonical)
	}
	if got := ExitCode(err); got != 2 {
		t.Errorf("ExitCode(err) = %d, want 2", got)
	}
}

// The "indexing <root> …" progress line must not precede the deadline's JSON
// error object under --json (#672). The timer races the walk, so the run is
// repeated: the line, if emitted, lands in at least one iteration.
func TestDocsListCmd_Deadline_JSON_ProgressLineNeverPrecedesObject(t *testing.T) {
	orig := docsListProgressDelay
	docsListProgressDelay = time.Nanosecond
	t.Cleanup(func() { docsListProgressDelay = orig })
	dir := writeDocsListFixture(t, 5)

	for i := range 200 {
		cmd := newDocsListCmd(module.Deps{})
		var stdout, stderr bytes.Buffer
		cmd.SetOut(&stdout)
		cmd.SetErr(&stderr)
		cmd.SetArgs([]string{"--json", "--timeout", "1ns", dir})
		if err := cmd.ExecuteContext(context.Background()); err == nil {
			t.Fatal("expected a deadline error, got nil")
		}
		dec := json.NewDecoder(strings.NewReader(stderr.String()))
		var obj docsErrorJSON
		if err := dec.Decode(&obj); err != nil || dec.More() {
			t.Fatalf("iteration %d: stderr is not exactly one JSON object (decode err %v): %q", i, err, stderr.String())
		}
	}
}

func TestDocsListCmd_Deadline_Human_NamesRootExit2(t *testing.T) {
	dir := writeDocsListFixture(t, 5)

	cmd := newDocsListCmd(module.Deps{})
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"--timeout", "1ns", dir})

	err := cmd.ExecuteContext(context.Background())
	if err == nil {
		t.Fatal("expected a deadline error, got nil")
	}
	canonical, canonErr := docspkg.CanonicalizeRoot(dir)
	if canonErr != nil {
		t.Fatalf("CanonicalizeRoot(%q): %v", dir, canonErr)
	}
	if !strings.Contains(err.Error(), canonical) {
		t.Errorf("err = %q, want it to name the root %q", err.Error(), canonical)
	}
	if got := ExitCode(err); got != 2 {
		t.Errorf("ExitCode(err) = %d, want 2", got)
	}
}

// A non-deadline failure under --json must take the same one-object shape as a
// deadline, not fang's human error frame (forgectl#577).
func TestDocsListCmd_NonDeadlineFailure_JSON_EmptyStdoutOneStderrObjectExit2(t *testing.T) {
	cmd := newDocsListCmd(module.Deps{})
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"--json", filepath.Join(t.TempDir(), "missing")})

	err := cmd.ExecuteContext(context.Background())
	if err == nil {
		t.Fatal("expected an error for a nonexistent root")
	}
	if got := ExitCode(err); got != 2 {
		t.Errorf("ExitCode(err) = %d, want 2", got)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want empty", stdout.String())
	}
	var obj docsErrorJSON
	dec := json.NewDecoder(&stderr)
	if decErr := dec.Decode(&obj); decErr != nil {
		t.Fatalf("stderr is not a JSON object: %v\nstderr: %s", decErr, stderr.String())
	}
	if dec.More() {
		t.Errorf("stderr carries more than one JSON value: %s", stderr.String())
	}
	if obj.Code != 2 || obj.Error == "" {
		t.Errorf("error object = %+v, want code 2 and a message", obj)
	}
}

func TestDocsListCmd_BadFlag_Exit2(t *testing.T) {
	cmd := newDocsListCmd(module.Deps{})
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"--bogus"})
	err := cmd.ExecuteContext(context.Background())
	if err == nil {
		t.Fatal("expected an error for an unknown flag")
	}
	if got := ExitCode(err); got != 2 {
		t.Errorf("ExitCode(err) = %d, want 2", got)
	}
}

func TestDocsListCmd_NegativeLimit_Errors(t *testing.T) {
	dir := writeDocsListFixture(t, 1)

	cmd := newDocsListCmd(module.Deps{})
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"--limit", "-1", dir})

	err := cmd.ExecuteContext(context.Background())
	if err == nil {
		t.Fatal("expected an error for --limit -1")
	}
	if got := ExitCode(err); got != 2 {
		t.Errorf("ExitCode(err) = %d, want 2", got)
	}
}

func TestDeadlineRoot_WalkDeadlineError_WinsOverFallback(t *testing.T) {
	err := &docspkg.WalkDeadlineError{Root: "/second/root", Err: context.DeadlineExceeded}
	if got := deadlineRoot(err, "/first/root"); got != "/second/root" {
		t.Errorf("deadlineRoot = %q, want the WalkDeadlineError's own root %q", got, "/second/root")
	}
}

func TestDeadlineRoot_NoWalkDeadlineError_FallsBack(t *testing.T) {
	err := context.DeadlineExceeded
	if got := deadlineRoot(err, "/first/root"); got != "/first/root" {
		t.Errorf("deadlineRoot = %q, want the fallback %q", got, "/first/root")
	}
}

func TestDocsListCmd_TimeoutDefault_Is15Seconds(t *testing.T) {
	cmd := newDocsListCmd(module.Deps{})
	f := cmd.Flags().Lookup("timeout")
	if f == nil {
		t.Fatal("--timeout flag not registered")
	}
	if f.DefValue != (15 * time.Second).String() {
		t.Errorf("--timeout default = %q, want %q", f.DefValue, (15 * time.Second).String())
	}
}
