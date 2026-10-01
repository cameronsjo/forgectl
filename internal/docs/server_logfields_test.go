package docs

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"testing"
)

// TestServerSlogRequestPathFieldsAreCapped pins forgectl#959 item 1: a slog
// call in server.go whose fields carry the request's root or rest passes
// them through termsafe (SafePathMax), as handleDoc's other fields do. A raw
// value is attacker-sized (the request path is uncapped) and slog's text
// handler quotes but does not bound it. The rule is structural: every
// "root", "rest" or "path" key in a slog call takes a call expression, never
// a bare identifier or a concatenation.
//
// Mutation that turns it red: put the raw root back in the "markdown render
// failed" call: slog.Error("...", "root", root, ...).
func TestServerSlogRequestPathFieldsAreCapped(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "server.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "slog" {
			return true
		}
		// Fields follow the message as key, value pairs.
		for i := 1; i+1 < len(call.Args); i += 2 {
			key, ok := call.Args[i].(*ast.BasicLit)
			if !ok || key.Kind != token.STRING {
				continue
			}
			name, err := strconv.Unquote(key.Value)
			if err != nil || (name != "root" && name != "rest" && name != "path") {
				continue
			}
			checked++
			val, ok := call.Args[i+1].(*ast.CallExpr)
			fun, isSel := ast.Expr(nil), false
			if ok {
				fun = val.Fun
				_, isSel = fun.(*ast.SelectorExpr)
			}
			if !ok || !isSel || selName(fun) != "termsafe.SafePathMax" {
				t.Errorf("%s: slog field %q is not wrapped in termsafe.SafePathMax", fset.Position(key.Pos()), name)
			}
		}
		return true
	})
	if checked < 5 {
		t.Errorf("checked %d root/rest/path slog fields, want at least 5; the walk no longer finds the calls", checked)
	}
}

// selName renders a selector expression as pkg.Name.
func selName(e ast.Expr) string {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok {
		return ""
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok {
		return ""
	}
	return pkg.Name + "." + sel.Sel.Name
}
