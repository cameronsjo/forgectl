package merge

import (
	"go/parser"
	"go/token"
	"os"
	osexec "os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const modulePath = "github.com/cameronsjo/forgectl"

// moduleRoot is the directory holding this module's go.mod.
func moduleRoot(t *testing.T) string {
	t.Helper()
	out, err := osexec.CommandContext(t.Context(), "go", "env", "GOMOD").Output() //nolint:gosec // G204: a literal go env call
	if err != nil {
		t.Fatalf("go env GOMOD: %v", err)
	}
	gomod := strings.TrimSpace(string(out))
	if filepath.Base(gomod) != "go.mod" {
		t.Fatalf("go env GOMOD = %q, expected a go.mod path", gomod)
	}
	return filepath.Dir(gomod)
}

// mergePathRoots are the packages the merge path starts from: this package,
// and every module package imported by internal/cli's surface*.go files and
// execute.go (the surface commands, the drain, and the runner every gate
// read goes through), parsed whatever their build tags. internal/cli itself
// is refused whole (internal/cli/**) but not followed as a root: the
// package links every command, so its closure is the whole module.
func mergePathRoots(t *testing.T, root string) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(root, "internal", "cli", "surface*.go"))
	if err != nil {
		t.Fatal(err)
	}
	files = append(files, filepath.Join(root, "internal", "cli", "execute.go"))
	roots := []string{modulePath + "/internal/surface/merge"}
	parsed := 0
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		parsed++
		for _, imp := range f.Imports {
			p, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Fatal(err)
			}
			if strings.HasPrefix(p, modulePath+"/") && !slices.Contains(roots, p) {
				roots = append(roots, p)
			}
		}
	}
	// Self-guard: the glob must still find the surface commands, or the
	// closure below is a closure of almost nothing.
	if parsed < 5 || !slices.Contains(roots, modulePath+"/internal/surface/worker") {
		t.Fatalf("parsed %d merge-path files with roots %q; expected the surface commands", parsed, roots)
	}
	return roots
}

// TestBuiltinRefusalsCoverTheGateClosure derives the module packages the
// merge path compiles in, for each target OS, and checks a file in each one
// is a built-in refusal. A new dependency of the gate fails here until
// builtinRefusedGlobs lists it (T10.2 security review I2).
func TestBuiltinRefusalsCoverTheGateClosure(t *testing.T) {
	if testing.Short() {
		t.Skip("runs go list")
	}
	root := moduleRoot(t)
	roots := mergePathRoots(t, root)
	seen := map[string]bool{}
	for _, goos := range []string{"darwin", "linux", "windows"} {
		args := append([]string{"list", "-deps", "-f", "{{.ImportPath}}"}, roots...)
		cmd := osexec.CommandContext(t.Context(), "go", args...) //nolint:gosec // G204: go with package paths parsed from this module's own files
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "GOOS="+goos)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("GOOS=%s go list -deps: %v", goos, err)
		}
		for _, pkg := range strings.Fields(string(out)) {
			if strings.HasPrefix(pkg, modulePath+"/internal/") {
				seen[pkg] = true
			}
		}
	}
	for _, want := range []string{"internal/surface/merge", "internal/githubauth", "internal/exec", "internal/termsafe", "internal/privdir"} {
		if !seen[modulePath+"/"+want] {
			t.Fatalf("the closure lacks %s; the derivation is broken", want)
		}
	}
	pkgs := make([]string, 0, len(seen))
	for p := range seen {
		pkgs = append(pkgs, p)
	}
	slices.Sort(pkgs)
	for _, pkg := range pkgs {
		file := strings.TrimPrefix(pkg, modulePath+"/") + "/x.go"
		if builtinRefusal(file) == "" {
			t.Errorf("%s is in the merge path's closure, but %s is not a built-in refusal: add its directory to builtinRefusedGlobs", pkg, file)
		}
	}
	if t.Failed() {
		return
	}
	t.Logf("%d module packages in the merge path's closure, all refused", len(pkgs))
}

// TestBuiltinRefusalsTopLevel pins that no file at the repository root is
// merged, the root configuration this repository has today included.
func TestBuiltinRefusalsTopLevel(t *testing.T) {
	for _, p := range []string{"main.go", "go.mod", ".golangci.yml", ".goreleaser.yaml", "release-please-config.json",
		".release-please-manifest.json", "AGENTS.md", "CLAUDE.md", "Makefile", "Dockerfile", ".mise.toml", ".coderabbit.yaml",
		"release_workflow_security_test.go", "README.md"} {
		if why := builtinRefusal(p); !strings.Contains(why, "top-level files") {
			t.Errorf("%s: %q, want the top-level refusal", p, why)
		}
	}
	for _, p := range []string{"scripts/check-changelog-owner.sh", "helper/x.swift", ".github/workflows/ci.yml", ".claude/settings.json",
		"internal/cli/tasks.go", "internal/cli/execute.go", "internal/pr/remote.go"} {
		if builtinRefusal(p) == "" {
			t.Errorf("%s is not refused", p)
		}
	}
	for _, p := range []string{"docs/CLAUDE.md", "internal/tasks/AGENTS.md", "docs/sub/claude.local.md",
		"docs/.claude/settings.json", "internal/tasks/.github/x.yml", "docs/.Claude/x"} {
		if builtinRefusal(p) == "" {
			t.Errorf("nested agent or CI file %s is not refused", p)
		}
	}
	for _, p := range []string{"internal/tasks/x.go", "docs/x.md"} {
		if why := builtinRefusal(p); why != "" {
			t.Errorf("%s refused: %s", p, why)
		}
	}
}
