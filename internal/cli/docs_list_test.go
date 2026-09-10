package cli

// Test plan for docs_list.go
//
// newDocsListCmd (Classification: API handler / cobra command)
//   [x] Happy: --json emits a valid JSON array with root/path/title/modTime
//   [x] Happy: human output lists the doc's root, path, and title
//   [x] Happy: an empty root reports "no docs found" rather than an empty table
//   [x] Unhappy: a nonexistent root argument surfaces NewIndex's error
//   [x] Happy: --limit 3 prints three rows in both the human and --json shapes
//   [x] Unhappy: a --timeout deadline under --json leaves stdout empty and
//       writes exactly one JSON error object to stderr, exit code 2
//   [x] Unhappy: a --timeout deadline without --json renders a human error
//       naming the root, exit code 2
//   [x] Unhappy: --limit rejects a negative count
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

	docspkg "github.com/cameronsjo/forgectl/internal/docs"
	"github.com/cameronsjo/forgectl/internal/module"
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

	if err := cmd.ExecuteContext(context.Background()); err == nil {
		t.Fatal("expected an error for a nonexistent root")
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
	var obj docsListDeadlineJSON
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

func TestDocsListCmd_NegativeLimit_Errors(t *testing.T) {
	dir := writeDocsListFixture(t, 1)

	cmd := newDocsListCmd(module.Deps{})
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"--limit", "-1", dir})

	if err := cmd.ExecuteContext(context.Background()); err == nil {
		t.Fatal("expected an error for --limit -1")
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
