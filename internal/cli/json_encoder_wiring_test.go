package cli

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// jsonEmitters are the encoding/json entry points that can put a document on
// stdout. Marshal is in the set because `json.Marshal` followed by a Fprintf is
// the most natural way to add the next --json surface, and a guard that misses
// it is a guard with a hole exactly where the next one will be drilled.
var jsonEmitters = map[string]bool{
	"NewEncoder":    true,
	"Marshal":       true,
	"MarshalIndent": true,
}

// TestJSONEmittersUseTermsafeEncoder is the coverage check the per-command
// tests cannot be: #279 is a defect of an ABSENT control, and a control that is
// present on one surface and absent on the next has not been fixed. A test per
// --json command would grow a new hole every time a command is added, so this
// asserts the one property directly — no command in this package builds a bare
// encoding/json emitter for its own output.
//
// internal/cli is the right scope: it is the package whose encoders write to
// stdout, which is where an unsafe character reaches an operator's terminal.
// Encoders that serialize to a file or a cache deliberately keep the stdlib
// call — internal/launch/usage_encode.go is the documented example — and are
// out of scope here.
//
// Two details keep the guard from being fooled. The package identifier is
// resolved from each file's own import block rather than assumed to be `json`,
// so `import j "encoding/json"` cannot slip past a name comparison. And files
// are enumerated and parsed individually rather than with parser.ParseDir,
// which is deprecated precisely because it drops files whose build tags exclude
// them from the current platform — exactly the files a host-only audit misses.
func TestJSONEmittersUseTermsafeEncoder(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read internal/cli: %v", err)
	}

	fset := token.NewFileSet()
	var parsed, emitters int
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		parsed++

		jsonName := localNameFor(t, file, "encoding/json")
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch fun := call.Fun.(type) {
			case *ast.SelectorExpr:
				pkgIdent, ok := fun.X.(*ast.Ident)
				if !ok {
					return true
				}
				switch {
				case pkgIdent.Name == "termsafe" && fun.Sel.Name == "JSONEncoder":
					emitters++
				case jsonName != "" && jsonName != "." && pkgIdent.Name == jsonName && jsonEmitters[fun.Sel.Name]:
					emitters++
					t.Errorf("%s: %s.%s writes JSON without the terminal-escaping filter; use termsafe.JSONEncoder",
						fset.Position(call.Pos()), jsonName, fun.Sel.Name)
				}
			case *ast.Ident:
				// A dot-import makes NewEncoder(w) a bare identifier with no
				// qualifier, so the selector branch above never sees it — the
				// one shape that reads as ordinary code and matches nothing.
				if jsonName == "." && jsonEmitters[fun.Name] {
					emitters++
					t.Errorf("%s: dot-imported %s writes JSON without the terminal-escaping filter; use termsafe.JSONEncoder",
						fset.Position(call.Pos()), fun.Name)
				}
			}
			return true
		})
	}

	// Without these the check passes just as loudly on a walk that matched
	// nothing at all — a green proving the walk ran, not the package clean.
	if parsed == 0 {
		t.Fatal("parsed no non-test files in internal/cli; the walk is broken, not the package clean")
	}
	if emitters == 0 {
		t.Fatalf("parsed %d files but found no JSON emitter; the matcher is broken, not the package clean", parsed)
	}
}

// localNameFor returns the identifier this file binds to importPath — the
// alias when one is given, the last path segment otherwise, or "." for a
// dot-import — and "" when the file does not import it. Resolving this is what
// makes the guard immune to an aliased import; assuming the name would make the
// alias the bypass.
func localNameFor(t *testing.T, file *ast.File, importPath string) string {
	t.Helper()
	for _, spec := range file.Imports {
		path, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			t.Fatalf("unquote import %s: %v", spec.Path.Value, err)
		}
		if path != importPath {
			continue
		}
		if spec.Name != nil {
			return spec.Name.Name
		}
		return importPath[strings.LastIndex(importPath, "/")+1:]
	}
	return ""
}
