package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// homeDiscardAllowlist names the production files that may discard
// os.UserHomeDir's error, each with the reason. It is empty on purpose: every
// site that looks the home directory up either returns the error, refuses on
// it, or (internal/docs/vault.go) keeps `err` and answers "" for a boundary
// that has a documented no-boundary meaning. A new entry needs a reason a
// reviewer can argue with.
var homeDiscardAllowlist = map[string]string{}

// homeDiscards returns one "path:line" finding per call to os.UserHomeDir
// whose error is thrown away: a blank second assignee (`h, _ := ...`), or a
// call used as a bare statement. A failed lookup that becomes "" and then a
// cwd-relative path is the bug class of #954/#965/#971/#972.
func homeDiscards(fset *token.FileSet, file *ast.File) []string {
	osNames := importAliases(file, "os")
	if len(osNames) == 0 {
		return nil
	}
	isHomeCall := func(e ast.Expr) bool {
		call, ok := e.(*ast.CallExpr)
		if !ok {
			return false
		}
		if id, ok := call.Fun.(*ast.Ident); ok {
			return osNames["."] && id.Name == "UserHomeDir" // dot-import of os
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "UserHomeDir" {
			return false
		}
		id, ok := sel.X.(*ast.Ident)
		return ok && osNames[id.Name]
	}
	var out []string
	ast.Inspect(file, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.AssignStmt:
			if len(v.Rhs) == 1 && len(v.Lhs) == 2 && isHomeCall(v.Rhs[0]) {
				if id, ok := v.Lhs[1].(*ast.Ident); ok && id.Name == "_" {
					out = append(out, fset.Position(v.Pos()).String())
				}
			}
		case *ast.ValueSpec:
			if len(v.Values) == 1 && len(v.Names) == 2 && isHomeCall(v.Values[0]) && v.Names[1].Name == "_" {
				out = append(out, fset.Position(v.Pos()).String())
			}
		case *ast.ExprStmt:
			if isHomeCall(v.X) {
				out = append(out, fset.Position(v.Pos()).String())
			}
		}
		return true
	})
	return out
}

// TestNoDiscardedUserHomeDirError pins forgectl#972: production code never
// discards os.UserHomeDir's error.
//
// Mutation that turns it red: change any production
// `home, err := os.UserHomeDir()` to `home, _ := os.UserHomeDir()`.
func TestNoDiscardedUserHomeDirError(t *testing.T) {
	fset := token.NewFileSet()
	var paths []string
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != "." && (strings.HasPrefix(d.Name(), ".") || d.Name() == "testdata") {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") {
			paths = append(paths, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatal("scanned no production files")
	}
	for _, p := range paths {
		if _, ok := homeDiscardAllowlist[filepath.ToSlash(p)]; ok {
			continue
		}
		file, err := parser.ParseFile(fset, p, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", p, err)
		}
		for _, f := range homeDiscards(fset, file) {
			t.Errorf("%s discards os.UserHomeDir's error; return it, refuse on it, or inject the lookup (see config.resolveDir)", f)
		}
	}
}

func TestHomeDiscardsMatcher(t *testing.T) {
	for _, tt := range []struct {
		name, src string
		want      int
	}{
		{"blank error", `package p; import "os"; func f() { h, _ := os.UserHomeDir(); _ = h }`, 1},
		{"aliased os", `package p; import o "os"; func f() { h, _ := o.UserHomeDir(); _ = h }`, 1},
		{"bare statement", `package p; import "os"; func f() { os.UserHomeDir() }`, 1},
		{"var spec blank error", `package p; import "os"; var h, _ = os.UserHomeDir()`, 1},
		{"dot import", `package p; import . "os"; func f() { h, _ := UserHomeDir(); _ = h }`, 1},
		{"dot import kept", `package p; import . "os"; func f() { h, err := UserHomeDir(); _, _ = h, err }`, 0},
		{"error kept", `package p; import "os"; func f() { h, err := os.UserHomeDir(); _, _ = h, err }`, 0},
		{"function value", `package p; import "os"; var g = os.UserHomeDir`, 0},
		{"other package", `package p; import "x"; func f() { h, _ := x.UserHomeDir(); _ = h }`, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "x.go", tt.src, 0)
			if err != nil {
				t.Fatal(err)
			}
			if got := homeDiscards(fset, file); len(got) != tt.want {
				t.Fatalf("%s: got %d findings %v, want %d", tt.name, len(got), got, tt.want)
			}
		})
	}
}
