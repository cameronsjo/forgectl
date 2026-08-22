package cli

import (
	"go/ast"
	"go/token"
	"testing"
)

const tmuxImportPath = "github.com/cameronsjo/forgectl/internal/tmux"

type tmuxTextKind struct {
	scalar     bool
	fields     map[string]bool
	collection *tmuxTextKind
}

var tmuxTextSources = map[string]tmuxTextKind{
	"ListSessions":        {collection: &tmuxTextKind{fields: map[string]bool{"Name": true, "Path": true}}},
	"ListWindows":         {collection: &tmuxTextKind{fields: map[string]bool{"Session": true, "Name": true}}},
	"ResolveSessionExact": {fields: map[string]bool{"Name": true}},
	"SeshList":            {collection: &tmuxTextKind{scalar: true}},
	// Tree applies the text boundary inside internal/tmux while composing the
	// whole rendering. Classifying it here keeps that producer-level exception
	// explicit rather than teaching the audit that arbitrary returned strings
	// are safe.
	"Tree": {},
}

var tmuxActionMethods = map[string]bool{
	"AttachSession": true,
	"AttachWindow":  true,
	"KillOthers":    true,
	"KillSession":   true,
	"LastSession":   true,
	"Pick":          true,
	"RenameSession": true,
}

type tmuxTextState struct {
	clients map[string]bool
	values  map[string]tmuxTextKind
}

func (s tmuxTextState) clone() tmuxTextState {
	copied := tmuxTextState{clients: map[string]bool{}, values: map[string]tmuxTextKind{}}
	for name, yes := range s.clients {
		copied.clients[name] = yes
	}
	for name, kind := range s.values {
		copied.values[name] = kind
	}
	return copied
}

// TestTmuxTextUsesApprovedRenderers is the text-boundary counterpart to the
// JSON emitter wiring guard. It follows results from a resolved *tmux.Client
// through ranges and local assignments, then rejects any tmux-derived text
// that reaches a fmt text construction without an approved termsafe wrapper.
//
// The import names are resolved from each file, so aliasing tmux, termsafe, or
// fmt cannot walk around the matcher. Unknown tmux client methods fail closed:
// the author must classify the method as a text source, an already-rendered
// producer, or an action before the new call site can pass.
func TestTmuxTextUsesApprovedRenderers(t *testing.T) {
	fset := token.NewFileSet()
	files, err := parsePackageSources(t, fset, "")
	if err != nil {
		t.Fatal(err)
	}

	byFile := map[string]int{}
	var parsed, sources int
	for name, file := range files {
		parsed++
		tmuxName := localNameFor(t, file, tmuxImportPath)
		if tmuxName == "" {
			continue
		}
		fmtName := localNameFor(t, file, "fmt")
		termsafeName := localNameFor(t, file, "github.com/cameronsjo/forgectl/internal/termsafe")
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			state := tmuxTextState{clients: tmuxClientParams(fn, tmuxName), values: map[string]tmuxTextKind{}}
			found := auditTmuxTextBody(t, fset, name, fn.Body, state, fmtName, termsafeName)
			byFile[name] += found
			sources += found
		}
	}

	if parsed == 0 {
		t.Fatal("parsed no non-test files in internal/cli; the walk is broken, not the package clean")
	}
	if sources == 0 {
		t.Fatalf("parsed %d files but found no tmux text source; the matcher is broken, not the package clean", parsed)
	}

	// Per-file ceilings are a one-way ratchet. An aggregate would be fungible:
	// removing one old source would silently buy room for a new unreviewed verb
	// elsewhere. A new source must be classified above and deliberately added
	// here after its renderer wiring is reviewed.
	want := map[string]int{
		"tmux_kill.go":   1,
		"tmux_ls.go":     1,
		"tmux_pick.go":   1,
		"tmux_rename.go": 1,
		"tmux_tree.go":   1,
		"tmux_window.go": 1,
	}
	for name, got := range byFile {
		if got > want[name] {
			t.Errorf("%s has %d tmux text source(s), ceiling is %d; review the new text boundary and raise only this file", name, got, want[name])
		}
	}
}

func tmuxClientParams(fn *ast.FuncDecl, tmuxName string) map[string]bool {
	clients := map[string]bool{}
	if fn.Type.Params == nil {
		return clients
	}
	for _, field := range fn.Type.Params.List {
		star, ok := field.Type.(*ast.StarExpr)
		if !ok {
			continue
		}
		if tmuxName == "." {
			ident, ok := star.X.(*ast.Ident)
			if !ok || ident.Name != "Client" {
				continue
			}
			for _, name := range field.Names {
				clients[name.Name] = true
			}
			continue
		}
		sel, ok := star.X.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Client" {
			continue
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok || pkg.Name != tmuxName {
			continue
		}
		for _, name := range field.Names {
			clients[name.Name] = true
		}
	}
	return clients
}

func auditTmuxTextBody(t *testing.T, fset *token.FileSet, file string, body ast.Node, state tmuxTextState, fmtName, termsafeName string) int {
	t.Helper()
	sources := 0
	ast.Inspect(body, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.FuncLit:
			sources += auditTmuxTextBody(t, fset, file, node.Body, state.clone(), fmtName, termsafeName)
			return false
		case *ast.AssignStmt:
			trackTmuxAssignment(node, state, termsafeName)
		case *ast.DeclStmt:
			if decl, ok := node.Decl.(*ast.GenDecl); ok {
				for _, spec := range decl.Specs {
					if value, ok := spec.(*ast.ValueSpec); ok {
						trackTmuxValueSpec(value, state, termsafeName)
					}
				}
			}
		case *ast.RangeStmt:
			if source, ok := node.X.(*ast.Ident); ok {
				if kind, found := state.values[source.Name]; found && kind.collection != nil {
					if value, ok := node.Value.(*ast.Ident); ok {
						state.values[value.Name] = *kind.collection
					}
				}
			}
		case *ast.CallExpr:
			if method, ok := tmuxClientMethod(node, state.clients); ok {
				if _, source := tmuxTextSources[method]; source {
					sources++
				} else if !tmuxActionMethods[method] {
					t.Errorf("%s: unclassified tmux.Client method %s; classify its returned text or mark it as an action", fset.Position(node.Pos()), method)
				}
			}
			if isFmtTextCall(node, fmtName) {
				for _, arg := range fmtTextArgs(node) {
					if tmuxTextIsUnsafe(arg, state, termsafeName) {
						t.Errorf("%s: tmux-derived text reaches fmt without termsafe.SafeLine or QuotePath", fset.Position(arg.Pos()))
					}
				}
			}
		}
		return true
	})
	return sources
}

func trackTmuxAssignment(assign *ast.AssignStmt, state tmuxTextState, termsafeName string) {
	for i, lhs := range assign.Lhs {
		ident, ok := lhs.(*ast.Ident)
		if !ok {
			continue
		}
		delete(state.values, ident.Name)
		if len(assign.Rhs) == 1 {
			if i > 0 {
				continue
			}
			if call, ok := assign.Rhs[0].(*ast.CallExpr); ok {
				if method, found := tmuxClientMethod(call, state.clients); found {
					if kind, source := tmuxTextSources[method]; source {
						state.values[ident.Name] = kind
					}
					continue
				}
			}
			if kind, ok := tmuxTextKindOf(assign.Rhs[0], state, termsafeName); ok {
				state.values[ident.Name] = kind
			}
			continue
		}
		if i >= len(assign.Rhs) {
			continue
		}
		if kind, ok := tmuxTextKindOf(assign.Rhs[i], state, termsafeName); ok {
			state.values[ident.Name] = kind
		}
	}
}

func trackTmuxValueSpec(spec *ast.ValueSpec, state tmuxTextState, termsafeName string) {
	for i, name := range spec.Names {
		delete(state.values, name.Name)
		if len(spec.Values) == 1 {
			if i > 0 {
				continue
			}
			if call, ok := spec.Values[0].(*ast.CallExpr); ok {
				if method, found := tmuxClientMethod(call, state.clients); found {
					if kind, source := tmuxTextSources[method]; source {
						state.values[name.Name] = kind
					}
					continue
				}
			}
			if kind, ok := tmuxTextKindOf(spec.Values[0], state, termsafeName); ok {
				state.values[name.Name] = kind
			}
			continue
		}
		if i < len(spec.Values) {
			if kind, ok := tmuxTextKindOf(spec.Values[i], state, termsafeName); ok {
				state.values[name.Name] = kind
			}
		}
	}
}

func tmuxTextKindOf(expr ast.Expr, state tmuxTextState, termsafeName string) (tmuxTextKind, bool) {
	if ident, ok := expr.(*ast.Ident); ok {
		kind, found := state.values[ident.Name]
		return kind, found
	}
	if tmuxTextIsUnsafe(expr, state, termsafeName) {
		return tmuxTextKind{scalar: true}, true
	}
	return tmuxTextKind{}, false
}

func tmuxTextIsUnsafe(expr ast.Expr, state tmuxTextState, termsafeName string) bool {
	switch node := expr.(type) {
	case *ast.Ident:
		return state.values[node.Name].scalar
	case *ast.SelectorExpr:
		ident, ok := node.X.(*ast.Ident)
		return ok && state.values[ident.Name].fields[node.Sel.Name]
	case *ast.ParenExpr:
		return tmuxTextIsUnsafe(node.X, state, termsafeName)
	case *ast.CallExpr:
		if isApprovedTextRenderer(node, termsafeName) {
			return false
		}
		for _, arg := range node.Args {
			if tmuxTextIsUnsafe(arg, state, termsafeName) {
				return true
			}
		}
	case *ast.BinaryExpr:
		return tmuxTextIsUnsafe(node.X, state, termsafeName) || tmuxTextIsUnsafe(node.Y, state, termsafeName)
	case *ast.IndexExpr:
		return tmuxTextIsUnsafe(node.X, state, termsafeName) || tmuxTextIsUnsafe(node.Index, state, termsafeName)
	case *ast.SliceExpr:
		return tmuxTextIsUnsafe(node.X, state, termsafeName)
	}
	return false
}

func isApprovedTextRenderer(call *ast.CallExpr, termsafeName string) bool {
	if termsafeName == "." {
		ident, ok := call.Fun.(*ast.Ident)
		return ok && approvedTextRendererName(ident.Name)
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok || pkg.Name != termsafeName {
		return false
	}
	return approvedTextRendererName(sel.Sel.Name)
}

func approvedTextRendererName(name string) bool {
	switch name {
	case "SafeLine", "QuotePath", "QuotePathIfUnsafe":
		return true
	default:
		return false
	}
}

func tmuxClientMethod(call *ast.CallExpr, clients map[string]bool) (string, bool) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return "", false
	}
	receiver, ok := sel.X.(*ast.Ident)
	return sel.Sel.Name, ok && clients[receiver.Name]
}

func isFmtTextCall(call *ast.CallExpr, fmtName string) bool {
	if fmtName == "" {
		return false
	}
	name := ""
	if fmtName == "." {
		if ident, ok := call.Fun.(*ast.Ident); ok {
			name = ident.Name
		}
	} else {
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return false
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok || pkg.Name != fmtName {
			return false
		}
		name = sel.Sel.Name
	}
	switch name {
	case "Errorf", "Sprint", "Sprintf", "Sprintln", "Fprint", "Fprintf", "Fprintln":
		return true
	default:
		return false
	}
}

func fmtTextArgs(call *ast.CallExpr) []ast.Expr {
	name := ""
	switch fun := call.Fun.(type) {
	case *ast.SelectorExpr:
		name = fun.Sel.Name
	case *ast.Ident:
		name = fun.Name
	}
	if name == "Fprint" || name == "Fprintf" || name == "Fprintln" {
		if len(call.Args) < 2 {
			return nil
		}
		return call.Args[1:]
	}
	return call.Args
}
