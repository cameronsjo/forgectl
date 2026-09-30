package exec

import (
	"go/ast"
	"go/build"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const execImportPath = "github.com/cameronsjo/forgectl/internal/exec"

// transformSeam is every exported name through which a caller can ask the
// seam to re-spell a sealed payload.
var transformSeam = map[string]bool{
	"MapOpaque":      true,
	"Transform":      true,
	"TmuxDirOperand": true,
}

// transformCallers is the allowlist of files outside internal/exec that may
// name the transform seam, relative to the module root. It is one file: the
// tmux adapter's create command, which escapes its sealed -c (forgectl#839).
var transformCallers = map[string]bool{
	"internal/surface/tmuxadapter/start.go": true,
}

// TestNoCallerCodeReceivesAnOpaquePayload keeps the sealed-payload promise
// that buildCmd, FakeSensitiveRunner and backend.BootstrapCommand state: no
// payload is handed to code outside this package. The in-package rules run
// on the type-checked package (checkExecPackage); this one walks every Go
// file in the module, tests included, so a test elsewhere cannot use the
// seam either. Outside internal/exec, only transformCallers may name
// MapOpaque, Transform or TmuxDirOperand, and no file may dot-import this
// package, because its references could not be resolved.
//
// Mutation that turns it red: call exec.MapOpaque from any other package's
// file.
func TestNoCallerCodeReceivesAnOpaquePayload(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("module root: %v", err)
	}
	fset := token.NewFileSet()
	parsed, sanctioned := 0, 0
	var findings []string
	report := func(pos token.Pos, msg string) {
		findings = append(findings, fset.Position(pos).String()+": "+msg)
	}
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			name := entry.Name()
			if path != root && (strings.HasPrefix(name, ".") || name == "vendor" || name == "testdata") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(entry.Name(), ".go") {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		if filepath.ToSlash(filepath.Dir(rel)) == "internal/exec" {
			return nil // checkExecPackage covers this package, typed
		}
		file, parseErr := parser.ParseFile(fset, path, nil, parser.ImportsOnly|parser.SkipObjectResolution)
		if parseErr != nil {
			return parseErr
		}
		parsed++
		alias := execAlias(file, report)
		if alias == "" {
			return nil
		}
		// Only now is the whole file worth parsing.
		file, parseErr = parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if parseErr != nil {
			return parseErr
		}
		ast.Inspect(file, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if id, ok := sel.X.(*ast.Ident); !ok || id.Name != alias || !transformSeam[sel.Sel.Name] {
				return true
			}
			if transformCallers[rel] {
				sanctioned++
				return true
			}
			report(sel.Pos(), "exec."+sel.Sel.Name+" used outside the transform allowlist; a new caller must be reviewed and added to transformCallers")
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range findings {
		t.Error(f)
	}
	if parsed == 0 {
		t.Fatal("parsed no Go files; the walk is broken, not the module clean")
	}
	if sanctioned == 0 {
		t.Fatal("found no use of the transform seam at its sanctioned site; the matcher is broken, not the module clean")
	}
}

// execAlias returns the name file uses for this package, or "" when it does
// not import it (or imports it blank). A dot-import is reported.
func execAlias(file *ast.File, report func(token.Pos, string)) string {
	for _, imp := range file.Imports {
		if p, _ := strconv.Unquote(imp.Path.Value); p != execImportPath {
			continue
		}
		if imp.Name == nil {
			return "exec"
		}
		switch imp.Name.Name {
		case ".":
			report(imp.Pos(), "dot-import of internal/exec hides references to the transform seam; use a named import")
			return ""
		case "_":
			return ""
		}
		return imp.Name.Name
	}
	return ""
}

// TestExecHandsNoPayloadToCallerCode type-checks internal/exec's production
// files and applies the two in-package rules (checkExecPackage).
//
// Mutations that turn it red, one per shape the rules must catch:
//
//   - func A(a Arg, f func(string) string) Arg      (a plain func parameter)
//   - type R func(string) string; func A(a Arg, r R) (a named func type)
//   - func A[F ~func(string) string](a Arg, f F)     (a type parameter)
//   - func A(a Arg, fs ...func(string) string)       (a variadic func)
//   - type r struct{}; func (r) M(f func(string))    (a method on an unexported type)
//   - var t Transform; t.apply = strings.ToUpper     (minting by assignment)
//   - Transform{apply: strings.ToUpper}              (minting by literal)
//
// each written in sensitive.go.
func TestExecHandsNoPayloadToCallerCode(t *testing.T) {
	for _, f := range checkExecPackage(t) {
		t.Error(f)
	}
}

// checkExecPackage returns a finding for:
//
//   - any exported function, or exported method on ANY type (an unexported
//     type's method is still callable through a value an importer holds),
//     with a parameter whose type is or contains a func signature: a plain,
//     named, variadic or pointer-to func, a func inside a slice, map, array,
//     channel or exported struct field, or a type parameter whose constraint
//     admits one. A callback is how a payload would reach caller code, as the
//     first MapOpaque did with `f func(string) string`;
//   - any composite literal of Transform with elements, or any assignment to
//     Transform.apply, outside transform.go, so the closed set is minted in
//     one reviewable file.
func checkExecPackage(t *testing.T) []string {
	t.Helper()
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	pkg, err := build.Default.ImportDir(dir, 0)
	if err != nil {
		t.Fatalf("list internal/exec: %v", err)
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, name := range pkg.GoFiles {
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	if len(files) == 0 {
		t.Fatal("parsed no production files of internal/exec")
	}
	info := &types.Info{
		Types:      map[ast.Expr]types.TypeAndValue{},
		Selections: map[*ast.SelectorExpr]*types.Selection{},
	}
	conf := types.Config{Importer: importer.ForCompiler(fset, "source", nil)}
	tpkg, err := conf.Check(execImportPath, fset, files, info)
	if err != nil {
		t.Fatalf("type-check internal/exec: %v", err)
	}

	var findings []string
	report := func(pos token.Pos, msg string) {
		findings = append(findings, fset.Position(pos).String()+": "+msg)
	}

	checkSig := func(fn *types.Func) {
		if !fn.Exported() {
			return
		}
		sig, ok := fn.Type().(*types.Signature)
		if !ok {
			return
		}
		params := sig.Params()
		for i := 0; i < params.Len(); i++ {
			if containsSignature(params.At(i).Type(), map[types.Type]bool{}) {
				report(fn.Pos(), fn.Name()+" takes a parameter that is or holds a func; a callback can receive an opaque payload, so the seam offers closed Transforms instead")
			}
		}
	}
	scope := tpkg.Scope()
	functions, methods := 0, 0
	for _, name := range scope.Names() {
		switch obj := scope.Lookup(name).(type) {
		case *types.Func:
			functions++
			checkSig(obj)
		case *types.TypeName:
			named, ok := obj.Type().(*types.Named)
			if !ok {
				continue
			}
			for i := 0; i < named.NumMethods(); i++ {
				methods++
				checkSig(named.Method(i))
			}
		}
	}
	if functions == 0 || methods == 0 {
		t.Fatalf("type-checked internal/exec but saw %d functions and %d methods; the walk is broken", functions, methods)
	}

	transform, ok := scope.Lookup("Transform").(*types.TypeName)
	if !ok {
		t.Fatal("internal/exec declares no Transform type")
	}
	var apply *types.Var
	if st, ok := transform.Type().Underlying().(*types.Struct); ok {
		for i := 0; i < st.NumFields(); i++ {
			if st.Field(i).Name() == "apply" {
				apply = st.Field(i)
			}
		}
	}
	if apply == nil {
		t.Fatal("Transform has no apply field; the minting rule would check nothing")
	}
	for _, f := range files {
		if filepath.Base(fset.Position(f.Pos()).Filename) == "transform.go" {
			continue
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.CompositeLit:
				if tv, ok := info.Types[node]; ok && types.Identical(tv.Type, transform.Type()) && len(node.Elts) > 0 {
					report(node.Pos(), "a Transform is minted outside transform.go; keep the closed set in one file")
				}
			case *ast.AssignStmt:
				for _, lhs := range node.Lhs {
					sel, ok := ast.Unparen(lhs).(*ast.SelectorExpr)
					if !ok {
						continue
					}
					if s, ok := info.Selections[sel]; ok && s.Obj() == apply {
						report(sel.Pos(), "Transform.apply is assigned outside transform.go; keep the closed set in one file")
					}
				}
			}
			return true
		})
	}
	return findings
}

// containsSignature reports whether t is, or holds, a func signature that a
// caller could supply: through its underlying type, an element, an exported
// struct field, or a type parameter's constraint. seen stops recursion.
func containsSignature(t types.Type, seen map[types.Type]bool) bool {
	if seen[t] {
		return false
	}
	seen[t] = true
	if tp, ok := t.(*types.TypeParam); ok {
		return constraintAdmitsSignature(tp.Constraint(), seen)
	}
	switch u := t.Underlying().(type) {
	case *types.Signature:
		return true
	case *types.Pointer:
		return containsSignature(u.Elem(), seen)
	case *types.Slice:
		return containsSignature(u.Elem(), seen)
	case *types.Array:
		return containsSignature(u.Elem(), seen)
	case *types.Chan:
		return containsSignature(u.Elem(), seen)
	case *types.Map:
		return containsSignature(u.Key(), seen) || containsSignature(u.Elem(), seen)
	case *types.Struct:
		// An unexported field cannot be set by a caller, so a closure the
		// package itself stores there (Arg.reveal) is not a callback.
		for i := 0; i < u.NumFields(); i++ {
			if f := u.Field(i); f.Exported() && containsSignature(f.Type(), seen) {
				return true
			}
		}
	case *types.Interface:
		return constraintAdmitsSignature(u, seen)
	}
	return false
}

// constraintAdmitsSignature reports whether an interface used as a
// constraint embeds, directly or through a union, a type holding a func.
func constraintAdmitsSignature(c types.Type, seen map[types.Type]bool) bool {
	iface, ok := c.Underlying().(*types.Interface)
	if !ok {
		return false
	}
	for i := 0; i < iface.NumEmbeddeds(); i++ {
		switch e := iface.EmbeddedType(i).(type) {
		case *types.Union:
			for j := 0; j < e.Len(); j++ {
				if containsSignature(e.Term(j).Type(), seen) {
					return true
				}
			}
		default:
			if containsSignature(e, seen) {
				return true
			}
		}
	}
	return false
}

// TestTmuxescIsALeaf keeps internal/tmux/tmuxesc what transform.go relies on:
// pure string escapes that import nothing but strings, so the payload
// MapOpaque hands them can go nowhere else.
//
// Mutation that turns it red: import "os" (or anything but strings) in
// tmuxesc.go.
func TestTmuxescIsALeaf(t *testing.T) {
	dir, err := filepath.Abs(filepath.Join("..", "tmux", "tmuxesc"))
	if err != nil {
		t.Fatal(err)
	}
	pkg, err := build.Default.ImportDir(dir, 0)
	if err != nil {
		t.Fatalf("list tmuxesc: %v", err)
	}
	if len(pkg.GoFiles) == 0 {
		t.Fatal("tmuxesc has no production files; the check is broken")
	}
	for _, imp := range pkg.Imports {
		if imp != "strings" {
			t.Errorf("tmuxesc imports %q; it must import only strings", imp)
		}
	}
}
