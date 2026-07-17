package cli

// Test plan for docs_list.go
//
// newDocsListCmd (Classification: API handler / cobra command)
//   [x] Happy: --json emits a valid JSON array with root/path/title/modTime
//   [x] Happy: human output lists the doc's root, path, and title
//   [x] Happy: an empty root reports "no docs found" rather than an empty table
//   [x] Unhappy: a nonexistent root argument surfaces NewIndex's error

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
