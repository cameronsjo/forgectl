package cli

// Test plan for docs_roots.go
//
// resolveDocsRoots (Classification: ops layer — root-set resolution)
//   [x] Happy: explicit args replace the default set entirely
//   [x] Happy: no args defaults to [cwd] when ./docs and the env var are absent
//   [x] Happy: no args includes ./docs when it exists
//   [x] Happy: config.Docs.Roots is additive to the defaults
//   [x] Happy: $CADENCE_FIELD_REPORTS_DIR is included when set and it exists
//
// dedupPaths (Classification: helper)
//   [x] Happy: "." and its absolute equivalent collapse to one entry

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/cameronsjo/forgectl/internal/config"
)

func TestResolveDocsRoots_ArgsOverrideDefaults(t *testing.T) {
	got, err := resolveDocsRoots([]string{"a", "b"}, config.DocsConfig{Roots: []string{"c"}})
	if err != nil {
		t.Fatalf("resolveDocsRoots: %v", err)
	}
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("resolveDocsRoots(args) = %v, want exactly the given args", got)
	}
}

func TestResolveDocsRoots_NoArgs_DefaultsToCwd(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("CADENCE_FIELD_REPORTS_DIR", "")

	got, err := resolveDocsRoots(nil, config.DocsConfig{})
	if err != nil {
		t.Fatalf("resolveDocsRoots: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("resolveDocsRoots(no args) = %v, want exactly [cwd]", got)
	}
}

func TestResolveDocsRoots_IncludesDocsDirWhenPresent(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	t.Setenv("CADENCE_FIELD_REPORTS_DIR", "")

	got, err := resolveDocsRoots(nil, config.DocsConfig{})
	if err != nil {
		t.Fatalf("resolveDocsRoots: %v", err)
	}
	found := false
	for _, r := range got {
		if filepath.Base(r) == "docs" {
			found = true
		}
	}
	if !found {
		t.Errorf("resolveDocsRoots = %v, want it to include ./docs", got)
	}
}

func TestResolveDocsRoots_ConfigRootsAreAdditive(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("CADENCE_FIELD_REPORTS_DIR", "")

	extra := t.TempDir()
	got, err := resolveDocsRoots(nil, config.DocsConfig{Roots: []string{extra}})
	if err != nil {
		t.Fatalf("resolveDocsRoots: %v", err)
	}
	found := false
	for _, r := range got {
		if r == extra {
			found = true
		}
	}
	if !found {
		t.Errorf("resolveDocsRoots = %v, want it to include configured extra root %q", got, extra)
	}
}

func TestResolveDocsRoots_IncludesFieldReportsDirWhenSet(t *testing.T) {
	cwd := t.TempDir()
	t.Chdir(cwd)
	frDir := t.TempDir()
	t.Setenv("CADENCE_FIELD_REPORTS_DIR", frDir)

	got, err := resolveDocsRoots(nil, config.DocsConfig{})
	if err != nil {
		t.Fatalf("resolveDocsRoots: %v", err)
	}
	found := false
	for _, r := range got {
		if r == frDir {
			found = true
		}
	}
	if !found {
		t.Errorf("resolveDocsRoots = %v, want it to include $CADENCE_FIELD_REPORTS_DIR %q", got, frDir)
	}
}

func TestDedupPaths_CollapsesEquivalentAbsolutePaths(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	got := dedupPaths([]string{".", dir})
	if len(got) != 1 {
		t.Errorf("dedupPaths([., abs]) = %v, want exactly one entry", got)
	}
}
