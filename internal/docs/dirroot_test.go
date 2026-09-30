package docs

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
// first open cannot block on a FIFO (forgectl#798). A new os.OpenRoot on a
// root path would reopen the hang, so this fails the moment os.OpenRoot is
// referenced anywhere but pinDirRoot, openDirRoot's second half: called in a function, or taken as a value (a package-level
// `var openRoot = os.OpenRoot` seam, a func-typed field, an argument). It also
// requires pinDirRoot's own call, so a scan that finds nothing cannot pass.
// The package name is read from each file's own import of "os", so an aliased
// (`import stdos "os"`) or dot import is seen too.
//
// Mutations that turn it red: revert any of the three sites (ResolveInRoot,
// openRootDir, openPinnedRoot) to os.OpenRoot(<path>); or add a
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
		f, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		osName := osImportName(f)
		if osName == "" {
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
					if pkg, ok := n.X.(*ast.Ident); ok && isPackageOS(pkg, osName) && n.Sel.Name == "OpenRoot" {
						hit = true
					} else {
						ast.Inspect(n.X, visit)
						return false
					}
				case *ast.Ident:
					// A dot import puts OpenRoot in file scope unqualified.
					hit = osName == "." && n.Name == "OpenRoot"
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

// Every directory this package opens as an os.Root below another goes
// through openChildDirRoot, whose first open cannot block on a FIFO
// (forgectl#798). It fails the moment a Root.OpenRoot method is referenced
// anywhere but openChildDirRoot, and requires openChildDirRoot's own call,
// so a scan that finds nothing cannot pass. A selector whose X names
// package "os" is os.OpenRoot, which TestOpenDirRoot_IsTheOnlyPathRootOpener
// owns.
//
// Mutations that turn it red: revert openDirVerified or openHeldSubdir to
// <parent>.OpenRoot(name); or add a non-test file with `os := root` shadowing
// the import and calling os.OpenRoot (the parse resolves objects, so the
// shadowing local is not mistaken for the package).
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
		f, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		osName := osImportName(f)
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
				if pkg, ok := sel.X.(*ast.Ident); ok && osName != "" && isPackageOS(pkg, osName) {
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

// isPackageOS reports whether id names the imported package "os" rather than
// a local that shadows its name. The parse runs with object resolution, which
// binds an identifier declared in a function or block scope to its declaration
// (id.Obj != nil) and leaves a file-scope import unbound (id.Obj == nil).
// Without it, `os := root; os.OpenRoot(...)` would be read as the package and
// skip the Root.OpenRoot check.
func isPackageOS(id *ast.Ident, osName string) bool {
	return id.Name == osName && id.Obj == nil //nolint:staticcheck // SA1019: ast.Object is the only scope signal without go/types
}

// osImportName is the name file f refers to package "os" by: "os", an alias,
// "." for a dot import, or "" when f does not import it (or imports it only
// for side effects).
func osImportName(f *ast.File) string {
	for _, imp := range f.Imports {
		if imp.Path.Value != `"os"` {
			continue
		}
		if imp.Name == nil {
			return "os"
		}
		if imp.Name.Name == "_" {
			return ""
		}
		return imp.Name.Name
	}
	return ""
}
