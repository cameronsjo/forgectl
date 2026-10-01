package gitleaks

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path"
	"strconv"
	"strings"
	"testing"
)

// gitleaksImports is the import allowlist for the package's shipped files.
// A new import is a deliberate, reviewed edit here.
var gitleaksImports = map[string]bool{
	"context":       true,
	"encoding/json": true,
	"errors":        true,
	"io":            true,
	"io/fs":         true,
	"os":            true,
	"os/exec":       true,
	"path/filepath": true,
	"sort":          true,
	"strconv":       true,
	"strings":       true,
	"time":          true,
	"github.com/cameronsjo/forgectl/internal/exec":       true,
	"github.com/cameronsjo/forgectl/internal/selfupdate": true,
}

// sourceViolations parses the named Go sources and returns every breach of
// the package's shape rules:
//   - an import outside gitleaksImports;
//   - any os selector outside temp.go, so the temp dir, the config and the
//     report are the package's whole filesystem surface;
//   - any os/exec selector but ErrDot, so no process starts except through
//     the Runner;
//   - a RunWithEnvFiltered call whose removals are not UnsetEnv().
func sourceViolations(t *testing.T, srcs map[string]string) []string {
	t.Helper()
	var bad []string
	fset := token.NewFileSet()
	for name, src := range srcs {
		f, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		local := map[string]string{} // local name -> import path
		for _, imp := range f.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			if !gitleaksImports[p] {
				bad = append(bad, fmt.Sprintf("%s: import %q not allowlisted", name, p))
			}
			n := path.Base(p)
			if imp.Name != nil {
				n = imp.Name.Name
			}
			local[n] = p
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch e := n.(type) {
			case *ast.SelectorExpr:
				id, ok := e.X.(*ast.Ident)
				if !ok {
					return true
				}
				switch local[id.Name] {
				case "os":
					if name != "temp.go" {
						bad = append(bad, fmt.Sprintf("%s: os.%s outside temp.go", fset.Position(e.Pos()), e.Sel.Name))
					}
				case "os/exec":
					if e.Sel.Name != "ErrDot" {
						bad = append(bad, fmt.Sprintf("%s: exec.%s; processes start only through the Runner", fset.Position(e.Pos()), e.Sel.Name))
					}
				}
			case *ast.CallExpr:
				sel, ok := e.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "RunWithEnvFiltered" {
					return true
				}
				ok = len(e.Args) >= 4
				if ok {
					call, isCall := e.Args[2].(*ast.CallExpr)
					fn, isIdent := (*ast.Ident)(nil), false
					if isCall {
						fn, isIdent = call.Fun.(*ast.Ident)
					}
					ok = isCall && isIdent && fn.Name == "UnsetEnv" && len(call.Args) == 0
				}
				if !ok {
					bad = append(bad, fmt.Sprintf("%s: RunWithEnvFiltered without UnsetEnv() as its removals", fset.Position(e.Pos())))
				}
			}
			return true
		})
	}
	return bad
}

// TestGitleaksSource_FilesystemOnlyInTemp pins the package's shape against
// its shipped source. Mutations that turn it red: an os.ReadFile in
// scan.go; an exec.Command anywhere; a RunWithEnvFiltered passing nil
// removals; importing net/http.
func TestGitleaksSource_FilesystemOnlyInTemp(t *testing.T) {
	dirents, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	srcs := map[string]string{}
	for _, d := range dirents {
		if !strings.HasSuffix(d.Name(), ".go") || strings.HasSuffix(d.Name(), "_test.go") {
			continue
		}
		b, err := os.ReadFile(d.Name())
		if err != nil {
			t.Fatal(err)
		}
		srcs[d.Name()] = string(b)
	}
	if len(srcs) < 4 {
		t.Fatalf("read %d sources; the pin is vacuous", len(srcs))
	}
	for _, b := range sourceViolations(t, srcs) {
		t.Error(b)
	}
}

// TestGitleaksSource_ProbesGoRed proves each rule refuses what it names.
func TestGitleaksSource_ProbesGoRed(t *testing.T) {
	probes := map[string]string{
		"os outside temp":  "package gitleaks\nimport \"os\"\nfunc f() { _, _ = os.ReadFile(\"x\") }",
		"aliased os":       "package gitleaks\nimport xos \"os\"\nfunc f() { _ = xos.Remove(\"x\") }",
		"exec.Command":     "package gitleaks\nimport \"os/exec\"\nfunc f() { _ = exec.Command(\"x\") }",
		"nil removals":     "package gitleaks\nfunc f(r Runner) { _, _ = r.RunWithEnvFiltered(nil, nil, nil, \"x\") }",
		"unlisted import":  "package gitleaks\nimport _ \"net/http\"",
		"partial removals": "package gitleaks\nfunc f(r Runner) { _, _ = r.RunWithEnvFiltered(nil, nil, []string{\"GITLEAKS_CONFIG\"}, \"x\") }",
	}
	for name, src := range probes {
		if bad := sourceViolations(t, map[string]string{"scan.go": src}); len(bad) == 0 {
			t.Errorf("probe %q was not refused", name)
		}
	}
}
