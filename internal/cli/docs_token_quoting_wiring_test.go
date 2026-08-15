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

// TestDocsTokenErrorsQuoteThePathExactlyOnce is a source-level guard, and it is
// source-level for a reason: wrapDocsTokenDescriptorError renders the path
// itself, so passing it the already-rendered displayPath quotes it twice —
// and the two call sites where that survived are inside openDocsTokenFile,
// on the branch where an opened descriptor's Stat fails. No unit test can
// reach that branch without a descriptor whose fstat fails on demand, and a
// mutation probe confirmed the defect survives the whole suite.
//
// The other half of the reason is build tags. docs_token_file_other.go never
// compiles on this machine, so a host-only check could not see it at all;
// parsing files individually reads every platform's opener regardless of which
// one the current GOOS selects. QuotePath, unlike the sanitizer it replaced, is
// not idempotent — that is what makes this a rule rather than a preference.
func TestDocsTokenErrorsQuoteThePathExactlyOnce(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read internal/cli: %v", err)
	}

	var parsed, checked int
	fset := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, "docs_token_file") ||
			!strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		parsed++

		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			fn, ok := call.Fun.(*ast.Ident)
			if !ok || fn.Name != "wrapDocsTokenDescriptorError" {
				return true
			}
			checked++
			if len(call.Args) < 2 {
				t.Errorf("%s: wrapDocsTokenDescriptorError with too few arguments", fset.Position(call.Pos()))
				return true
			}
			if ident, ok := call.Args[1].(*ast.Ident); ok && ident.Name != "path" {
				t.Errorf("%s: wrapDocsTokenDescriptorError takes the RAW path, got %q — "+
					"it renders the path itself, and QuotePath is not idempotent",
					fset.Position(call.Pos()), ident.Name)
			}
			return true
		})
	}

	// Without these the guard passes just as loudly on a walk that matched
	// nothing — a green proving the walk ran, not the call sites clean.
	if parsed < 2 {
		t.Fatalf("parsed %d docs_token_file source(s); expected the shared file plus every platform opener", parsed)
	}
	if checked == 0 {
		t.Fatal("found no wrapDocsTokenDescriptorError call; the matcher is broken, not the call sites clean")
	}
}
