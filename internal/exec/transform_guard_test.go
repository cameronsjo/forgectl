package exec

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
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
// on the type-checked package (TestExportedAPI,
// TestTransformIsMintedOnlyInTransformGo); this one walks every Go
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
			return nil // the typed per-GOOS guards cover this package
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

// TestTransformIsMintedOnlyInTransformGo keeps the Transform set closed: no
// production file of internal/exec but transform.go may write a Transform
// literal with elements or assign Transform.apply, for any GOOS in
// guardGOOS. The exported-API golden (TestExportedAPI) cannot see this,
// because minting inside the package changes no exported signature.
//
// Mutations that turn it red, each written in sensitive.go:
//
//   - var t Transform; t.apply = strings.ToUpper     (minting by assignment)
//   - Transform{apply: strings.ToUpper}              (minting by literal)
func TestTransformIsMintedOnlyInTransformGo(t *testing.T) {
	for _, goos := range guardGOOS {
		c := checkExecFor(t, goos)
		for _, f := range mintingFindings(t, c) {
			t.Errorf("[GOOS=%s] %s", goos, f)
		}
	}
}

// mintingFindings returns a finding for any composite literal of Transform
// with elements, or any assignment to Transform.apply, outside transform.go.
func mintingFindings(t *testing.T, c *checkedPackage) []string {
	t.Helper()
	transform, ok := c.pkg.Scope().Lookup("Transform").(*types.TypeName)
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
	var findings []string
	report := func(pos token.Pos, msg string) {
		findings = append(findings, c.fset.Position(pos).String()+": "+msg)
	}
	for _, f := range c.files {
		if filepath.Base(c.fset.Position(f.Pos()).Filename) == "transform.go" {
			continue
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.CompositeLit:
				if tv, ok := c.info.Types[node]; ok && types.Identical(tv.Type, transform.Type()) && len(node.Elts) > 0 {
					report(node.Pos(), "a Transform is minted outside transform.go; keep the closed set in one file")
				}
			case *ast.AssignStmt:
				for _, lhs := range node.Lhs {
					sel, ok := ast.Unparen(lhs).(*ast.SelectorExpr)
					if !ok {
						continue
					}
					if s, ok := c.info.Selections[sel]; ok && s.Obj() == apply {
						report(sel.Pos(), "Transform.apply is assigned outside transform.go; keep the closed set in one file")
					}
				}
			}
			return true
		})
	}
	return findings
}

// TestTmuxescIsALeaf keeps internal/tmux/tmuxesc what transform.go relies on:
// pure string escapes that hold no state and import nothing but strings, so
// the payload MapOpaque hands them can go nowhere but the returned string.
// It reads every non-test .go file in the directory, whatever its build
// constraint, so a file tagged for another GOOS is held to the same rules:
//
//   - the only import is strings;
//   - the only declarations are funcs: no package-level var (which could
//     capture a payload), const, type or init;
//   - every func is a plain, non-generic, non-method func whose parameters
//     are all string and whose single result is string or bool.
//
// Mutations that turn it red: import "os" in tmuxesc.go; declare
// `var last string` in tmuxesc.go; add `func Hook(f func(string)) string`.
func TestTmuxescIsALeaf(t *testing.T) {
	dir, err := filepath.Abs(filepath.Join("..", "tmux", "tmuxesc"))
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	funcs := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range tmuxescFindings(fset, file, &funcs) {
			t.Error(f)
		}
	}
	if funcs == 0 {
		t.Fatal("found no funcs in tmuxesc; the walk is broken, not the package clean")
	}
}

// tmuxescFindings applies TestTmuxescIsALeaf's rules to one file, counting
// the funcs it accepts into funcs.
func tmuxescFindings(fset *token.FileSet, file *ast.File, funcs *int) []string {
	var findings []string
	report := func(pos token.Pos, msg string) {
		findings = append(findings, fset.Position(pos).String()+": "+msg)
	}
	isIdent := func(e ast.Expr, names ...string) bool {
		id, ok := e.(*ast.Ident)
		return ok && slices.Contains(names, id.Name)
	}
	for _, imp := range file.Imports {
		if p, _ := strconv.Unquote(imp.Path.Value); p != "strings" {
			report(imp.Pos(), "tmuxesc imports "+strconv.Quote(p)+"; it must import only strings")
		}
	}
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.GenDecl:
			if d.Tok != token.IMPORT {
				report(d.Pos(), "tmuxesc declares a package-level "+d.Tok.String()+"; it may declare only pure string funcs, so no state can capture a payload")
			}
		case *ast.FuncDecl:
			name := d.Name.Name
			switch {
			case d.Recv != nil:
				report(d.Pos(), name+" is a method; tmuxesc may declare only plain funcs")
				continue
			case name == "init":
				report(d.Pos(), "tmuxesc declares init; it may declare only pure string funcs")
				continue
			case d.Type.TypeParams != nil:
				report(d.Pos(), name+" is generic; tmuxesc funcs take only string parameters")
				continue
			}
			ok := true
			for _, p := range d.Type.Params.List {
				if !isIdent(p.Type, "string") {
					ok = false
				}
			}
			res := d.Type.Results
			if res == nil || len(res.List) != 1 || len(res.List[0].Names) > 1 || !isIdent(res.List[0].Type, "string", "bool") {
				ok = false
			}
			if !ok {
				report(d.Pos(), name+" is not a func of strings returning one string or bool; a payload could leave tmuxesc through it")
				continue
			}
			*funcs++
		}
	}
	return findings
}
