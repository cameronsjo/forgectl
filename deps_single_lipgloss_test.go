package main

import (
	"os"
	"os/exec"
	"path/filepath"
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
	const shim = `"charm.land/lipgloss/v2/compat"`
	err := filepath.WalkDir("internal", func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if strings.Contains(string(data), shim) {
			t.Errorf("%s imports the lipgloss v1 compatibility shim; write against the v2 API directly", path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk internal: %v", err)
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
	out, err := exec.Command("go", "list", "-m", "all").CombinedOutput()
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
