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
// first open cannot block on a FIFO (forgectl#792). A new os.OpenRoot call
// would reopen the hang wherever it runs outside the lifecycle lock, so this
// fails the moment one appears anywhere but pinDirRoot, openDirRoot's second
// half. It also requires pinDirRoot's own call, so a scan that finds nothing
// cannot pass.
//
// Mutation that turns it red: revert any of the seven sites (ownerRecordLive,
// openFindingsStore, the two prune opens, the three teardown opens) to
// os.OpenRoot(c.sessionsDir) or os.OpenRoot(c.findingsDir).
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
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "OpenRoot" {
					return true
				}
				if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "os" {
					return true
				}
				if fd.Name.Name == "pinDirRoot" && fd.Recv == nil {
					inPin++
					return true
				}
				t.Errorf("%s: os.OpenRoot in %s; open the directory with openDirRoot so a FIFO at the path cannot block it",
					fset.Position(call.Pos()), fd.Name.Name)
				return true
			})
		}
	}
	if inPin != 1 {
		t.Fatalf("found %d os.OpenRoot calls in pinDirRoot, want 1; the check cannot see the open it guards", inPin)
	}
}
