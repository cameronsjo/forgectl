package cli

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// escapedLabelRenderers are the functions permitted to produce a huh option
// label. Each applies a terminal boundary to untrusted text before it reaches
// the picker.
var escapedLabelRenderers = map[string]bool{
	"repoPickerLabel": true,
	"prPickerLabel":   true,
	"safeTerm":        true,
	"safeCandidate":   true,
	// Row renderers that apply the boundary to every field they compose. They
	// are approved as whole renderers because a label built from one is escaped
	// field by field inside it.
	"sessionRowWidth":      true,
	"sessionRow":           true,
	"projectCandidateLine": true,
}

// TestPickerLabelsAreBuiltByAnEscapingRenderer closes the seam the #281 review
// named: extracting repoPickerLabel made the label assertable, but a test that
// calls that function directly still passes when the picker stops calling it.
// The untested thing was never the renderer — it was the WIRING.
//
// So this asserts the wiring. Every huh.NewOption in this package must take a
// label whose every dynamic leaf passes through an approved renderer; the
// option's second argument, the selection key, is deliberately unconstrained
// because it is never printed.
//
// Three properties are load-bearing, and each exists because a weaker version of
// this guard was shown to approve a raw label:
//
//   - EVERY dynamic leaf must be approved, not merely one of them. Recursing
//     into a call's arguments is what lets truncate(safeTerm(x), n) stay
//     approved, but the same recursion approved
//     fmt.Sprintf("%s %s", safeTerm(a), raw) — and growing a label by one field
//     is exactly how a picker row changes.
//   - Locals are tracked PER FUNCTION, in source order. A file-scoped map is
//     judged on the last assignment anywhere in the file, and `label` is the
//     local name in two different functions today — so a raw label passed or a
//     correct one failed purely on the order the functions happen to appear in.
//   - A re-wrap carries approval only when it CARRIES the approved value: the
//     RHS is that identifier, or a call taking it as an argument. Approving any
//     expression that merely mentions the name launders raw text through a map
//     lookup keyed on it.
func TestPickerLabelsAreBuiltByAnEscapingRenderer(t *testing.T) {
	fset := token.NewFileSet()
	files, err := parsePackageSources(t, fset, "")
	if err != nil {
		t.Fatal(err)
	}

	var options int
	for name, file := range files {
		for _, decl := range file.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Body != nil {
				options += checkFuncLabels(t, fset, name, fn)
				continue
			}
			// An option built in a package-level var block has no enclosing
			// function, so the per-function walk above never reaches it — and the
			// options floor stays green on the strength of the real options
			// elsewhere, leaving this one silently unexamined. There are no
			// locals out here, so the strict form with an empty approval map is
			// exactly right.
			options += checkDeclLabels(t, fset, name, decl)
		}
	}

	// Without these the guard passes just as loudly on a walk that matched
	// nothing — a green proving the walk ran, not the package clean.
	if len(files) == 0 {
		t.Fatal("parsed no non-test files in internal/cli; the walk is broken, not the package clean")
	}
	if options == 0 {
		t.Fatal("parsed files but found no huh.NewOption; the matcher is broken, not the package clean")
	}
}

// checkDeclLabels checks huh.NewOption calls in a declaration with no function
// body — a package-level var block — against an empty approval map.
func checkDeclLabels(t *testing.T, fset *token.FileSet, file string, decl ast.Decl) int {
	t.Helper()
	options := 0
	ast.Inspect(decl, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || !isHuhNewOption(call) {
			return true
		}
		options++
		if len(call.Args) == 0 || !labelIsEscaped(call.Args[0], nil) {
			t.Errorf("%s:%d: package-level huh.NewOption label is not built by an escaping renderer",
				file, fset.Position(call.Pos()).Line)
		}
		return true
	})
	return options
}

func isHuhNewOption(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "NewOption" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "huh"
}

// checkFuncLabels walks one function body in source order, tracking which locals
// hold escaped text, and reports how many huh.NewOption calls it checked.
func checkFuncLabels(t *testing.T, fset *token.FileSet, file string, fn *ast.FuncDecl) int {
	t.Helper()
	return checkBodyLabels(t, fset, file, fn.Body, map[string]bool{})
}

// checkBodyLabels walks one body in source order against its own approval map.
//
// A function literal gets a COPY of the enclosing map rather than sharing it.
// Inheriting is right — a closure legitimately uses a label the enclosing scope
// escaped — but sharing is not: a nested `label := safeTerm(x)` shadows the
// outer name, and with one shared map that inner approval leaks out and blesses
// an outer `label` still holding raw text. Copy-in gives the closure what it can
// see without letting its shadows escape.
func checkBodyLabels(t *testing.T, fset *token.FileSet, file string, body ast.Node, approved map[string]bool) int {
	t.Helper()
	options := 0

	ast.Inspect(body, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.FuncLit:
			if node == body {
				return true
			}
			inner := make(map[string]bool, len(approved))
			for name, ok := range approved {
				inner[name] = ok
			}
			options += checkBodyLabels(t, fset, file, node.Body, inner)
			return false // walked separately; do not descend with the outer map
		case *ast.AssignStmt:
			for i, rhs := range node.Rhs {
				if i >= len(node.Lhs) {
					continue
				}
				ident, ok := node.Lhs[i].(*ast.Ident)
				if !ok {
					continue
				}
				if labelIsEscaped(rhs, approved) {
					approved[ident.Name] = true
					continue
				}
				// Revoked: an assignment that replaces the escaped text rather
				// than carrying it leaves the name holding raw bytes.
				delete(approved, ident.Name)
			}
		case *ast.CallExpr:
			if !isHuhNewOption(node) {
				return true
			}
			options++
			if len(node.Args) == 0 {
				t.Errorf("%s:%d: huh.NewOption with no label argument", file, fset.Position(node.Pos()).Line)
				return true
			}
			if !labelIsEscaped(node.Args[0], approved) {
				t.Errorf("%s:%d: huh.NewOption label is not fully built by an escaping renderer; "+
					"every dynamic part must pass through one of %v",
					file, fset.Position(node.Pos()).Line, sortedRendererNames())
			}
		}
		return true
	})
	return options
}

// labelIsEscaped reports whether every dynamic part of expr passes through an
// approved renderer or an already-approved local.
//
// The rule is all-leaves, not any-leaf. A call to an approved renderer covers
// everything beneath it — that is the point of a renderer — so recursion stops
// there; anything else must have every one of its own operands covered.
// Literals and constants are inert by construction.
func labelIsEscaped(expr ast.Expr, approved map[string]bool) bool {
	switch node := expr.(type) {
	case *ast.BasicLit:
		return true
	case *ast.Ident:
		return approved[node.Name]
	case *ast.ParenExpr:
		return labelIsEscaped(node.X, approved)
	case *ast.BinaryExpr:
		return labelIsEscaped(node.X, approved) && labelIsEscaped(node.Y, approved)
	case *ast.CallExpr:
		if ident, ok := node.Fun.(*ast.Ident); ok && escapedLabelRenderers[ident.Name] {
			return true
		}
		// A wrapper (a style Render, a truncate, an fmt.Sprintf): approved only
		// when every argument it composes is itself approved. A format string is
		// a BasicLit, so it costs nothing here.
		if len(node.Args) == 0 {
			return false
		}
		for _, arg := range node.Args {
			if !labelIsEscaped(arg, approved) {
				return false
			}
		}
		return true
	default:
		// A selector, index, or anything else reaching for a field directly is
		// raw text until a renderer says otherwise.
		return false
	}
}

// parsePackageSources parses every non-test .go file in dir (default "."),
// individually rather than with parser.ParseDir, so a file excluded by the
// current platform's build tags is still read.
func parsePackageSources(t *testing.T, fset *token.FileSet, dir string) (map[string]*ast.File, error) {
	t.Helper()
	if dir == "" {
		dir = "."
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	files := map[string]*ast.File{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			return nil, err
		}
		files[name] = file
	}
	return files, nil
}

func sortedRendererNames() []string {
	names := make([]string, 0, len(escapedLabelRenderers))
	for name := range escapedLabelRenderers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
