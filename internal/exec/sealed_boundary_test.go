package exec

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/types"
	"io"
	"os"
	osexec "os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

// TestSealedIsUnimportableOutsideExec proves the rule the whole seal rests on
// (forgectl#854): Go's internal-package rule lets only internal/exec and its
// subpackages import internal/exec/internal/sealed, so a package anywhere else
// that tries to reach a payload through sealed does not compile.
//
// It runs a real `go build` of a probe package that imports sealed, placed at
// module paths outside internal/exec (each must fail with the go tool's "use
// of internal package ... not allowed") and inside it (each must build, which
// shows the outside failures come from the boundary and not from a broken
// probe). The probe is supplied through -overlay, so no file is written into
// the tree: a probe directory that existed while other packages' tests walk
// the module could fail them, or be seen by the module guards here.
//
// Mutation that turns it red: move sealed.go to internal/sealed (outside
// internal/exec) and point sealedImportPath and internal/exec's imports at it;
// every outside row then builds.
func TestSealedIsUnimportableOutsideExec(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := osexec.LookPath("go"); err != nil {
		t.Fatalf("the go tool is not on PATH, and this proof needs it: %v", err)
	}
	backing := filepath.Join(t.TempDir(), "probe.go")
	src := "package zzsealprobe\n\nimport \"" + sealedImportPath + "\"\n\nvar _ = sealed.New\n"
	if err := os.WriteFile(backing, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	build := func(rel string) (string, error) {
		overlay, err := json.Marshal(map[string]map[string]string{
			"Replace": {filepath.Join(root, filepath.FromSlash(rel), "probe.go"): backing},
		})
		if err != nil {
			t.Fatal(err)
		}
		overlayPath := filepath.Join(t.TempDir(), "overlay.json")
		if err := os.WriteFile(overlayPath, overlay, 0o600); err != nil {
			t.Fatal(err)
		}
		cmd := osexec.CommandContext(t.Context(), "go", "build", "-overlay", overlayPath, "./"+rel) //nolint:gosec // G204: the go tool with a probe path this test chose; nothing is tainted
		cmd.Dir = root
		cmd.Env = append(os.Environ(), pinnedGoEnv...)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	for _, rel := range []string{"zzsealprobe", "internal/zzsealprobe", "internal/surface/zzsealprobe", "internal/execzz/zzsealprobe"} {
		out, err := build(rel)
		if err == nil {
			t.Errorf("%s imports sealed and built; the payload is reachable from outside internal/exec", rel)
			continue
		}
		if want := "use of internal package " + sealedImportPath + " not allowed"; !strings.Contains(out, want) {
			t.Errorf("%s failed to build, but not on the internal-package rule (want %q):\n%s", rel, want, out)
		}
	}
	for _, rel := range []string{"internal/exec/zzsealprobe", "internal/exec/internal/zzsealprobe"} {
		if out, err := build(rel); err != nil {
			t.Errorf("%s imports sealed from inside internal/exec and failed to build, so the probe is broken: %v\n%s", rel, err, out)
		}
	}
}

// TestSealedStartHasOneCaller pins that sealed.Start, the one function that
// puts a payload into a process, is named in internal/exec's production files
// only inside (*OSSensitiveRunner).startSealed, on every platform in
// guardPlatforms. Test files are not checked.
//
// This is not what keeps plaintext out of internal/exec: the compiler does
// that, since sealed.Start returns only a *sealed.Proc (wait and kill, no
// accessor, pinned by the golden) and no exported name of sealed returns an
// *exec.Cmd. It keeps process launches through the seam to the runner, so a
// second launch path cannot appear without review. A package-level func can
// be reached only by naming it (a call or a func value), so a Uses walk sees
// every route; there is no interface or method-value shape that avoids it.
//
// Mutation that turns it red: `var _ = sealed.Start` in sensitive.go.
func TestSealedStartHasOneCaller(t *testing.T) {
	for _, p := range guardPlatforms {
		c := checkExecFor(t, p)
		start, ok := c.sealed.Scope().Lookup("Start").(*types.Func)
		if !ok {
			t.Fatalf("[%s] sealed declares no Start func; the rule would check nothing", p)
		}
		for _, f := range namedOutside(c, start, "startSealed") {
			t.Errorf("[%s] %s: sealed.Start is named outside (*OSSensitiveRunner).startSealed; launch sealed processes through the runner",
				p, f)
		}
	}
}

// namedOutside returns the position of every use of target in c's production
// files that does not sit inside the method (*OSSensitiveRunner).within, plus
// one extra finding when there is no use inside within at all, since then the
// matcher, not the package, is what is clean.
func namedOutside(c *checkedPackage, target *types.Func, within string) []string {
	var findings []string
	inside := 0
	for _, f := range c.files {
		for _, decl := range f.Decls {
			fd, isFunc := decl.(*ast.FuncDecl)
			door := isFunc && fd.Name.Name == within && fd.Recv != nil && len(fd.Recv.List) == 1 &&
				types.ExprString(fd.Recv.List[0].Type) == "*OSSensitiveRunner"
			ast.Inspect(decl, func(n ast.Node) bool {
				id, ok := n.(*ast.Ident)
				if !ok || c.info.Uses[id] != target {
					return true
				}
				if door {
					inside++
				} else {
					findings = append(findings, c.fset.Position(id.Pos()).String())
				}
				return true
			})
		}
	}
	if inside == 0 {
		findings = append(findings, "(no use inside "+within+" at all: the matcher is broken, not the package clean)")
	}
	return findings
}

// sealedImporterAllowed is every package, by import path with any test-variant
// suffix removed, that may import internal/exec/internal/sealed: internal/exec
// itself, and the test binaries go test builds for sealed's own tests.
var sealedImporterAllowed = map[string]bool{
	execImportPath:             true,
	sealedImportPath + ".test": true,
	sealedImportPath + "_test": true,
}

// TestOnlyExecImportsSealed closes what the internal-package rule leaves open:
// that rule admits every package under internal/exec, so a future
// internal/exec/<sub> could import sealed and hold a payload in a package no
// other guard reads. Every package `go list -deps -test ./...` reports, for
// every guard platform with cgo off and on, may import sealed only if
// sealedImporterAllowed names it.
//
// The same scan is then run over an -overlay probe subpackage,
// internal/exec/zzsub, that imports sealed, and it must be reported there, so
// a clean result is shown to come from the module and not from a matcher
// that sees nothing.
//
// Mutation that turns it red: a real internal/exec/zzsub/probe.go importing
// sealed (the overlay probe is that file without touching the tree).
func TestOnlyExecImportsSealed(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	for _, imp := range sealedImporters(t, root, "") {
		t.Errorf("%s imports %s; only internal/exec may hold a sealed payload, so fold the code into internal/exec or add it to sealedImporterAllowed after review",
			imp, sealedImportPath)
	}

	backing := filepath.Join(t.TempDir(), "probe.go")
	src := "package zzsub\n\nimport \"" + sealedImportPath + "\"\n\nvar _ = sealed.New\n"
	if err := os.WriteFile(backing, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	overlay, err := json.Marshal(map[string]map[string]string{
		"Replace": {filepath.Join(root, "internal", "exec", "zzsub", "probe.go"): backing},
	})
	if err != nil {
		t.Fatal(err)
	}
	overlayPath := filepath.Join(t.TempDir(), "overlay.json")
	if err := os.WriteFile(overlayPath, overlay, 0o600); err != nil {
		t.Fatal(err)
	}
	probe := execImportPath + "/zzsub"
	if got := sealedImporters(t, root, overlayPath); !slices.Contains(got, probe) {
		t.Fatalf("the scan did not report the probe %s importing sealed (got %q); the matcher is broken, not the module clean", probe, got)
	}
}

// sealedImporters returns, sorted and deduplicated, every package outside
// sealedImporterAllowed that imports sealed, across every guard platform
// with cgo off and on, reading the go tool under pinnedGoEnv and, when
// overlay is non-empty, with -overlay.
func sealedImporters(t *testing.T, root, overlay string) []string {
	t.Helper()
	strip := func(path string) string {
		base, _, _ := strings.Cut(path, " ")
		return base
	}
	type run struct {
		out []byte
		err error
	}
	var runs []*run
	var wg sync.WaitGroup
	for _, p := range guardPlatforms {
		for _, cgo := range []string{"0", "1"} {
			r := &run{}
			runs = append(runs, r)
			wg.Go(func() {
				args := []string{"list", "-deps", "-test", "-json=ImportPath,Imports"}
				if overlay != "" {
					args = append(args, "-overlay", overlay)
				}
				cmd := osexec.CommandContext(t.Context(), "go", append(args, "./...")...) //nolint:gosec // G204: the go tool with fixed arguments and an overlay path this test wrote
				cmd.Dir = root
				cmd.Env = append(append(os.Environ(), pinnedGoEnv...), "GOOS="+p.goos, "GOARCH="+p.goarch, "CGO_ENABLED="+cgo)
				var stderr bytes.Buffer
				cmd.Stderr = &stderr
				r.out, r.err = cmd.Output()
				if r.err != nil {
					r.err = fmt.Errorf("go list for %s cgo=%s: %w\n%s", p, cgo, r.err, stderr.String())
				}
			})
		}
	}
	wg.Wait()
	found := map[string]bool{}
	listed := 0
	for _, r := range runs {
		if r.err != nil {
			t.Fatal(r.err)
		}
		dec := json.NewDecoder(bytes.NewReader(r.out))
		for {
			var pkg struct {
				ImportPath string
				Imports    []string
			}
			if err := dec.Decode(&pkg); errors.Is(err, io.EOF) {
				break
			} else if err != nil {
				t.Fatal(err)
			}
			listed++
			if sealedImporterAllowed[strip(pkg.ImportPath)] {
				continue
			}
			if slices.ContainsFunc(pkg.Imports, func(imp string) bool { return strip(imp) == sealedImportPath }) {
				found[strip(pkg.ImportPath)] = true
			}
		}
	}
	if listed == 0 {
		t.Fatal("go list reported no packages; the scan is broken, not the module clean")
	}
	out := make([]string, 0, len(found))
	for imp := range found {
		out = append(out, imp)
	}
	slices.Sort(out)
	return out
}
