package cli

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// TestStyledPrintsGoThroughColorOut is the structural guard behind colorOut.
//
// It exists because per-call-site wrapping already failed once, in the commit
// that introduced colorOut: six styled commands were converted and two were
// missed — `ghostty cheat` and `tmux cheat`, which print internal/tui's styled
// helpers. Piped, they emitted 117 and 23 escape sequences respectively, and
// did the same under NO_COLOR=1. Nothing caught it, because nothing was
// looking; a reviewer found it.
//
// The rule: inside package cli, cmd.OutOrStdout() may not be handed straight to
// fmt.Fprint/Fprintf/Fprintln when another argument of that same call renders
// styled text. Wrap it in colorOut(cmd), which downgrades truecolor to whatever
// the destination actually supports and strips colour entirely for a pipe or
// NO_COLOR. Lip Gloss v2 does not do this inside Style.Render — it moved the
// decision to the writer — so an unwrapped writer is a silent regression with
// no compile error.
//
// A print with no styled argument is untouched: plain text through raw stdout
// is correct and is what most of this package does.
func TestStyledPrintsGoThroughColorOut(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse package cli: %v", err)
	}
	if len(pkgs) == 0 {
		t.Fatal("parsed no packages; the guard would pass over nothing")
	}

	checked := 0
	for _, pkg := range pkgs {
		for name, file := range pkg.Files {
			ast.Inspect(file, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || !isFprint(call.Fun) || len(call.Args) < 2 {
					return true
				}
				checked++
				if !isOutOrStdout(call.Args[0]) {
					return true
				}
				for _, arg := range call.Args[1:] {
					if what := styledArg(arg); what != "" {
						pos := fset.Position(call.Pos())
						t.Errorf("%s:%d: styled output (%s) written to cmd.OutOrStdout(); wrap it in colorOut(cmd) or it emits raw ANSI into pipes and under NO_COLOR",
							filepath.Base(name), pos.Line, what)
					}
				}
				return true
			})
		}
	}
	if checked == 0 {
		t.Fatal("found no fmt.Fprint* calls in package cli; the guard matched nothing and could not have failed")
	}
}

// isFprint reports whether fun is fmt.Fprint, Fprintf, or Fprintln.
func isFprint(fun ast.Expr) bool {
	sel, ok := fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok || pkg.Name != "fmt" {
		return false
	}
	switch sel.Sel.Name {
	case "Fprint", "Fprintf", "Fprintln":
		return true
	}
	return false
}

// isOutOrStdout reports whether e is a bare cmd.OutOrStdout() call.
func isOutOrStdout(e ast.Expr) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "OutOrStdout"
}

// styledArg returns a description of the styling in e, or "" if there is none.
// It looks for a call into the tui package (whose helpers render styled sheets)
// and for any .Render( call, which is how a lipgloss style is applied.
func styledArg(e ast.Expr) string {
	found := ""
	ast.Inspect(e, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if sel.Sel.Name == "Render" {
			found = "a lipgloss .Render call"
			return false
		}
		if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "tui" {
			found = "tui." + sel.Sel.Name
			return false
		}
		return true
	})
	return found
}
