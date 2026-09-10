package main

import (
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// charmV1Modules are the module paths the charm.land/v2 migration retired.
// muesli/termenv is on the list because it was lipgloss v1's colour-profile
// backend: if it reappears in the build list, a v1 rendering path came back
// with it.
var charmV1Modules = []string{
	"github.com/charmbracelet/lipgloss",
	"github.com/charmbracelet/bubbletea",
	"github.com/charmbracelet/bubbles",
	"github.com/charmbracelet/huh",
	"github.com/muesli/termenv",
}

// TestExactlyOneLipgloss pins the invariant the migration bought: forgectl
// links one lipgloss, not two.
//
// It asserts against the BUILD LIST (`go list -m all`) rather than go.mod,
// because a second major arrives as an indirect requirement of a dependency —
// which is exactly how the two-lipgloss situation started, with fang pulling
// charm.land/lipgloss/v2 in beside the v1 the TUI imported. go.mod's direct
// block would have shown nothing wrong.
//
// Two majors of one styling library is not merely duplication: they disagree
// about where colour degradation happens. v1 downgraded inside Style.Render;
// v2 does it at the writer. Code written against one behaves differently under
// the other, and NO_COLOR compliance is decided by which one a given call site
// happened to use.
func TestExactlyOneLipgloss(t *testing.T) {
	// Resolved once: `go list -m all` walks the whole module graph and is slow
	// cold, and both checks below read the same list.
	modules := buildList(t)

	var lipgloss []string
	for _, line := range modules {
		path, _, _ := strings.Cut(line, " ")
		for _, retired := range charmV1Modules {
			if path == retired {
				t.Errorf("%s is back in the build list; forgectl links one charm major (see docs/plans/2026-09-05-tui-theme-and-hub.md)", path)
			}
		}
		if strings.HasPrefix(path, "charm.land/lipgloss/") || path == "github.com/charmbracelet/lipgloss" {
			lipgloss = append(lipgloss, path)
		}
	}
	if len(lipgloss) != 1 {
		t.Errorf("build list has %d lipgloss modules %v, want exactly 1", len(lipgloss), lipgloss)
	}
}

// TestNoLipglossCompatShim rejects charm.land/lipgloss/v2/compat, the package
// that exists to let v1-shaped code keep running under v2. Importing it would
// reintroduce v1's colour semantics inside the v2 module — the two-majors
// problem again, this time invisible to TestExactlyOneLipgloss because it is
// one module.
func TestNoLipglossCompatShim(t *testing.T) {
	// The whole module, not just internal/ — main.go and any future root-package
	// file could import the shim and an internal/-only walk would stay green.
	hits, err := findCompatShimImports(".")
	if err != nil {
		t.Fatalf("scan for the compat shim: %v", err)
	}
	for _, h := range hits {
		t.Errorf("%s imports the lipgloss v1 compatibility shim; write against the v2 API directly", h)
	}
}

// compatShim is the package whose whole purpose is to run v1-shaped code under
// lipgloss v2 — reintroducing v1's colour semantics inside the v2 module.
const compatShim = "charm.land/lipgloss/v2/compat"

// findCompatShimImports walks root and returns every Go file importing the
// shim.
//
// It parses import declarations rather than grepping the source. A Go import
// path is a string literal and a RAW literal is legal, so a substring search for
// the double-quoted spelling can be walked past — in a check whose entire job is
// to be unwalkable. Unquoting the parsed path covers both forms.
//
// It is a function rather than test-body code so a test can point it at a
// fixture directory and confirm it actually catches a shim import. A check
// nothing exercises is a check nobody knows still works.
func findCompatShimImports(root string) ([]string, error) {
	fset := token.NewFileSet()
	var hits []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// testdata holds deliberately odd fixtures.
			if d.Name() == "testdata" {
				return filepath.SkipDir
			}
			// Dot-directories (.git, .claude — including nested worktrees under
			// .claude/worktrees/) are never source; skip the whole family by name
			// shape rather than enumerating each one. path != root guards the
			// walk root itself, whose DirEntry.Name() can also start with ".".
			if path != root && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		file, parseErr := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if parseErr != nil {
			// Recorded and skipped rather than returned: returning aborts the
			// whole walk, so one unparseable file would hide a real shim import
			// in every file after it.
			hits = append(hits, path+" (unparseable: "+parseErr.Error()+")")
			return nil
		}
		for _, imp := range file.Imports {
			p, unquoteErr := strconv.Unquote(imp.Path.Value)
			if unquoteErr != nil {
				hits = append(hits, path+" (unparseable import path "+imp.Path.Value+")")
				continue
			}
			if p == compatShim {
				hits = append(hits, path)
			}
		}
		return nil
	})
	return hits, err
}

// TestFindCompatShimImports runs the real scanner against fixtures, including
// the raw-string import form that a substring search would walk past.
//
// It calls findCompatShimImports rather than re-deriving the parse in the test,
// because a test that reimplements the thing it checks pins the standard
// library's behaviour and not forgectl's — it would stay green if the scanner
// regressed to a grep tomorrow.
func TestFindCompatShimImports(t *testing.T) {
	tests := []struct {
		name    string
		src     string
		wantHit bool
	}{
		{
			name:    "interpreted string import",
			src:     "package p\nimport compat \"" + compatShim + "\"\n",
			wantHit: true,
		},
		{
			// Legal Go, and invisible to a search for the quoted spelling.
			name:    "raw string import",
			src:     "package p\nimport compat `" + compatShim + "`\n",
			wantHit: true,
		},
		{
			name: "blank-imported shim",
			src:  "package p\nimport _ \"" + compatShim + "\"\n",
			// A blank import still links the package and its side effects.
			wantHit: true,
		},
		{
			name: "unrelated import",
			src:  "package p\nimport \"charm.land/lipgloss/v2\"\n",
		},
		{
			// A mention in a comment or a string constant is not an import.
			// Narrowing away from these was the point of parsing.
			name: "shim named in a comment and a constant",
			src:  "package p\n\n// see " + compatShim + "\nconst s = \"" + compatShim + "\"\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "x.go"), []byte(tt.src), 0o600); err != nil {
				t.Fatalf("write fixture: %v", err)
			}
			hits, err := findCompatShimImports(dir)
			if err != nil {
				t.Fatalf("scan: %v", err)
			}
			if got := len(hits) > 0; got != tt.wantHit {
				t.Errorf("found shim = %v, want %v (hits %v)\nsource:\n%s", got, tt.wantHit, hits, tt.src)
			}
		})
	}
}

// TestFindCompatShimImports_KeepsWalkingPastAnUnparseableFile pins that one bad
// file cannot hide a shim import in a file scanned after it. Returning the parse
// error from the WalkDir callback aborts the walk, which fails closed for the
// bad file and silently open for everything behind it.
func TestFindCompatShimImports_KeepsWalkingPastAnUnparseableFile(t *testing.T) {
	dir := t.TempDir()
	// "aaa" sorts before "zzz", so the broken file is visited first.
	if err := os.WriteFile(filepath.Join(dir, "aaa.go"), []byte("this is not go"), 0o600); err != nil {
		t.Fatal(err)
	}
	shimSrc := "package p\nimport compat \"" + compatShim + "\"\n"
	if err := os.WriteFile(filepath.Join(dir, "zzz.go"), []byte(shimSrc), 0o600); err != nil {
		t.Fatal(err)
	}

	hits, err := findCompatShimImports(dir)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	found := false
	for _, h := range hits {
		if strings.HasSuffix(h, "zzz.go") {
			found = true
		}
	}
	if !found {
		t.Errorf("the shim import behind an unparseable file was not reported; hits = %v", hits)
	}
}

// TestFindCompatShimImports_SkipsDotDirectories pins the forgectl#480 fix: a
// shim import planted under a nested dot-directory — the same shape as a
// `.claude/worktrees/<slug>` checkout left behind by another session — must
// never surface as a hit. Without the skip, this test fails exactly the way
// TestNoColorLiteralsOutsideTheme did against a real worktree tree.
func TestFindCompatShimImports_SkipsDotDirectories(t *testing.T) {
	dir := t.TempDir()
	nested := filepath.Join(dir, ".claude", "worktrees", "some-other-session")
	if err := os.MkdirAll(nested, 0o750); err != nil {
		t.Fatal(err)
	}
	shimSrc := "package p\nimport compat \"" + compatShim + "\"\n"
	if err := os.WriteFile(filepath.Join(nested, "x.go"), []byte(shimSrc), 0o600); err != nil {
		t.Fatal(err)
	}

	hits, err := findCompatShimImports(dir)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(hits) != 0 {
		t.Errorf("shim import under a nested dot-directory was reported, want it skipped; hits = %v", hits)
	}
}

// buildList returns `go list -m all`, one module per line.
//
// Shelling out to the toolchain is the only way to see the resolved build
// list — parsing go.mod would miss exactly the indirect requirement this test
// exists to catch. The skip mirrors release_workflow_security_test.go's
// treatment of tools that may be absent from a minimal environment.
func buildList(t *testing.T) []string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not on PATH; cannot resolve the module build list")
	}
	out, err := exec.CommandContext(t.Context(), "go", "list", "-m", "all").CombinedOutput()
	if err != nil {
		t.Fatalf("go list -m all: %v\n%s", err, out)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	// A build list with a handful of entries means the command reported
	// something other than the real graph; better to fail than to pass an
	// assertion over nothing.
	if len(lines) < 10 {
		t.Fatalf("go list -m all returned %d lines; the build list was not resolved:\n%s", len(lines), out)
	}
	return lines
}
