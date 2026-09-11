package cli

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestStyledPrintsGoThroughColorOut is the structural guard behind colorOut.
//
// It exists because per-call-site wrapping already failed, twice, in the very
// change that introduced colorOut: `ghostty cheat` and `tmux cheat` printed
// internal/tui's styled helpers to raw stdout (117 and 23 escape sequences
// piped and under NO_COLOR), and `bench status` reached the same leak through a
// helper. Lip Gloss v2 emits colour from Style.Render unconditionally — it
// moved the downgrade into the writer — so an unwrapped writer is a silent
// NO_COLOR and pipe regression with no compile error. ADR-0008 rule 5 states
// the requirement; this test is what enforces it.
//
// The rule: inside package cli, a raw cmd.OutOrStdout() must not receive styled
// text — not directly through fmt.Fprint*, not through a local variable, and
// not by being handed to a helper that styles. Wrap it in
// deps.Theme.Writer(cmd.OutOrStdout(), os.Environ()) — colorOut(cmd) is the
// legacy equivalent, kept only for the two call sites this migration could
// not reach (see colorout.go's doc comment).
//
// Plain text through a raw writer is untouched, and so is JSON: those are
// correct and are what most of this package does.
//
// WHAT IT CANNOT SEE, stated because an earlier version of this comment claimed
// coverage it did not have, and that overclaim is the thing most likely to let
// the next leak through. It is syntactic and intraprocedural, so it misses:
//
//   - a writer stored in a struct field or map (`s.out = cmd.OutOrStdout()`),
//     since bindings are tracked by identifier name only;
//   - a writer returned from a helper (`w := writerFor(cmd)`), since there is
//     no interprocedural return tracking;
//   - a name shadowed in an inner scope, which clears the outer binding for the
//     rest of the function — the miss direction, not the false-positive one;
//   - `io.WriteString(out, styled)` and `out.Write(...)`, which are not
//     fmt.Fprint* and carry no styling this can name;
//   - a hand-written "\x1b[" format string, which has no .Render and no styled
//     var to match — that one is caught by the root theme-literal test instead.
//
// The behavioural table in PR 2b (running each plain-output verb with stdout
// piped and asserting no escape) is what covers those; this guard is the fast
// structural half.
func TestStyledPrintsGoThroughColorOut(t *testing.T) {
	fset := token.NewFileSet()
	// SA1019: ParseDir is deprecated because it ignores build tags when
	// grouping files into packages. That does not matter here — this reads one
	// directory whose files all belong to package cli, and the alternative
	// (golang.org/x/tools/go/packages) would add a dependency to the module for
	// a guard that needs syntax only, no type information.
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool { //nolint:staticcheck
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse package cli: %v", err)
	}
	files := map[string]*ast.File{}
	for _, pkg := range pkgs {
		for name, f := range pkg.Files {
			files[name] = f
		}
	}
	if len(files) == 0 {
		t.Fatal("parsed no files; the guard would pass over nothing")
	}

	styledVars := findStyledVars(files)
	styling := findStylingFuncs(files, styledVars)
	if len(styling) == 0 {
		t.Fatal("found no styling functions in package cli; the guard matched nothing and could not have failed")
	}

	sites := 0
	for name, file := range files {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			// Bindings are tracked as the walk proceeds, in source order, so a
			// later `out = colorOut(cmd)` cannot retroactively clear an earlier
			// styled write through `out := cmd.OutOrStdout()`.
			raw := map[string]bool{}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				noteWriterBinding(n, raw)
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				// EVERY argument position, not just the first. k8s.go passes
				// the writer third (`client.Logs(ctx, in, out, …)`), so an
				// Args[0]-only check is blind to a whole file.
				if !slices.ContainsFunc(call.Args, func(a ast.Expr) bool { return isRawWriter(a, raw) }) {
					return true
				}
				sites++
				what := ""
				switch {
				case isFprint(call.Fun):
					for _, arg := range call.Args {
						if w := styledExpr(arg, styledVars, styling); w != "" {
							what = w
						}
					}
				default:
					// Both `render(w)` and `tui.Render(w)` / `h.render(w)`. The
					// selector form was invisible before, which was the most
					// likely miss: styledExpr already treats a tui.* call as
					// styling when it appears as an argument, so the package's
					// own model said such a callee styles.
					what = styledCallee(call.Fun, styling)
				}
				if what != "" {
					pos := fset.Position(call.Pos())
					t.Errorf("%s:%d: styled output (%s) written to a raw cmd.OutOrStdout(); wrap it in deps.Theme.Writer(cmd.OutOrStdout(), os.Environ()) or it emits raw ANSI into pipes and under NO_COLOR (ADR-0008 rule 5)",
						filepath.Base(name), pos.Line, what)
				}
				return true
			})
		}
	}
	if sites == 0 {
		t.Fatal("found no call taking a raw cmd.OutOrStdout(); the guard matched nothing and could not have failed")
	}
}

// findStyledVars collects package-level vars whose initializer renders a style —
// launchOKMark and friends are pre-rendered at package scope, so a function that
// merely returns one contains no .Render( call of its own.
func findStyledVars(files map[string]*ast.File) map[string]bool {
	out := map[string]bool{}
	for _, file := range files {
		for _, decl := range file.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.VAR {
				continue
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, val := range vs.Values {
					if i < len(vs.Names) && hasRenderCall(val) {
						out[vs.Names[i].Name] = true
					}
				}
			}
		}
	}
	return out
}

// findStylingFuncs returns package-level functions that produce styled text,
// closed transitively: a function styles if it renders, names a styled var,
// calls into internal/tui, or calls another styling function.
func findStylingFuncs(files map[string]*ast.File, styledVars map[string]bool) map[string]bool {
	styling := map[string]bool{}
	for changed := true; changed; {
		changed = false
		for _, file := range files {
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil || styling[fn.Name.Name] {
					continue
				}
				if styledExpr(fn.Body, styledVars, styling) != "" {
					styling[fn.Name.Name] = true
					changed = true
				}
			}
		}
	}
	return styling
}

// styledExpr describes the styling inside n, or "" if there is none.
func styledExpr(n ast.Node, styledVars, styling map[string]bool) string {
	found := ""
	ast.Inspect(n, func(n ast.Node) bool {
		if found != "" {
			return false
		}
		switch e := n.(type) {
		case *ast.Ident:
			if styledVars[e.Name] {
				found = "the pre-rendered " + e.Name
			}
		case *ast.CallExpr:
			if id, ok := e.Fun.(*ast.Ident); ok && styling[id.Name] {
				found = id.Name + "()"
				return false
			}
			sel, ok := e.Fun.(*ast.SelectorExpr)
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
		}
		return true
	})
	return found
}

// hasRenderCall reports whether n contains a .Render( call.
func hasRenderCall(n ast.Node) bool {
	found := false
	ast.Inspect(n, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Render" {
				found = true
				return false
			}
		}
		return true
	})
	return found
}

// styledCallee describes the styling a callee performs, or "" if it does none.
// It covers a bare identifier (`renderBenchReport(w, …)`), a package-qualified
// call into internal/tui (`tui.KeybindSheet(w, …)`), and a method whose name is
// known to style (`h.renderReport(w)`).
func styledCallee(fun ast.Expr, styling map[string]bool) string {
	switch f := fun.(type) {
	case *ast.Ident:
		if styling[f.Name] {
			return f.Name + " styles its output"
		}
	case *ast.SelectorExpr:
		if pkg, ok := f.X.(*ast.Ident); ok && pkg.Name == "tui" {
			return "tui." + f.Sel.Name + " styles its output"
		}
		if styling[f.Sel.Name] {
			return f.Sel.Name + " styles its output"
		}
	}
	return ""
}

// noteWriterBinding updates raw when n binds a name to a writer — through
// either `out := cmd.OutOrStdout()` or `var out = cmd.OutOrStdout()`.
//
// It is called during the walk rather than in a prepass on purpose. A prepass
// computes one final set, so a function that rebinds the name later
// (`out = colorOut(cmd)`) would clear it for the whole body and hide an earlier
// styled write through the raw writer.
func noteWriterBinding(n ast.Node, raw map[string]bool) {
	switch d := n.(type) {
	case *ast.AssignStmt:
		bindWriters(d.Lhs, d.Rhs, raw)
	case *ast.ValueSpec: // var out = cmd.OutOrStdout()
		lhs := make([]ast.Expr, len(d.Names))
		for i, name := range d.Names {
			lhs[i] = name
		}
		bindWriters(lhs, d.Values, raw)
	}
}

// bindWriters pairs lhs names with rhs expressions, marking a name raw when it
// takes a bare cmd.OutOrStdout() and clearing it when it takes colorOut(...).
func bindWriters(lhs, rhs []ast.Expr, raw map[string]bool) {
	for i, expr := range rhs {
		if i >= len(lhs) {
			break
		}
		id, ok := lhs[i].(*ast.Ident)
		if !ok {
			continue
		}
		switch {
		case isOutOrStdoutCall(expr):
			raw[id.Name] = true
		case isSafeWriterCall(expr):
			delete(raw, id.Name)
		}
	}
}

// isRawWriter reports whether e is a raw cmd.OutOrStdout() — spelled inline, or
// via a local bound to one.
func isRawWriter(e ast.Expr, raw map[string]bool) bool {
	if isOutOrStdoutCall(e) {
		return true
	}
	id, ok := e.(*ast.Ident)
	return ok && raw[id.Name]
}

func isOutOrStdoutCall(e ast.Expr) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "OutOrStdout"
}

// isSafeWriterCall reports whether e is a known theme-aware wrapper around a
// raw writer — the legacy colorOut(cmd) shim, or any receiver's Writer(...)
// method (th.Writer(w, env), deps.Theme.Writer(w, env)) — theme.Theme's
// colorprofile-backed replacement. Both downgrade styled output to the
// destination's colour profile before anything styled reaches it, so binding
// a name to either result clears that name's raw status.
//
// The "any receiver" breadth on the selector form is a known widening: it
// cannot distinguish theme.Theme.Writer from an unrelated same-named method
// elsewhere in the package. Acceptable here because the guard is already
// documented as a heuristic (see the miss list above), and package cli has no
// other type exposing a two-arg Writer method today.
func isSafeWriterCall(e ast.Expr) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		return fun.Name == "colorOut"
	case *ast.SelectorExpr:
		return fun.Sel.Name == "Writer"
	}
	return false
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
