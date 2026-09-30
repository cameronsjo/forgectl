package pr

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every os.Root this package opens by path goes through openDirRoot, whose
// first open cannot block on a FIFO (forgectl#792). A new os.OpenRoot would
// reopen the hang wherever it runs outside the lifecycle lock, so this fails
// the moment os.OpenRoot is referenced anywhere but pinDirRoot, openDirRoot's
// second half: called in a function, or taken as a value (a package-level
// `var openRoot = os.OpenRoot` seam, a func-typed field, an argument). It also
// requires pinDirRoot's own call, so a scan that finds nothing cannot pass.
//
// Mutations that turn it red: revert any of the seven sites (ownerRecordLive,
// openFindingsStore, the two prune opens, the three teardown opens) to
// os.OpenRoot(c.sessionsDir) or os.OpenRoot(c.findingsDir); or add a
// package-level `var openRootSeam = os.OpenRoot` to any non-test file.
func TestOpenDirRoot_IsTheOnlyPathRootOpener(t *testing.T) {
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	inPin := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(filepath.Clean(name))
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, name, src, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, d := range f.Decls {
			where := "package scope"
			fd, isFunc := d.(*ast.FuncDecl)
			if isFunc {
				where = fd.Name.Name
			}
			ast.Inspect(d, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "OpenRoot" {
					return true
				}
				if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "os" {
					return true
				}
				if isFunc && fd.Name.Name == "pinDirRoot" && fd.Recv == nil {
					inPin++
					return true
				}
				t.Errorf("%s: os.OpenRoot referenced in %s; open the directory with openDirRoot so a FIFO at the path cannot block it",
					fset.Position(sel.Pos()), where)
				return true
			})
		}
	}
	if inPin != 1 {
		t.Fatalf("found %d os.OpenRoot references in pinDirRoot, want 1; the check cannot see the open it guards", inPin)
	}
}
