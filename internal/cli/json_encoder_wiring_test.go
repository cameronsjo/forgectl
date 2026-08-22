package cli

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const rawJSONExemption = "termsafe:allow-raw-json"

// jsonEmitters are the encoding/json entry points that can build a document.
// Marshal belongs here even when its immediate result is a []byte: Marshal
// followed by a write is the easiest way for a new terminal sink to bypass
// termsafe.JSONEncoder.
var jsonEmitters = map[string]bool{
	"NewEncoder":    true,
	"Marshal":       true,
	"MarshalIndent": true,
}

type jsonEmitterFinding struct {
	position token.Position
	message  string
}

type exemptionMarker struct {
	position token.Position
	used     int
}

// TestJSONEmittersUseTermsafeEncoder is the default-deny coverage check that a
// per-command test cannot be. It walks every production Go file in the module,
// including build-tagged files, so moving a command-output helper out of
// internal/cli cannot move it outside the control.
//
// A raw encoding/json emitter is allowed only with a one-call marker on the
// same line or immediately above it:
//
//	// termsafe:allow-raw-json persisted cache record, never command output
//	data, err := json.Marshal(record)
//
// The reason is mandatory, and a marker cannot cover two calls. File, cache,
// HTTP, and protocol encoders are not exempt by category: a new sink defaults
// to covered until a reviewer records why that exact site cannot render to an
// operator's terminal. Unused markers fail too, so an exemption cannot drift
// onto unrelated code after its emitter is removed.
func TestJSONEmittersUseTermsafeEncoder(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("module root: %v", err)
	}
	findings, parsed, emitters, err := scanRawJSONEmitters(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, finding := range findings {
		t.Errorf("%s: %s", finding.position, finding.message)
	}
	if parsed == 0 {
		t.Fatal("parsed no production Go files; the walk is broken, not the module clean")
	}
	if emitters == 0 {
		t.Fatalf("parsed %d files but found no encoding/json emitter; the matcher is broken, not the module clean", parsed)
	}
}

func scanRawJSONEmitters(root string) (findings []jsonEmitterFinding, parsed, emitters int, err error) {
	fset := token.NewFileSet()
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
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}

		file, parseErr := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if parseErr != nil {
			return fmt.Errorf("parse %s: %w", path, parseErr)
		}
		parsed++
		jsonName := importLocalName(file, "encoding/json")
		markers, markerFindings := exemptionMarkers(fset, file)
		findings = append(findings, markerFindings...)

		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || !isJSONEmitter(call, jsonName) {
				return true
			}
			emitters++
			position := fset.Position(call.Pos())
			marker := markerForCall(markers, position.Line)
			if marker != nil {
				marker.used++
				if marker.used > 1 {
					findings = append(findings, jsonEmitterFinding{position: position, message: "raw JSON exemption covers more than one emitter; give each call its own marker"})
				}
				return true
			}
			findings = append(findings, jsonEmitterFinding{
				position: position,
				message:  "encoding/json emitter lacks terminal escaping; use termsafe.JSONEncoder or add a one-call " + rawJSONExemption + " reason",
			})
			return true
		})

		for _, marker := range markers {
			if marker.used == 0 {
				findings = append(findings, jsonEmitterFinding{position: marker.position, message: "unused raw JSON exemption marker"})
			}
		}
		return nil
	})
	return findings, parsed, emitters, err
}

func isJSONEmitter(call *ast.CallExpr, jsonName string) bool {
	switch fun := call.Fun.(type) {
	case *ast.SelectorExpr:
		pkgIdent, ok := fun.X.(*ast.Ident)
		return ok && jsonName != "" && jsonName != "." && pkgIdent.Name == jsonName && jsonEmitters[fun.Sel.Name]
	case *ast.Ident:
		return jsonName == "." && jsonEmitters[fun.Name]
	default:
		return false
	}
}

func exemptionMarkers(fset *token.FileSet, file *ast.File) (map[int]*exemptionMarker, []jsonEmitterFinding) {
	markers := make(map[int]*exemptionMarker)
	var findings []jsonEmitterFinding
	for _, group := range file.Comments {
		for _, comment := range group.List {
			text := strings.TrimSpace(strings.TrimPrefix(comment.Text, "//"))
			if text != rawJSONExemption && !strings.HasPrefix(text, rawJSONExemption+" ") {
				continue
			}
			position := fset.Position(comment.Pos())
			if !strings.HasPrefix(comment.Text, "//") || text == rawJSONExemption {
				findings = append(findings, jsonEmitterFinding{position: position, message: rawJSONExemption + " requires a reason"})
			}
			if _, exists := markers[position.Line]; exists {
				findings = append(findings, jsonEmitterFinding{position: position, message: "more than one raw JSON exemption marker on a line"})
				continue
			}
			markers[position.Line] = &exemptionMarker{position: position}
		}
	}
	return markers, findings
}

func markerForCall(markers map[int]*exemptionMarker, callLine int) *exemptionMarker {
	if marker := markers[callLine]; marker != nil {
		return marker
	}
	return markers[callLine-1]
}

// localNameFor returns the identifier this file binds to importPath — the
// alias when one is given, the last path segment otherwise, or "." for a
// dot-import. Resolving the import is what makes the guard immune to spelling:
// import j "encoding/json" is exactly as covered as the conventional name.
func importLocalName(file *ast.File, importPath string) string {
	for _, spec := range file.Imports {
		path, err := strconv.Unquote(spec.Path.Value)
		if err != nil || path != importPath {
			continue
		}
		if spec.Name != nil {
			return spec.Name.Name
		}
		return importPath[strings.LastIndex(importPath, "/")+1:]
	}
	return ""
}

// localNameFor is shared with the other AST wiring guards in this package.
func localNameFor(t *testing.T, file *ast.File, importPath string) string {
	t.Helper()
	return importLocalName(file, importPath)
}

func TestJSONEmitterGuardAdversarialCases(t *testing.T) {
	tests := []struct {
		name         string
		source       string
		wantFindings int
		wantEmitters int
	}{
		{name: "other package NewEncoder", source: `package docs; import "encoding/json"; func f(w anyWriter) { json.NewEncoder(w) }`, wantFindings: 1, wantEmitters: 1},
		{name: "aliased Marshal", source: `package output; import j "encoding/json"; func f(v any) { _, _ = j.Marshal(v) }`, wantFindings: 1, wantEmitters: 1},
		{name: "dot imported MarshalIndent", source: `package output; import . "encoding/json"; func f(v any) { _, _ = MarshalIndent(v, "", "  ") }`, wantFindings: 1, wantEmitters: 1},
		{name: "all variants", source: `package output
import j "encoding/json"
func f(w anyWriter, v any) { j.NewEncoder(w); j.Marshal(v); j.MarshalIndent(v, "", "") }`, wantFindings: 3, wantEmitters: 3},
		{name: "local identifier is not import", source: `package output; func f(json fakeJSON, v any) { json.Marshal(v) }`, wantFindings: 0, wantEmitters: 0},
		{name: "reasoned previous-line exemption", source: `package cache
import "encoding/json"
func f(v any) {
// termsafe:allow-raw-json persisted cache record, never command output
json.Marshal(v)
}`, wantFindings: 0, wantEmitters: 1},
		{name: "reasoned same-line exemption", source: `package cache; import "encoding/json"; func f(v any) { json.Marshal(v) // termsafe:allow-raw-json protocol payload
}`, wantFindings: 0, wantEmitters: 1},
		{name: "marker needs reason", source: `package cache; import "encoding/json"; func f(v any) {
// termsafe:allow-raw-json
json.Marshal(v)
}`, wantFindings: 1, wantEmitters: 1},
		{name: "one marker cannot cover two calls", source: `package cache; import "encoding/json"; func f(v any) {
// termsafe:allow-raw-json persisted cache record
json.Marshal(v); json.Marshal(v)
}`, wantFindings: 1, wantEmitters: 2},
		{name: "unused marker fails", source: `package cache
// termsafe:allow-raw-json stale exemption
func f() {}`, wantFindings: 1, wantEmitters: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "internal", "other", "source.go")
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(tt.source), 0o600); err != nil {
				t.Fatal(err)
			}
			findings, _, emitters, err := scanRawJSONEmitters(root)
			if err != nil {
				t.Fatal(err)
			}
			if len(findings) != tt.wantFindings {
				t.Errorf("findings = %d (%v), want %d", len(findings), findings, tt.wantFindings)
			}
			if emitters != tt.wantEmitters {
				t.Errorf("emitters = %d, want %d", emitters, tt.wantEmitters)
			}
		})
	}
}
