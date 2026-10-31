package cli

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// TestLedgerCallersUseTheLedgerBoundAdapter is a source-level guard
// (forgectl#1077). Each of these functions reads references only from its
// herdr session's own ledger, and must close and probe through
// Adapter.ForSessionLedger, or a herdr restart leaves every row unreadable
// again. Each builds its adapter from the herdr binary on PATH, so no unit
// test can observe which adapter it used; dropping the call from any one of
// them fails here.
func TestLedgerCallersUseTheLedgerBoundAdapter(t *testing.T) {
	want := map[string]bool{"openSurfaceLedger": false, "herdrWorkspaces": false, "drainCloseRow": false}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read internal/cli: %v", err)
	}
	fset := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			if _, tracked := want[fn.Name.Name]; !tracked {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "ForSessionLedger" {
					want[fn.Name.Name] = true
				}
				return true
			})
		}
	}
	for fn, found := range want {
		if !found {
			t.Errorf("%s does not use Adapter.ForSessionLedger: its ledger rows stay unreadable after a herdr restart", fn)
		}
	}
}
