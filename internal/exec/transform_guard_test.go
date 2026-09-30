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

// unsafeAllowed is the allowlist of files, relative to the module root, that
// may import "unsafe". It is empty: nothing in the module imports it today
// (forgectl#854), and a new entry must name why the file needs to read memory
// past the type system.
var unsafeAllowed = map[string]string{}

// TestNoFileReachesPastTheTypeSystem closes the hole the guard tests above
// cannot see (forgectl#854, P-1): every one of them reasons about what Go's
// type system lets a file name, and three things step around it. A
// //go:linkname directive binds a local declaration to any symbol in any
// package, unexported or internal, so another package could call
// (*OSSensitiveRunner).buildCmd and read the argv it assembles; a probe did,
// and got the payload back while every other guard stayed green. An "unsafe"
// import reads any memory, including a sealed Arg's closure, and it is also
// what the compiler requires before it honours a linkname. An assembly or
// .syso file links in code that names any symbol directly. So, in every file
// of the module, tests and every build constraint included:
//
//   - no //go:linkname directive, anywhere;
//   - no "unsafe" import outside unsafeAllowed;
//   - no .s, .S or .syso file.
//
// This is the interim guard; sealing the payload behind a package boundary
// (forgectl#854) does not retire it, because a linkname reaches into an
// internal package too.
//
// Mutations that turn it red: a file anywhere outside internal/exec with
// `import _ "unsafe"` and `//go:linkname buildCmd
// github.com/cameronsjo/forgectl/internal/exec.(*OSSensitiveRunner).buildCmd`
// (both rules fire; add the file to unsafeAllowed and the linkname rule still
// fires); a new file importing "unsafe" alone; an empty x_amd64.s.
func TestNoFileReachesPastTheTypeSystem(t *testing.T) {
	rootDir, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("module root: %v", err)
	}
	root, err := os.OpenRoot(rootDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	fsys := root.FS()
	fset := token.NewFileSet()
	parsed, sawExec := 0, false
	var findings []string
	report := func(pos token.Pos, msg string) {
		findings = append(findings, fset.Position(pos).String()+": "+msg)
	}
	err = fs.WalkDir(fsys, ".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		name := entry.Name()
		if entry.IsDir() {
			if path != "." && (strings.HasPrefix(name, ".") || name == "vendor" || name == "testdata") {
				return fs.SkipDir
			}
			return nil
		}
		switch filepath.Ext(name) {
		case ".s", ".S", ".syso":
			findings = append(findings, path+": an assembly or object file can name any symbol past the type system; forgectl ships none")
			return nil
		case ".go":
		default:
			return nil
		}
		src, readErr := fs.ReadFile(fsys, path)
		if readErr != nil {
			return readErr
		}
		file, parseErr := parser.ParseFile(fset, path, src, parser.ImportsOnly|parser.ParseComments|parser.SkipObjectResolution)
		if parseErr != nil {
			return parseErr
		}
		parsed++
		sawExec = sawExec || filepath.ToSlash(filepath.Dir(path)) == "internal/exec"
		for _, imp := range file.Imports {
			if p, _ := strconv.Unquote(imp.Path.Value); p == "unsafe" && unsafeAllowed[path] == "" {
				report(imp.Pos(), `imports "unsafe", which reads any memory, a sealed payload included; add the file to unsafeAllowed with a reason only after review`)
			}
		}
		// ImportsOnly stops at the imports, so scan the rest of the source for
		// the directive too: a linkname sits beside a declaration further down.
		for i, line := range strings.Split(string(src), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "//go:linkname") {
				findings = append(findings, path+":"+strconv.Itoa(i+1)+": a //go:linkname directive binds to any symbol, unexported or internal, so it can call buildCmd and read a payload")
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range findings {
		t.Error(f)
	}
	if parsed == 0 || !sawExec {
		t.Fatalf("parsed %d Go files (internal/exec among them: %v); the walk is broken, not the module clean", parsed, sawExec)
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
// production file of internal/exec but transform.go may, on any platform in
// guardPlatforms,
//
//   - write a Transform composite literal with elements,
//   - convert any value to Transform (which would mint one from an
//     identical struct type holding an arbitrary func),
//   - assign Transform.apply, or
//   - take the address of Transform.apply (which would let the pointer's
//     holder assign it later).
//
// The API golden (TestExportedAPI) cannot see this, because minting inside
// the package changes no signature.
//
// Mutations that turn it red, each written in sensitive.go:
//
//   - var t Transform; t.apply = strings.ToUpper     (minting by assignment)
//   - Transform{apply: strings.ToUpper}              (minting by literal)
//   - Transform(struct{ apply func(string) string }{strings.ToUpper})
//     (minting by conversion)
//   - p := &t.apply; *p = strings.ToUpper              (minting through a pointer)
func TestTransformIsMintedOnlyInTransformGo(t *testing.T) {
	for _, p := range guardPlatforms {
		c := checkExecFor(t, p)
		for _, f := range mintingFindings(t, c) {
			t.Errorf("[%s] %s", p, f)
		}
	}
}

// mintingFindings returns a finding for each TestTransformIsMintedOnlyInTransformGo
// rule broken outside transform.go.
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
		findings = append(findings, c.fset.Position(pos).String()+": "+msg+"; keep the closed set in transform.go")
	}
	isApply := func(e ast.Expr) bool {
		sel, ok := ast.Unparen(e).(*ast.SelectorExpr)
		if !ok {
			return false
		}
		s, ok := c.info.Selections[sel]
		return ok && s.Obj() == apply
	}
	for _, f := range c.files {
		if filepath.Base(c.fset.Position(f.Pos()).Filename) == "transform.go" {
			continue
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.CompositeLit:
				if tv, ok := c.info.Types[node]; ok && types.Identical(tv.Type, transform.Type()) && len(node.Elts) > 0 {
					report(node.Pos(), "a Transform is minted by a composite literal outside transform.go")
				}
			case *ast.CallExpr:
				if tv, ok := c.info.Types[node.Fun]; ok && tv.IsType() && types.Identical(tv.Type, transform.Type()) {
					report(node.Pos(), "a value is converted to Transform outside transform.go")
				}
			case *ast.AssignStmt:
				for _, lhs := range node.Lhs {
					if isApply(lhs) {
						report(lhs.Pos(), "Transform.apply is assigned outside transform.go")
					}
				}
			case *ast.UnaryExpr:
				if node.Op == token.AND && isApply(node.X) {
					report(node.Pos(), "the address of Transform.apply is taken outside transform.go")
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
// constraint, so a file tagged for another platform is held to the same
// rules:
//
//   - the only import is strings;
//   - the only declarations are funcs: no package-level var (which could
//     capture a payload), const, type or init;
//   - every func is a plain, non-generic, non-method func with a body (no
//     assembly), whose parameters are all string and whose single result is
//     string or bool;
//   - a body starts no goroutine and calls only through a selector (a
//     strings func, a method of a strings type, or a func-typed field of a
//     local value; see the waiver below), the builtin len, a string
//     conversion, or another func of this package. print, println and
//     every other builtin are refused, so nothing is written anywhere.
//
// Mutations that turn it red: import "os" in tmuxesc.go; declare
// `var last string`; add `func Hook(f func(string)) string`; call
// println(s) inside Format.
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
	var files []*ast.File
	own := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, file)
		for _, decl := range file.Decls {
			if d, ok := decl.(*ast.FuncDecl); ok && d.Recv == nil {
				own[d.Name.Name] = true
			}
		}
	}
	funcs := 0
	for _, file := range files {
		for _, f := range tmuxescFindings(fset, file, own, &funcs) {
			t.Error(f)
		}
	}
	if funcs == 0 {
		t.Fatal("found no funcs in tmuxesc; the walk is broken, not the package clean")
	}
}

// tmuxescFindings applies TestTmuxescIsALeaf's rules to one file, counting
// the funcs it accepts into funcs. own names the package's plain funcs.
func tmuxescFindings(fset *token.FileSet, file *ast.File, own map[string]bool, funcs *int) []string {
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
			case d.Body == nil:
				report(d.Pos(), name+" has no body; an assembly func cannot be checked")
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
			bodyOK := true
			ast.Inspect(d.Body, func(n ast.Node) bool {
				switch node := n.(type) {
				case *ast.GoStmt:
					report(node.Pos(), name+" starts a goroutine; tmuxesc funcs must be pure")
					bodyOK = false
				case *ast.CallExpr:
					switch fun := node.Fun.(type) {
					case *ast.SelectorExpr:
						// Any selector call passes, unchecked by type: a strings.X
						// call (strings is the only import), a method of a value
						// whose type comes from strings, or a call through a field
						// of a local struct. The last is not "a type only strings
						// can supply", but whatever func it holds was built here
						// from strings funcs, this package's funcs or func
						// literals, and Inspect walks every one of those bodies.
						return true
					case *ast.Ident:
						if fun.Name == "len" || fun.Name == "string" || own[fun.Name] {
							return true
						}
					}
					report(node.Pos(), name+" calls "+types.ExprString(node.Fun)+"; tmuxesc bodies may call only strings, len, string conversions and the package's own funcs")
					bodyOK = false
				}
				return true
			})
			if bodyOK {
				*funcs++
			}
		}
	}
	return findings
}
