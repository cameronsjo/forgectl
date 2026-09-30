package exec

import (
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// updateAPI regenerates testdata/exported_api*.golden from the live package.
// Run it only after reviewing the surface change it records.
var updateAPI = flag.Bool("update", false, "rewrite testdata/exported_api*.golden from internal/exec's exported API")

// guardGOOS is every GOOS the package guards type-check internal/exec for,
// so a file whose build constraint excludes the host is still covered.
var guardGOOS = []string{"linux", "darwin", "windows", "freebsd"}

const (
	apiGoldenDir    = "testdata"
	apiGoldenShared = "exported_api.golden"
	apiRegenerate   = "go test ./internal/exec -run TestExportedAPI -update"
)

// TestExportedAPI pins internal/exec's whole exported surface in
// testdata/exported_api.golden: every exported func, var, const and type,
// every exported field, and the exported method set (promoted methods
// included) of every named type, unexported ones too, since a method on an
// unexported type is callable through a value an importer holds or reaches
// through an interface assertion. It is the guard behind the sealed-payload
// promise: a callback parameter of any shape (func, named func, generic,
// variadic, interface, any), an accessor that returns a payload, or an
// exported func var is a change to this surface, so it fails here until a
// reviewer accepts it and regenerates the golden. The golden records the
// surface; it does not judge it. Judging is the review that precedes -update.
//
// The surface is computed once per GOOS in guardGOOS. When every GOOS agrees
// there is one shared golden; when a surface legitimately differs, -update
// writes exported_api_<goos>.golden per GOOS instead, and each GOOS is held
// to its own file.
//
// Mutations that turn it red, each written in a production file:
//
//   - func (a Arg) Peek() string { return a.reveal() }  (an accessor returning the payload)
//   - var Hook func(string) string                     (an exported func var)
//   - func Inspect(a Arg, v any)                        (an any parameter)
//   - func WinOnly() {} in a new x_windows.go            (a windows-only export)
func TestExportedAPI(t *testing.T) {
	got := map[string]string{}
	for _, goos := range guardGOOS {
		got[goos] = renderExportedAPI(checkExecFor(t, goos).pkg)
	}
	if *updateAPI {
		writeAPIGoldens(t, got)
		return
	}
	for _, goos := range guardGOOS {
		name := "exported_api_" + goos + ".golden"
		want, err := os.ReadFile(filepath.Clean(filepath.Join(apiGoldenDir, name)))
		if errors.Is(err, os.ErrNotExist) {
			name = apiGoldenShared
			want, err = os.ReadFile(filepath.Clean(filepath.Join(apiGoldenDir, name)))
		}
		if err != nil {
			t.Fatalf("read golden for GOOS=%s: %v; after reviewing the surface, create it with: %s", goos, err, apiRegenerate)
		}
		if diff := lineDiff(string(want), got[goos]); diff != "" {
			t.Errorf("GOOS=%s: internal/exec's exported API differs from testdata/%s:\n%s\n"+
				"Every exported name is a way for a sealed payload to reach caller code. Review the change "+
				"(no callback parameter, no accessor or func var that yields a payload), then regenerate with:\n\t%s",
				goos, name, diff, apiRegenerate)
		}
	}
}

// writeAPIGoldens writes one shared golden when every GOOS agrees, and one
// golden per GOOS otherwise, removing whichever form no longer applies.
func writeAPIGoldens(t *testing.T, got map[string]string) {
	t.Helper()
	if err := os.MkdirAll(apiGoldenDir, 0o750); err != nil {
		t.Fatal(err)
	}
	shared := true
	for _, goos := range guardGOOS {
		shared = shared && got[goos] == got[guardGOOS[0]]
	}
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(apiGoldenDir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	remove := func(name string) {
		if err := os.Remove(filepath.Join(apiGoldenDir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	}
	for _, goos := range guardGOOS {
		name := "exported_api_" + goos + ".golden"
		if shared {
			remove(name)
		} else {
			write(name, got[goos])
		}
	}
	if shared {
		write(apiGoldenShared, got[guardGOOS[0]])
	} else {
		remove(apiGoldenShared)
	}
}

// lineDiff lists the lines only in want (-) and only in got (+), or "" when
// the two sets agree. Both are sorted line sets, so a set difference is the
// whole story.
func lineDiff(want, got string) string {
	in := func(s string) map[string]bool {
		m := map[string]bool{}
		for _, l := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
			m[l] = true
		}
		return m
	}
	w, g := in(want), in(got)
	var out []string
	for l := range w {
		if !g[l] {
			out = append(out, "- "+l)
		}
	}
	for l := range g {
		if !w[l] {
			out = append(out, "+ "+l)
		}
	}
	sort.Strings(out)
	return strings.Join(out, "\n")
}

// renderExportedAPI spells pkg's exported surface as sorted lines, one per
// func, var, const, type, exported field and exported method.
func renderExportedAPI(pkg *types.Package) string {
	qual := types.RelativeTo(pkg)
	var lines []string
	add := func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }
	scope := pkg.Scope()
	for _, name := range scope.Names() {
		switch obj := scope.Lookup(name).(type) {
		case *types.Func:
			if obj.Exported() {
				add("func %s%s", name, strings.TrimPrefix(types.TypeString(obj.Type(), qual), "func"))
			}
		case *types.Var:
			if obj.Exported() {
				add("var %s %s", name, types.TypeString(obj.Type(), qual))
			}
		case *types.Const:
			if obj.Exported() {
				add("const %s %s", name, types.TypeString(obj.Type(), qual))
			}
		case *types.TypeName:
			renderType(obj, qual, add)
		}
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n") + "\n"
}

// renderType spells an exported type's declaration, and for any named type,
// exported or not, its exported fields and its exported methods.
func renderType(obj *types.TypeName, qual types.Qualifier, add func(string, ...any)) {
	name := obj.Name()
	if obj.IsAlias() {
		if obj.Exported() {
			add("type %s = %s", name, types.TypeString(types.Unalias(obj.Type()), qual))
		}
		return
	}
	named, ok := obj.Type().(*types.Named)
	if !ok {
		return
	}
	var tparams []string
	for i := 0; i < named.TypeParams().Len(); i++ {
		tp := named.TypeParams().At(i)
		tparams = append(tparams, tp.Obj().Name()+" "+types.TypeString(tp.Constraint(), qual))
	}
	head := name
	if len(tparams) > 0 {
		head += "[" + strings.Join(tparams, ", ") + "]"
	}
	under := named.Underlying()
	st, isStruct := under.(*types.Struct)
	if obj.Exported() {
		if isStruct {
			add("type %s struct", head)
		} else {
			add("type %s %s", head, types.TypeString(under, qual))
		}
	}
	if isStruct {
		for i := 0; i < st.NumFields(); i++ {
			if f := st.Field(i); f.Exported() {
				embedded := ""
				if f.Embedded() {
					embedded = " (embedded)"
				}
				add("field %s.%s %s%s", name, f.Name(), types.TypeString(f.Type(), qual), embedded)
			}
		}
	}
	if _, isIface := under.(*types.Interface); isIface {
		return // an interface's methods are part of its type line
	}
	valueSet := types.NewMethodSet(named)
	ptrSet := types.NewMethodSet(types.NewPointer(named))
	for i := 0; i < ptrSet.Len(); i++ {
		m := ptrSet.At(i).Obj()
		if !m.Exported() {
			continue
		}
		recv := "*" + name
		if valueSet.Lookup(m.Pkg(), m.Name()) != nil {
			recv = name
		}
		sig := m.Type().(*types.Signature)
		plain := types.NewSignatureType(nil, nil, nil, sig.Params(), sig.Results(), sig.Variadic())
		add("method (%s) %s%s", recv, m.Name(), strings.TrimPrefix(types.TypeString(plain, qual), "func"))
	}
}

// checkedPackage is internal/exec's production files type-checked for one
// GOOS.
type checkedPackage struct {
	fset  *token.FileSet
	files []*ast.File
	info  *types.Info
	pkg   *types.Package
}

var checkedByGOOS = map[string]*checkedPackage{}

// checkExecFor type-checks internal/exec's production files as they build
// for goos (amd64, cgo off), resolving every import from source under the
// same build.Context so a GOOS-tagged dependency file is read too. Results
// are cached per GOOS for the test binary's life.
func checkExecFor(t *testing.T, goos string) *checkedPackage {
	t.Helper()
	if c, ok := checkedByGOOS[goos]; ok {
		return c
	}
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	ctx := build.Default
	ctx.GOOS, ctx.GOARCH, ctx.CgoEnabled = goos, "amd64", false
	bp, err := ctx.ImportDir(dir, 0)
	if err != nil {
		t.Fatalf("list internal/exec for GOOS=%s: %v", goos, err)
	}
	fset := token.NewFileSet()
	files, err := parseGoFiles(fset, dir, bp.GoFiles)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatalf("parsed no production files of internal/exec for GOOS=%s", goos)
	}
	info := &types.Info{
		Types:      map[ast.Expr]types.TypeAndValue{},
		Selections: map[*ast.SelectorExpr]*types.Selection{},
	}
	imp := &sourceImporter{ctx: &ctx, fset: fset, pkgs: map[string]*types.Package{}}
	conf := types.Config{Importer: imp, Sizes: types.SizesFor("gc", ctx.GOARCH)}
	pkg, err := conf.Check(execImportPath, fset, files, info)
	if err != nil {
		t.Fatalf("type-check internal/exec for GOOS=%s: %v", goos, err)
	}
	c := &checkedPackage{fset: fset, files: files, info: info, pkg: pkg}
	checkedByGOOS[goos] = c
	return c
}

// sourceImporter type-checks imports from source under one build.Context,
// function bodies skipped. go/importer's "source" importer is fixed to
// build.Default, so it would read a dependency's host-GOOS files.
type sourceImporter struct {
	ctx  *build.Context
	fset *token.FileSet
	pkgs map[string]*types.Package // nil value: import in progress
}

func (s *sourceImporter) Import(path string) (*types.Package, error) {
	return s.ImportFrom(path, "", 0)
}

func (s *sourceImporter) ImportFrom(path, dir string, _ types.ImportMode) (*types.Package, error) {
	if path == "unsafe" {
		return types.Unsafe, nil
	}
	bp, err := s.ctx.Import(path, dir, 0)
	if err != nil {
		return nil, err
	}
	if p, ok := s.pkgs[bp.ImportPath]; ok {
		if p == nil {
			return nil, fmt.Errorf("import cycle through %s", bp.ImportPath)
		}
		return p, nil
	}
	s.pkgs[bp.ImportPath] = nil
	files, err := parseGoFiles(s.fset, bp.Dir, bp.GoFiles)
	if err != nil {
		return nil, err
	}
	conf := types.Config{Importer: s, IgnoreFuncBodies: true, Sizes: types.SizesFor("gc", s.ctx.GOARCH)}
	p, err := conf.Check(bp.ImportPath, s.fset, files, nil)
	if err != nil {
		return nil, fmt.Errorf("type-check %s for GOOS=%s: %w", bp.ImportPath, s.ctx.GOOS, err)
	}
	s.pkgs[bp.ImportPath] = p
	return p, nil
}

func parseGoFiles(fset *token.FileSet, dir string, names []string) ([]*ast.File, error) {
	files := make([]*ast.File, 0, len(names))
	for _, name := range names {
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, err
		}
		files = append(files, f)
	}
	return files, nil
}
