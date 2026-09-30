package exec

import (
	"go/ast"
	"go/parser"
	"go/token"
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
// payload is handed to code outside this package. Three rules, over every Go
// file in the module (tests included, so a test elsewhere cannot use the
// seam either):
//
//   - Outside internal/exec, only transformCallers may name MapOpaque,
//     Transform or TmuxDirOperand, and no file may dot-import this package
//     (its references could not be resolved).
//   - Inside internal/exec's production files, a Transform is minted only in
//     transform.go, so the closed set is closed in one reviewable place.
//   - No exported function or method of this package takes a parameter of
//     func type. A callback is how a payload would reach caller code, as the
//     first MapOpaque did with `f func(string) string`.
//
// Mutations that turn it red: call exec.MapOpaque from any other package's
// file; add `func MapOpaqueWith(a Arg, f func(string) string) Arg` to this
// package; write `Transform{apply: ...}` in sensitive.go.
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
		file, parseErr := parser.ParseFile(fset, path, nil, 0)
		if parseErr != nil {
			return parseErr
		}
		parsed++
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		inExec := filepath.ToSlash(filepath.Dir(rel)) == "internal/exec"
		isTest := strings.HasSuffix(rel, "_test.go")

		if inExec {
			if !isTest {
				checkExecFile(file, rel, report)
			}
			return nil
		}
		alias := ""
		for _, imp := range file.Imports {
			if p, _ := strconv.Unquote(imp.Path.Value); p != execImportPath {
				continue
			}
			alias = "exec"
			if imp.Name != nil {
				alias = imp.Name.Name
			}
			if alias == "." {
				report(imp.Pos(), "dot-import of internal/exec hides references to the transform seam; use a named import")
			}
		}
		if alias == "" || alias == "." || alias == "_" {
			return nil
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

// checkExecFile applies the two in-package rules to one production file of
// internal/exec.
func checkExecFile(file *ast.File, rel string, report func(token.Pos, string)) {
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || !fn.Name.IsExported() {
			continue
		}
		if fn.Recv != nil && !receiverExported(fn.Recv) {
			continue
		}
		for _, field := range fn.Type.Params.List {
			if _, isFunc := field.Type.(*ast.FuncType); isFunc {
				report(field.Pos(), fn.Name.Name+" takes a func parameter; a callback can receive an opaque payload, so the seam offers closed Transforms instead")
			}
		}
	}
	if rel == "internal/exec/transform.go" {
		return
	}
	ast.Inspect(file, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		if id, ok := lit.Type.(*ast.Ident); ok && id.Name == "Transform" {
			report(lit.Pos(), "a Transform is minted outside transform.go; keep the closed set in one file")
		}
		return true
	})
}

func receiverExported(recv *ast.FieldList) bool {
	if len(recv.List) == 0 {
		return false
	}
	typ := recv.List[0].Type
	if star, ok := typ.(*ast.StarExpr); ok {
		typ = star.X
	}
	id, ok := typ.(*ast.Ident)
	return ok && id.IsExported()
}
