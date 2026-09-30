package exec

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	osexec "os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// unsafePointerAllowed is the allowlist of files, relative to the module
// root, that may hold an unsafe.Pointer-typed value or type without importing
// "unsafe". It is empty: nothing in the module does today (forgectl#888), and
// a new entry must name why the file needs a raw pointer it did not spell.
var unsafePointerAllowed = map[string]string{}

// reflectMemoryAllowed is the allowlist of reflect-memory method uses, keyed
// "<file relative to the module root> <method name>", that
// TestNoFileReadsMemoryThroughReflect admits. It is empty: no production
// file calls any of refusedReflectMethods today (forgectl#888), and a new
// entry must name why the file needs a field's address.
var reflectMemoryAllowed = map[string]string{}

// refusedReflectMethods names the methods that hand out an address, or
// dispatch dynamically to one that does, keyed by receiver ("Value" is
// reflect.Value, "Type" is reflect.Type). Addr is allowed on an unexported
// field, UnsafePointer and UnsafeAddr skip the read-only check, and Pointer
// and UnsafeAddr give the same address as a uintptr. Method and MethodByName
// reach every one of them by name at run time, so they would step around the
// static check without naming it (forgectl#888).
var refusedReflectMethods = map[string][]string{
	"Value": {"Addr", "UnsafeAddr", "UnsafePointer", "Pointer", "Method", "MethodByName"},
	"Type":  {"Method", "MethodByName"},
}

// TestNoFileReadsMemoryThroughReflect closes the route
// TestNoFileReachesPastTheTypeSystem cannot see, because that guard reads
// imports and directives and this route needs neither (forgectl#888). reflect
// reads a closure-sealed payload without an "unsafe" import:
// Field(i).Addr() is allowed on an unexported field, UnsafePointer() does not
// check read-only, and a value whose type is already unsafe.Pointer converts
// to *T without the import. A probe from a production file in internal/tmux
// read an Arg's payload that way while every other guard stayed green.
//
// It type-checks, with go/types, every production package of this module
// (every in-tree package `go list -deps ./...` reports, test files excluded)
// on every platform in guardPlatforms, cgo off and on, under pinnedGoEnv. It
// resolves every import from the source go list names. It refuses, per
// typedFindings:
//
//   - any expression or type whose type holds unsafe.Pointer,
//     in a file that does not import "unsafe", outside unsafePointerAllowed.
//     This includes a conversion of such a value to *T, a func value like
//     reflect.NewAt, and a type reached through an alias. A file that does
//     import it is TestNoFileReachesPastTheTypeSystem's to refuse;
//   - any use (a call, a method value or a method expression, promoted or
//     not) of a method in refusedReflectMethods, and of any interface method
//     with the same name and signature, since an interface can hold a
//     reflect.Value. reflectMemoryAllowed is the allowlist.
//
// The rule is an allowlist over what go/types resolves, not a scan of the
// text, so a renamed import, an embedded reflect.Value or a type-parameter
// constraint does not hide a use. TestTypedFindingsSeeEveryRoute pins the
// matcher against each route.
//
// What it does not close: reading raw memory through the operating system,
// such as /proc/self/mem at an address printed with %p, needs no reflect and
// no unsafe. That is a review property.
//
// Mutations that turn it red: a production file in internal/tmux that does
// `p := reflect.ValueOf(&a).Elem().Field(0).Addr().UnsafePointer()` and
// `(**struct{ fn uintptr; s string })(p)` without importing unsafe; a
// production file calling `reflect.ValueOf(&x).Elem().UnsafeAddr()`.
func TestNoFileReadsMemoryThroughReflect(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := osexec.LookPath("go"); err != nil {
		t.Fatalf("the go tool is not on PATH, and it is the only source of the package set: %v", err)
	}
	type result struct {
		p        guardPlatform
		findings []string
		checked  []string
		err      error
	}
	results := make([]*result, len(guardPlatforms))
	var wg sync.WaitGroup
	// Two platforms at a time: each holds the type-checked standard library
	// and every dependency in memory.
	sem := make(chan struct{}, 2)
	for i, p := range guardPlatforms {
		r := &result{p: p}
		results[i] = r
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			r.findings, r.checked, r.err = typedGuardPlatform(t.Context(), root, p)
		})
	}
	wg.Wait()
	byFinding := map[string][]string{}
	for _, r := range results {
		if r.err != nil {
			t.Fatalf("[%s] %v", r.p, r.err)
		}
		if !slices.Contains(r.checked, execImportPath) || !slices.Contains(r.checked, modulePath+"/internal/tmux") {
			t.Fatalf("[%s] type-checked %d packages, internal/exec and internal/tmux not both among them; the package set is broken, not the module clean", r.p, len(r.checked))
		}
		for _, f := range r.findings {
			byFinding[f] = append(byFinding[f], r.p.String())
		}
	}
	keys := make([]string, 0, len(byFinding))
	for f := range byFinding {
		keys = append(keys, f)
	}
	slices.Sort(keys)
	for _, f := range keys {
		t.Errorf("%s [%s]", f, strings.Join(slices.Compact(byFinding[f]), " "))
	}
}

// typedListPackage is the part of `go list -deps -json` output the typed guard
// reads.
type typedListPackage struct {
	ImportPath string
	Dir        string
	Module     *goListModule
	GoFiles    []string
	CgoFiles   []string
	ImportMap  map[string]string
}

// listTypedPackages runs `go list -deps -json ./...` for p with cgo set as
// given, under pinnedGoEnv, and returns every package it reports by import
// path.
func listTypedPackages(ctx context.Context, root string, p guardPlatform, cgo string) (map[string]*typedListPackage, error) {
	cmd := osexec.CommandContext(ctx, "go", "list", "-deps", "-json=ImportPath,Dir,Module,GoFiles,CgoFiles,ImportMap", "./...")
	cmd.Dir = root
	cmd.Env = append(append(os.Environ(), pinnedGoEnv...), "GOOS="+p.goos, "GOARCH="+p.goarch, "CGO_ENABLED="+cgo)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go list for %s cgo=%s: %w\n%s", p, cgo, err, stderr.String())
	}
	pkgs := map[string]*typedListPackage{}
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var pkg typedListPackage
		if err := dec.Decode(&pkg); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, fmt.Errorf("decode go list for %s cgo=%s: %w", p, cgo, err)
		}
		pkgs[pkg.ImportPath] = &pkg
	}
	return pkgs, nil
}

// typedGuardPlatform type-checks every in-tree package for p, cgo off and
// then on, and returns typedFindings over them plus the import paths it
// checked. Out-of-tree packages always come from the cgo-off listing and are
// checked without function bodies: under cgo on, the standard library's cgo
// files would need the C toolchain, and no dependency's exported surface
// differs by cgo in a way a module file could name.
func typedGuardPlatform(ctx context.Context, root string, p guardPlatform) (findings, checked []string, err error) {
	off, err := listTypedPackages(ctx, root, p, "0")
	if err != nil {
		return nil, nil, err
	}
	on, err := listTypedPackages(ctx, root, p, "1")
	if err != nil {
		return nil, nil, err
	}
	fset := token.NewFileSet()
	depPkgs := map[string]*types.Package{}
	seen := map[string]bool{}
	for _, listing := range []map[string]*typedListPackage{off, on} {
		l := &typedLoader{
			root: root, fset: fset, sizes: types.SizesFor("gc", p.goarch),
			deps: off, mod: map[string]*typedListPackage{},
			depPkgs: depPkgs, modPkgs: map[string]*types.Package{},
		}
		for path, lp := range listing {
			if lp.Module.inTree(root) || (lp.Dir != "" && underRoot(root, lp.Dir)) {
				l.mod[path] = lp
			}
		}
		paths := make([]string, 0, len(l.mod))
		for path := range l.mod {
			paths = append(paths, path)
		}
		slices.Sort(paths)
		for _, path := range paths {
			if _, err := l.module(path); err != nil {
				return nil, nil, err
			}
		}
		reflectPkg, err := l.dep("reflect")
		if err != nil {
			return nil, nil, fmt.Errorf("load reflect for %s: %w", p, err)
		}
		refused, err := refusedReflectFuncs(reflectPkg)
		if err != nil {
			return nil, nil, err
		}
		for _, c := range l.checked {
			checked = append(checked, c.path)
			for _, f := range typedFindings(fset, c.files, c.info, refused, l.rel) {
				if !seen[f] {
					seen[f] = true
					findings = append(findings, f)
				}
			}
		}
	}
	return findings, checked, nil
}

// typedLoader type-checks packages from the source go list names, resolving
// each import through the importing package's ImportMap (the standard
// library's vendored packages). In-tree packages are checked with function
// bodies and recorded; every other package is checked without them.
type typedLoader struct {
	root    string
	fset    *token.FileSet
	sizes   types.Sizes
	deps    map[string]*typedListPackage // the cgo-off listing, every package
	mod     map[string]*typedListPackage // in-tree packages under this cgo setting
	depPkgs map[string]*types.Package    // nil value: check in progress
	modPkgs map[string]*types.Package    // nil value: check in progress
	checked []typedCheckedPackage
}

type typedCheckedPackage struct {
	path  string
	files []*ast.File
	info  *types.Info
}

type importerFunc func(path string) (*types.Package, error)

func (f importerFunc) Import(path string) (*types.Package, error) { return f(path) }

func (l *typedLoader) rel(filename string) string {
	rel, err := filepath.Rel(l.root, filename)
	if err != nil {
		return filename
	}
	return filepath.ToSlash(rel)
}

func (l *typedLoader) resolve(from *typedListPackage, path string) (*types.Package, error) {
	if path == "unsafe" {
		return types.Unsafe, nil
	}
	if mapped, ok := from.ImportMap[path]; ok {
		path = mapped
	}
	if _, ok := l.mod[path]; ok {
		return l.module(path)
	}
	return l.dep(path)
}

func (l *typedLoader) dep(path string) (*types.Package, error) {
	if p, ok := l.depPkgs[path]; ok {
		if p == nil {
			return nil, fmt.Errorf("import cycle through %s", path)
		}
		return p, nil
	}
	lp := l.deps[path]
	if lp == nil {
		return nil, fmt.Errorf("%s is not in the cgo-off go list output", path)
	}
	l.depPkgs[path] = nil
	p, _, err := l.check(lp, false)
	if err != nil {
		delete(l.depPkgs, path)
		return nil, err
	}
	l.depPkgs[path] = p
	return p, nil
}

func (l *typedLoader) module(path string) (*types.Package, error) {
	if p, ok := l.modPkgs[path]; ok {
		if p == nil {
			return nil, fmt.Errorf("import cycle through %s", path)
		}
		return p, nil
	}
	lp := l.mod[path]
	if len(lp.CgoFiles) > 0 {
		return nil, fmt.Errorf("%s has cgo files %q, which no guard type-checks; TestNoFileReachesPastTheTypeSystem refuses them", path, lp.CgoFiles)
	}
	l.modPkgs[path] = nil
	p, c, err := l.check(lp, true)
	if err != nil {
		delete(l.modPkgs, path)
		return nil, err
	}
	l.modPkgs[path] = p
	l.checked = append(l.checked, c)
	return p, nil
}

func (l *typedLoader) check(lp *typedListPackage, bodies bool) (*types.Package, typedCheckedPackage, error) {
	files, err := parseGoFiles(l.fset, lp.Dir, lp.GoFiles)
	if err != nil {
		return nil, typedCheckedPackage{}, err
	}
	var info *types.Info
	if bodies {
		info = &types.Info{
			Types: map[ast.Expr]types.TypeAndValue{},
			Uses:  map[*ast.Ident]types.Object{},
		}
	}
	conf := types.Config{
		Importer:         importerFunc(func(path string) (*types.Package, error) { return l.resolve(lp, path) }),
		IgnoreFuncBodies: !bodies,
		Sizes:            l.sizes,
	}
	p, err := conf.Check(lp.ImportPath, l.fset, files, info)
	if err != nil {
		return nil, typedCheckedPackage{}, fmt.Errorf("type-check %s: %w", lp.ImportPath, err)
	}
	return p, typedCheckedPackage{path: lp.ImportPath, files: files, info: info}, nil
}

// refusedReflectFuncs returns the method objects refusedReflectMethods names,
// looked up in reflectPkg. It fails when one is missing: a renamed method
// would otherwise drop out of the rule unseen.
func refusedReflectFuncs(reflectPkg *types.Package) ([]*types.Func, error) {
	var funcs []*types.Func
	for _, recv := range []string{"Value", "Type"} {
		obj, ok := reflectPkg.Scope().Lookup(recv).(*types.TypeName)
		if !ok {
			return nil, fmt.Errorf("reflect declares no type %s", recv)
		}
		for _, name := range refusedReflectMethods[recv] {
			// A *T method set holds T's methods too; an interface's pointer has none.
			T := obj.Type()
			if !types.IsInterface(T) {
				T = types.NewPointer(T)
			}
			m, _, _ := types.LookupFieldOrMethod(T, true, reflectPkg, name)
			fn, ok := m.(*types.Func)
			if !ok {
				return nil, fmt.Errorf("reflect.%s has no method %s", recv, name)
			}
			funcs = append(funcs, fn)
		}
	}
	return funcs, nil
}

// typedFindings is the rule TestNoFileReadsMemoryThroughReflect applies to one
// type-checked package: its files, their types.Info (Types and Uses),
// the refused reflect methods, and rel, which names a file for the
// allowlists and the report.
func typedFindings(fset *token.FileSet, files []*ast.File, info *types.Info, refused []*types.Func, rel func(string) string) []string {
	var findings []string
	seen := map[string]bool{}
	report := func(pos token.Pos, msg string) {
		position := fset.Position(pos)
		f := rel(position.Filename) + ":" + strconv.Itoa(position.Line) + ":" + strconv.Itoa(position.Column) + ": " + msg
		if !seen[f] {
			seen[f] = true
			findings = append(findings, f)
		}
	}
	for _, file := range files {
		name := rel(fset.Position(file.Pos()).Filename)
		importsUnsafe := slices.ContainsFunc(file.Imports, func(imp *ast.ImportSpec) bool {
			p, _ := strconv.Unquote(imp.Path.Value)
			return p == "unsafe"
		})
		checkPointer := !importsUnsafe && unsafePointerAllowed[name] == ""
		ast.Inspect(file, func(n ast.Node) bool {
			if expr, ok := n.(ast.Expr); ok && checkPointer {
				if tv, ok := info.Types[expr]; ok && holdsUnsafePointer(tv.Type, map[types.Type]bool{}) {
					report(expr.Pos(), fmt.Sprintf("%s has type %s, which holds unsafe.Pointer, in a file that does not import \"unsafe\"; converted to *T it reads any memory, a sealed payload included; add the file to unsafePointerAllowed with a reason only after review",
						types.ExprString(expr), tv.Type))
				}
			}
			id, ok := n.(*ast.Ident)
			if !ok {
				return true
			}
			fn, ok := info.Uses[id].(*types.Func)
			if !ok {
				return true
			}
			if match := refusedReflectUse(fn, refused); match != nil && reflectMemoryAllowed[name+" "+fn.Name()] == "" {
				report(id.Pos(), fmt.Sprintf("uses %s, as %s: it hands out a field's address past the read-only check (or reaches such a method by name), which reads a sealed payload without importing \"unsafe\"; add %q to reflectMemoryAllowed with a reason only after review",
					fn.FullName(), match.FullName(), name+" "+fn.Name()))
			}
			return true
		})
	}
	return findings
}

// refusedReflectUse returns the refused reflect method fn stands for: fn
// itself, or, when fn is an interface method (which a reflect.Value or
// reflect.Type can satisfy), the refused method of the same name and an
// identical signature. It returns nil for anything else.
func refusedReflectUse(fn *types.Func, refused []*types.Func) *types.Func {
	fn = fn.Origin()
	if slices.Contains(refused, fn) {
		return fn
	}
	recv := fn.Signature().Recv()
	if recv == nil || !types.IsInterface(recv.Type()) {
		return nil
	}
	for _, r := range refused {
		if r.Name() == fn.Name() && types.Identical(r.Signature(), fn.Signature()) {
			return r
		}
	}
	return nil
}

// holdsUnsafePointer reports whether a value of type t is, or holds in its
// structure, an unsafe.Pointer: the type itself, an element, key, field,
// parameter, result, interface method, type argument or constraint term. A
// named type whose underlying type is a struct or an interface is not
// entered: its unexported fields are its own package's (reflect.Value has an
// unsafe.Pointer field, and holding a reflect.Value is fine), and anything it
// hands out is typed at the use.
func holdsUnsafePointer(t types.Type, seen map[types.Type]bool) bool {
	if t == nil || seen[t] {
		return false
	}
	seen[t] = true
	holds := func(t types.Type) bool { return holdsUnsafePointer(t, seen) }
	tuple := func(tup *types.Tuple) bool {
		for v := range tup.Variables() {
			if holds(v.Type()) {
				return true
			}
		}
		return false
	}
	switch t := types.Unalias(t).(type) {
	case *types.Basic:
		return t.Kind() == types.UnsafePointer
	case *types.Pointer:
		return holds(t.Elem())
	case *types.Slice:
		return holds(t.Elem())
	case *types.Array:
		return holds(t.Elem())
	case *types.Chan:
		return holds(t.Elem())
	case *types.Map:
		return holds(t.Key()) || holds(t.Elem())
	case *types.Signature:
		return tuple(t.Params()) || tuple(t.Results())
	case *types.Tuple:
		return tuple(t)
	case *types.Struct:
		for f := range t.Fields() {
			if holds(f.Type()) {
				return true
			}
		}
		return false
	case *types.Interface:
		for m := range t.Methods() {
			if holds(m.Type()) {
				return true
			}
		}
		for e := range t.EmbeddedTypes() {
			if holds(e) {
				return true
			}
		}
		return false
	case *types.Union:
		for term := range t.Terms() {
			if holds(term.Type()) {
				return true
			}
		}
		return false
	case *types.TypeParam:
		return holds(t.Constraint())
	case *types.Named:
		for arg := range t.TypeArgs().Types() {
			if holds(arg) {
				return true
			}
		}
		switch t.Underlying().(type) {
		case *types.Struct, *types.Interface:
			return false
		}
		return holds(t.Underlying())
	}
	return false
}

// TestTypedFindingsSeeEveryRoute pins typedFindings against each route
// forgectl#888 names and the ways around a narrower rule. Every probe is
// type-checked against the host's reflect. A row that wants findings shows
// the matcher sees that route; a clean row shows ordinary reflect use and an
// unsafe-importing file (TestNoFileReachesPastTheTypeSystem's to refuse) are
// left alone.
//
// Mutations that turn it red: drop the Types walk (the NewAt and SetPointer
// rows go quiet), drop the interface arm of refusedReflectUse (the look-alike
// and constraint rows go quiet), or enter a named struct in
// holdsUnsafePointer (the clean reflect row reports reflect.Value).
func TestTypedFindingsSeeEveryRoute(t *testing.T) {
	const prelude = "package probe\n\nimport \"reflect\"\n\nvar _ reflect.Value\n\ntype sealedArg struct{ reveal func() string }\n\n"
	rows := []struct {
		name, src string
		want      bool
	}{
		{"issue route: Addr, UnsafePointer and a *T conversion", `func f(a *sealedArg) string {
	p := reflect.ValueOf(a).Elem().Field(0).Addr().UnsafePointer()
	return (*(**struct{ fn uintptr; s string })(p)).s
}`, true},
		{"UnsafeAddr call", `func f(x *int) uintptr { return reflect.ValueOf(x).Elem().UnsafeAddr() }`, true},
		{"Pointer call", `func f(x *int) uintptr { return reflect.ValueOf(x).Pointer() }`, true},
		{"Addr method value", `func f(v reflect.Value) func() reflect.Value { return v.Addr }`, true},
		{"UnsafeAddr method expression", `var g = reflect.Value.UnsafeAddr`, true},
		{"pointer-receiver method expression", `var g = (*reflect.Value).Addr`, true},
		{"embedded reflect.Value", `type w struct{ reflect.Value }
func f(x w) reflect.Value { return x.Addr() }`, true},
		{"interface look-alike", `func f(v reflect.Value) uintptr {
	return any(v).(interface{ UnsafeAddr() uintptr }).UnsafeAddr()
}`, true},
		{"type-parameter constraint", `func f[T interface{ UnsafeAddr() uintptr }](v T) uintptr { return v.UnsafeAddr() }`, true},
		{"dynamic MethodByName", `func f(v reflect.Value) reflect.Value { return reflect.ValueOf(v).MethodByName("Addr") }`, true},
		{"reflect.Type MethodByName", `func f(v reflect.Value) (reflect.Method, bool) { return reflect.TypeOf(v).MethodByName("Addr") }`, true},
		{"reflect.NewAt as a value", `var g = reflect.NewAt`, true},
		{"SetPointer method expression", `var g = reflect.Value.SetPointer`, true},
		{"clean reflect use", `func f(a *sealedArg) (reflect.Kind, string, bool) {
	v := reflect.ValueOf(a).Elem().Field(0)
	return v.Kind(), v.Type().String(), reflect.DeepEqual(a, a)
}`, false},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			got := typedProbeFindings(t, prelude+row.src+"\n")
			if row.want && len(got) == 0 {
				t.Errorf("no finding; the matcher misses this route")
			}
			if !row.want && len(got) != 0 {
				t.Errorf("findings on a clean probe:\n%s", strings.Join(got, "\n"))
			}
		})
	}
	t.Run("a file importing unsafe is the import guard's", func(t *testing.T) {
		src := "package probe\n\nimport \"unsafe\"\n\nfunc f(p unsafe.Pointer) *int { return (*int)(p) }\n"
		if got := typedProbeFindings(t, src); len(got) != 0 {
			t.Errorf("findings in an unsafe-importing file, which TestNoFileReachesPastTheTypeSystem refuses instead:\n%s", strings.Join(got, "\n"))
		}
	})
}

// typedProbeFindings type-checks src as one file against the host's standard
// library and returns typedFindings over it.
func typedProbeFindings(t *testing.T, src string) []string {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "probe.go", src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	info := &types.Info{
		Types: map[ast.Expr]types.TypeAndValue{},
		Uses:  map[*ast.Ident]types.Object{},
	}
	imp := importer.ForCompiler(fset, "source", nil)
	conf := types.Config{Importer: imp}
	if _, err := conf.Check("probe", fset, []*ast.File{file}, info); err != nil {
		t.Fatalf("type-check probe: %v\n%s", err, src)
	}
	reflectPkg, err := imp.Import("reflect")
	if err != nil {
		t.Fatal(err)
	}
	refused, err := refusedReflectFuncs(reflectPkg)
	if err != nil {
		t.Fatal(err)
	}
	return typedFindings(fset, []*ast.File{file}, info, refused, func(s string) string { return s })
}
