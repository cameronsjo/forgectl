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
// first open cannot block on a FIFO (forgectl#792). A new os.OpenRoot would
// reopen the hang wherever it runs outside the lifecycle lock, so this fails
// the moment os.OpenRoot is referenced anywhere but pinDirRoot, openDirRoot's
// second half: called in a function, or taken as a value (a package-level
// `var openRoot = os.OpenRoot` seam, a func-typed field, an argument). It also
// requires pinDirRoot's own call, so a scan that finds nothing cannot pass.
// The package name is read from each file's own import of "os", so an aliased
// (`import stdos "os"`) or dot import is seen too.
//
// Mutations that turn it red: revert any of the seven sites (ownerRecordLive,
// openFindingsStore, the two prune opens, the three teardown opens) to
// os.OpenRoot(c.sessionsDir) or os.OpenRoot(c.findingsDir); or add a
// package-level `var openRootSeam = os.OpenRoot` to any non-test file, or
// the same through an aliased import (`stdos "os"` and `stdos.OpenRoot`).
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
		osNames := osImportNames(f)
		if len(osNames) == 0 {
			continue
		}
		for _, d := range f.Decls {
			where := "package scope"
			fd, isFunc := d.(*ast.FuncDecl)
			if isFunc {
				where = fd.Name.Name
			}
			var visit func(ast.Node) bool
			visit = func(n ast.Node) bool {
				hit := false
				switch n := n.(type) {
				case *ast.SelectorExpr:
					// Only X can name the package; Sel is a field or method
					// name (root.OpenRoot is an os.Root method, not os.OpenRoot).
					if pkg, ok := n.X.(*ast.Ident); ok && osNames[pkg.Name] && pathRootOpeners[n.Sel.Name] {
						hit = true
					} else {
						ast.Inspect(n.X, visit)
						return false
					}
				case *ast.Ident:
					// A dot import puts OpenRoot in file scope unqualified.
					hit = osNames["."] && pathRootOpeners[n.Name]
				}
				if !hit {
					return true
				}
				if isFunc && fd.Name.Name == "pinDirRoot" && fd.Recv == nil {
					inPin++
					return false
				}
				t.Errorf("%s: os.OpenRoot referenced in %s; open the directory with openDirRoot so a FIFO at the path cannot block it",
					fset.Position(n.Pos()), where)
				return false
			}
			ast.Inspect(d, visit)
		}
	}
	if inPin != 1 {
		t.Fatalf("found %d os.OpenRoot references in pinDirRoot, want 1; the check cannot see the open it guards", inPin)
	}
}

// Every store child this package opens as an os.Root goes through
// openChildDirRoot, whose first open cannot block on a FIFO (forgectl#798).
// It fails the moment a Root.OpenRoot method is referenced anywhere but
// openChildDirRoot, and requires openChildDirRoot's own call, so a scan that
// finds nothing cannot pass. A selector whose X names package "os" is
// os.OpenRoot, which TestOpenDirRoot_IsTheOnlyPathRootOpener owns.
//
// Mutations that turn it red: revert openFindingsChild or findingsChildSize
// to store.OpenRoot(name).
func TestOpenChildDirRoot_IsTheOnlyChildRootOpener(t *testing.T) {
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	inChild := 0
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
		osNames := osImportNames(f)
		for _, d := range f.Decls {
			where := "package scope"
			fd, isFunc := d.(*ast.FuncDecl)
			if isFunc {
				where = fd.Name.Name
			}
			ast.Inspect(d, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "OpenRoot" {
					return true
				}
				if pkg, ok := sel.X.(*ast.Ident); ok && osNames[pkg.Name] {
					return true
				}
				if isFunc && fd.Name.Name == "openChildDirRoot" && fd.Recv == nil {
					inChild++
					return true
				}
				t.Errorf("%s: Root.OpenRoot referenced in %s; open the child with openChildDirRoot so a FIFO at the name cannot block it",
					fset.Position(sel.Pos()), where)
				return true
			})
		}
	}
	if inChild != 1 {
		t.Fatalf("found %d Root.OpenRoot references in openChildDirRoot, want 1; the check cannot see the open it guards", inChild)
	}
}

// pathRootOpeners are the os functions that open a directory by path as an
// os.Root: OpenInRoot calls OpenRoot on its dir argument, so it blocks on a
// FIFO the same way.
var pathRootOpeners = map[string]bool{"OpenRoot": true, "OpenInRoot": true}

// osImportNames is every name file f refers to package "os" by: "os", an
// alias, or "." for a dot import. A file may import "os" more than once under
// different names, so all of them count. A blank import names nothing.
func osImportNames(f *ast.File) map[string]bool {
	names := map[string]bool{}
	for _, imp := range f.Imports {
		if imp.Path.Value != `"os"` {
			continue
		}
		switch {
		case imp.Name == nil:
			names["os"] = true
		case imp.Name.Name != "_":
			names[imp.Name.Name] = true
		}
	}
	return names
}
