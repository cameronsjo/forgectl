package exec

import (
	"encoding/json"
	"go/ast"
	"go/types"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
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

// TestOnlyBuildCmdReachesSealedCommand keeps the one reveal behind the one
// door: sealed.Command, which fills an *exec.Cmd with the plaintext, is named
// in internal/exec's production files only inside
// (*OSSensitiveRunner).buildCmd, on every platform in guardPlatforms. A call
// elsewhere, or a reference that stores the func for later, is a finding.
// Sealing makes a reveal outside internal/exec uncompilable; inside it, this
// is what keeps the reveal from spreading past the runner.
//
// Mutation that turns it red: `var _ = sealed.Command` in sensitive.go.
func TestOnlyBuildCmdReachesSealedCommand(t *testing.T) {
	for _, p := range guardPlatforms {
		c := checkExecFor(t, p)
		command, ok := c.sealed.Scope().Lookup("Command").(*types.Func)
		if !ok {
			t.Fatalf("[%s] sealed declares no Command func; the rule would check nothing", p)
		}
		inBuildCmd := 0
		for _, f := range c.files {
			for _, decl := range f.Decls {
				fd, isFunc := decl.(*ast.FuncDecl)
				door := isFunc && fd.Name.Name == "buildCmd" && fd.Recv != nil && len(fd.Recv.List) == 1 &&
					types.ExprString(fd.Recv.List[0].Type) == "*OSSensitiveRunner"
				ast.Inspect(decl, func(n ast.Node) bool {
					id, ok := n.(*ast.Ident)
					if !ok || c.info.Uses[id] != command {
						return true
					}
					if door {
						inBuildCmd++
						return true
					}
					t.Errorf("[%s] %s: sealed.Command is named outside (*OSSensitiveRunner).buildCmd; the reveal must stay behind that one door",
						p, c.fset.Position(id.Pos()))
					return true
				})
			}
		}
		if inBuildCmd == 0 {
			t.Fatalf("[%s] found no use of sealed.Command in buildCmd; the matcher is broken, not the package clean", p)
		}
	}
}
