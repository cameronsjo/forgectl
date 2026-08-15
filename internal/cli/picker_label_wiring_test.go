package cli

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// escapedLabelRenderers are the functions permitted to produce a huh option
// label. Each applies a terminal boundary to untrusted text before it reaches
// the picker.
//
// sanitizeCell is in the set under protest: it maps only C0 and DEL, so C1 and
// bidi reach the terminal through a PR title. It is weaker than everything else
// here and is tracked in #324; listing it keeps this guard honest about what it
// currently permits instead of quietly passing a call it does not really
// approve of.
var escapedLabelRenderers = map[string]bool{
	"repoPickerLabel": true,
	"safeTerm":        true,
	"safeCandidate":   true,
	"sanitizeCell":    true,
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
// So this asserts the wiring itself. Every huh.NewOption in this package must
// take a label that is either a call to an approved renderer or a local
// variable assigned from one; the option's second argument, the selection key,
// is deliberately unconstrained because it is never printed.
func TestPickerLabelsAreBuiltByAnEscapingRenderer(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read internal/cli: %v", err)
	}

	fset := token.NewFileSet()
	var parsed, options int
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		parsed++

		// Labels are frequently built a line or two above the option, and then
		// re-wrapped (a dim style on a live session). So track locals: a name
		// becomes approved when assigned from an approved renderer, STAYS
		// approved through a re-wrap that carries it along, and is REVOKED by an
		// assignment that does neither. Without the revocation the guard would
		// bless a label that was escaped once and then overwritten with raw text.
		escapedLocals := map[string]bool{}
		ast.Inspect(file, func(n ast.Node) bool {
			assign, ok := n.(*ast.AssignStmt)
			if !ok {
				return true
			}
			for i, rhs := range assign.Rhs {
				if i >= len(assign.Lhs) {
					continue
				}
				ident, ok := assign.Lhs[i].(*ast.Ident)
				if !ok {
					continue
				}
				if callsEscapingRenderer(rhs) || referencesApproved(rhs, escapedLocals) {
					escapedLocals[ident.Name] = true
					continue
				}
				delete(escapedLocals, ident.Name)
			}
			return true
		})

		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "NewOption" {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || pkg.Name != "huh" {
				return true
			}
			options++
			if len(call.Args) == 0 {
				t.Errorf("%s: huh.NewOption with no label argument", fset.Position(call.Pos()))
				return true
			}
			label := call.Args[0]
			if callsEscapingRenderer(label) {
				return true
			}
			if ident, ok := label.(*ast.Ident); ok && escapedLocals[ident.Name] {
				return true
			}
			t.Errorf("%s: huh.NewOption label is not built by an escaping renderer; use one of %v",
				fset.Position(call.Pos()), sortedRendererNames())
			return true
		})
	}

	// Without these the guard passes just as loudly on a walk that matched
	// nothing — a green proving the walk ran, not the package clean.
	if parsed == 0 {
		t.Fatal("parsed no non-test files in internal/cli; the walk is broken, not the package clean")
	}
	if options == 0 {
		t.Fatal("parsed files but found no huh.NewOption; the matcher is broken, not the package clean")
	}
}

// callsEscapingRenderer reports whether expr is a call to an approved renderer,
// including one wrapped in further calls (a truncate or a pad around it).
func callsEscapingRenderer(expr ast.Expr) bool {
	call, ok := expr.(*ast.CallExpr)
	if !ok {
		return false
	}
	if ident, ok := call.Fun.(*ast.Ident); ok && escapedLabelRenderers[ident.Name] {
		return true
	}
	for _, arg := range call.Args {
		if callsEscapingRenderer(arg) {
			return true
		}
	}
	return false
}

// referencesApproved reports whether expr carries an already-approved local
// through — the `label = style.Render(label)` shape, where the escaped text is
// re-wrapped rather than replaced.
func referencesApproved(expr ast.Expr, approved map[string]bool) bool {
	found := false
	ast.Inspect(expr, func(n ast.Node) bool {
		if ident, ok := n.(*ast.Ident); ok && approved[ident.Name] {
			found = true
		}
		return !found
	})
	return found
}

func sortedRendererNames() []string {
	names := make([]string, 0, len(escapedLabelRenderers))
	for name := range escapedLabelRenderers {
		names = append(names, name)
	}
	return names
}
