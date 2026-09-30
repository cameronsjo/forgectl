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

// reflectMemoryAllowed is the allowlist of findings
// TestNoFileReadsMemoryThroughReflect admits, keyed by the key its message
// names: "<file relative to the module root> <name>" for a use of one of
// refusedReflectMethods or refusedMemoryFuncs, "<file> embed <type>" for an
// embedding, and "<file> interface <Type>.<method>" for a declared
// interface (<Type> is the declared name, or the literal's text for an
// interface literal). It is empty: no production file does any of these
// today (forgectl#888, forgectl#897), and a new entry must name why the file
// needs it.
var reflectMemoryAllowed = map[string]string{}

// refusedMemoryFuncs names, by package path, the standard-library functions
// that copy process memory out wholesale (forgectl#897). WriteHeapDump
// writes every heap object, closure captures included, to a file
// descriptor, so every sealed payload with it. SetTraceback("crash") makes
// the next fatal error raise a core dump of the whole address space; the
// other levels are harmless, but the level is a runtime string, so the call
// is refused by name. The GOTRACEBACK environment variable and a debugger
// reach the same dumps with no call at all, which is the operating-system
// residual in startSealed's doc.
var refusedMemoryFuncs = map[string][]string{
	"runtime/debug": {"WriteHeapDump", "SetTraceback"},
}

// refusedMemoryFunc reports whether fn is one of refusedMemoryFuncs: a
// package-level function, matched by package path and name. A function can be
// reached only by naming it, so a Uses walk sees every route (a call, a func
// value, a renamed import).
func refusedMemoryFunc(fn *types.Func) bool {
	return fn.Pkg() != nil && fn.Signature().Recv() == nil &&
		slices.Contains(refusedMemoryFuncs[fn.Pkg().Path()], fn.Name())
}

// refusedReflectMethods names the methods that hand out an address, or
// dispatch dynamically to one that does, keyed by receiver ("Value" is
// reflect.Value, "Type" is reflect.Type). Addr is allowed on an unexported
// field, UnsafePointer and UnsafeAddr skip the read-only check, Pointer and
// UnsafeAddr give the same address as a uintptr, and InterfaceData gives an
// interface's data word as one. Method and MethodByName
// reach every one of them by name at run time, so they would step around the
// static check without naming it (forgectl#888).
var refusedReflectMethods = map[string][]string{
	"Value": {"Addr", "UnsafeAddr", "UnsafePointer", "Pointer", "InterfaceData", "Method", "MethodByName"},
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
//   - any struct field that embeds reflect.Value, *reflect.Value,
//     reflect.Type, or a type that promotes one of refusedReflectMethods
//     (reflectRule.promotes). An adapter embedding reflect.Value plus a
//     method of its own satisfies an interface no reflect type does, and
//     promotion is the only way a refused method reaches a use unnamed;
//   - any interface type declared in the file (a named or generic interface,
//     or an interface literal in any type position) with a method whose name
//     is one of refusedReflectMethods, whatever its signature. Every
//     look-alike needs such an interface, and stdlib interfaces such as
//     net.Listener are not declared in the module, so they are unaffected.
//     The rule matches the method name only, so it also refuses two shapes
//     that carry no reflect value: an interface that embeds a stdlib one
//     with such a method (interface{ net.Listener; Extra() } gains Addr),
//     and a narrowed fake of one (interface{ Addr() net.Addr }). Both are
//     known false positives, cleared per interface with a
//     "<file> interface <Type>.<method>" entry in reflectMemoryAllowed;
//   - any use (a call, a method value or a method expression, promoted or
//     not) of a method in refusedReflectMethods, and of any method of the
//     same name, whatever its signature, on an interface a reflect.Value or
//     reflect.Type could satisfy by name;
//   - any use (a call or a func value) of a function in refusedMemoryFuncs,
//     which copies the heap out wholesale, closure captures included
//     (forgectl#897).
//
// reflectMemoryAllowed is the allowlist for every rule but the first.
//
// The rule is an allowlist over what go/types resolves, not a scan of the
// text, so a renamed import, an embedded reflect.Value or a type-parameter
// constraint does not hide a use. TestTypedFindingsSeeEveryRoute pins the
// matcher against each route.
//
// What it does not close is the residual risk in startSealed's doc
// (sensitive_run.go): memory read through the operating system needs no
// reflect and no unsafe. Nor does it see reflect's plain-data readers
// (Value.String, Bytes, Index and the rest), which read an unexported
// string, slice or map field without the read-only check; that is why a
// payload lives in a closure rather than such a field
// (TestMaskAndOutputHoldNoPlainData, forgectl#897).
//
// Mutations that turn it red: a production file in internal/tmux that does
// `p := reflect.ValueOf(&a).Elem().Field(0).Addr().UnsafePointer()` and
// `(**struct{ fn uintptr; s string })(p)` without importing unsafe; a
// production file calling `reflect.ValueOf(&x).Elem().UnsafeAddr()`; a
// production file calling `debug.WriteHeapDump(f.Fd())`.
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

// typedGuardPlatform type-checks every in-tree package for p with cgo off,
// and again with cgo on when that changes an in-tree file list, and returns
// typedFindings over them plus the import paths it checked. Out-of-tree
// packages always come from the cgo-off listing and are checked without
// function bodies: under cgo on, the standard library's cgo files would need
// the C toolchain, and no dependency's exported surface differs by cgo in a
// way a module file could name.
func typedGuardPlatform(ctx context.Context, root string, p guardPlatform) (findings, checked []string, err error) {
	off, err := listTypedPackages(ctx, root, p, "0")
	if err != nil {
		return nil, nil, err
	}
	on, err := listTypedPackages(ctx, root, p, "1")
	if err != nil {
		return nil, nil, err
	}
	inTree := func(listing map[string]*typedListPackage) map[string]*typedListPackage {
		mod := map[string]*typedListPackage{}
		for path, lp := range listing {
			if lp.Module.inTree(root) || (lp.Dir != "" && underRoot(root, lp.Dir)) {
				mod[path] = lp
			}
		}
		return mod
	}
	passes := []map[string]*typedListPackage{inTree(off)}
	// The cgo-on pass re-checks the in-tree packages only when cgo changes
	// one of their file lists; otherwise it would check the same files twice.
	if modOn := inTree(on); !sameFileSets(passes[0], modOn) {
		passes = append(passes, modOn)
	}
	fset := token.NewFileSet()
	depPkgs := map[string]*types.Package{}
	seen := map[string]bool{}
	for _, mod := range passes {
		l := &typedLoader{
			root: root, fset: fset, sizes: types.SizesFor("gc", p.goarch),
			deps: off, mod: mod,
			depPkgs: depPkgs, modPkgs: map[string]*types.Package{},
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
		rule, err := newReflectRule(reflectPkg)
		if err != nil {
			return nil, nil, err
		}
		for _, c := range l.checked {
			checked = append(checked, c.path)
			for _, f := range typedFindings(fset, c.files, c.info, rule, l.rel) {
				if !seen[f] {
					seen[f] = true
					findings = append(findings, f)
				}
			}
		}
	}
	return findings, checked, nil
}

// sameFileSets reports whether a and b list the same packages with the same
// Go and cgo files.
func sameFileSets(a, b map[string]*typedListPackage) bool {
	if len(a) != len(b) {
		return false
	}
	for path, pa := range a {
		pb, ok := b[path]
		if !ok || !slices.Equal(pa.GoFiles, pb.GoFiles) || !slices.Equal(pa.CgoFiles, pb.CgoFiles) {
			return false
		}
	}
	return true
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

// reflectRule is Rule B's data, looked up in the reflect package the checked
// code resolves: the refused method objects, and the exported method names
// of each type that could sit behind an interface and hand one out (a
// *reflect.Value, whose method set holds Value's too, and a reflect.Type).
type reflectRule struct {
	refused []*types.Func
	names   map[string]bool // the refused methods' names
	holders []map[string]bool
}

// newReflectRule looks up refusedReflectMethods in reflectPkg. It fails when
// one is missing: a renamed method would otherwise drop out of the rule
// unseen.
func newReflectRule(reflectPkg *types.Package) (*reflectRule, error) {
	rule := &reflectRule{names: map[string]bool{}}
	for _, recv := range []string{"Value", "Type"} {
		obj, ok := reflectPkg.Scope().Lookup(recv).(*types.TypeName)
		if !ok {
			return nil, fmt.Errorf("reflect declares no type %s", recv)
		}
		// A *T method set holds T's methods too; an interface's pointer has none.
		T := obj.Type()
		if !types.IsInterface(T) {
			T = types.NewPointer(T)
		}
		names := map[string]bool{}
		for sel := range types.NewMethodSet(T).Methods() {
			if sel.Obj().Exported() {
				names[sel.Obj().Name()] = true
			}
		}
		rule.holders = append(rule.holders, names)
		for _, name := range refusedReflectMethods[recv] {
			m, _, _ := types.LookupFieldOrMethod(T, true, reflectPkg, name)
			fn, ok := m.(*types.Func)
			if !ok {
				return nil, fmt.Errorf("reflect.%s has no method %s", recv, name)
			}
			rule.refused = append(rule.refused, fn)
			rule.names[name] = true
		}
	}
	return rule, nil
}

// typedFindings is the rule TestNoFileReadsMemoryThroughReflect applies to one
// type-checked package: its files, their types.Info (Types and Uses),
// Rule B's reflect data, and rel, which names a file for the
// allowlists and the report.
func typedFindings(fset *token.FileSet, files []*ast.File, info *types.Info, rule *reflectRule, rel func(string) string) []string {
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
		// declared names each interface type written as a type declaration's
		// right-hand side; ast.Inspect visits the TypeSpec before its type.
		declared := map[*ast.InterfaceType]string{}
		ast.Inspect(file, func(n ast.Node) bool {
			if ts, ok := n.(*ast.TypeSpec); ok {
				if it, ok := ts.Type.(*ast.InterfaceType); ok {
					declared[it] = ts.Name.Name
				}
			}
			if expr, ok := n.(ast.Expr); ok && checkPointer {
				if tv, ok := info.Types[expr]; ok && holdsUnsafePointer(tv.Type, map[types.Type]bool{}) {
					report(expr.Pos(), fmt.Sprintf("%s has type %s, which holds unsafe.Pointer, in a file that does not import \"unsafe\"; converted to *T it reads any memory, a sealed payload included; add the file to unsafePointerAllowed with a reason only after review",
						types.ExprString(expr), tv.Type))
				}
			}
			if st, ok := n.(*ast.StructType); ok {
				for _, field := range st.Fields.List {
					if len(field.Names) != 0 {
						continue
					}
					key := name + " embed " + types.ExprString(field.Type)
					if via := rule.promotes(info.Types[field.Type].Type); via != nil && reflectMemoryAllowed[key] == "" {
						report(field.Pos(), fmt.Sprintf("embeds %s, which promotes %s: an adapter type can then satisfy an interface no reflect type does and reach it without naming it; add %q to reflectMemoryAllowed with a reason only after review",
							types.ExprString(field.Type), via.FullName(), key))
					}
				}
			}
			if it, ok := n.(*ast.InterfaceType); ok {
				if iface, ok := info.Types[it].Type.(*types.Interface); ok {
					typeName, ok := declared[it]
					if !ok {
						typeName = types.ExprString(it)
					}
					for m := range iface.Methods() {
						key := name + " interface " + typeName + "." + m.Name()
						if rule.names[m.Name()] && reflectMemoryAllowed[key] == "" {
							report(it.Pos(), fmt.Sprintf("declares interface %s with method %s, a name reflect.Value or reflect.Type uses to hand out an address; any such interface, whatever the signature, can carry a reflect value (or an adapter around one) to a call site that never names reflect; add %q to reflectMemoryAllowed with a reason only after review",
								typeName, m.Name(), key))
						}
					}
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
			if refusedMemoryFunc(fn) && reflectMemoryAllowed[name+" "+fn.Name()] == "" {
				report(id.Pos(), fmt.Sprintf("uses %s, which copies process memory out wholesale, every closure-sealed payload included; add %q to reflectMemoryAllowed with a reason only after review",
					fn.FullName(), name+" "+fn.Name()))
			}
			if match := rule.use(fn); match != nil && reflectMemoryAllowed[name+" "+fn.Name()] == "" {
				report(id.Pos(), fmt.Sprintf("uses %s, as %s: it hands out a field's address past the read-only check (or reaches such a method by name), which reads a sealed payload without importing \"unsafe\"; add %q to reflectMemoryAllowed with a reason only after review",
					fn.FullName(), match.FullName(), name+" "+fn.Name()))
			}
			return true
		})
	}
	return findings
}

// promotes returns a refused reflect method that embedding a field of type t
// promotes into the embedding struct, or nil. That covers reflect.Value,
// *reflect.Value and reflect.Type themselves, and any type that embeds one of
// them in turn, and an embedded interface whose promoted method use would
// refuse (interface{ UnsafeAddr() uintptr }). Promotion is the only way a
// refused method reaches a call site without being named: a hand-written
// forwarder names it and is caught at that use, but a struct embedding
// reflect.Value plus a method of its own satisfies an interface no reflect
// type does, which the holder filter in use lets through. So the embedding
// itself is refused.
func (r *reflectRule) promotes(t types.Type) *types.Func {
	if t == nil {
		return nil
	}
	for _, refused := range r.refused {
		obj, _, _ := types.LookupFieldOrMethod(t, true, nil, refused.Name())
		if fn, ok := obj.(*types.Func); ok {
			if match := r.use(fn); match != nil {
				return match
			}
		}
	}
	return nil
}

// use returns the refused reflect method fn stands for: fn
// itself (its origin, for an instantiated method), or, when fn is a method
// of an interface a reflect.Value or reflect.Type could satisfy, a refused
// method of the same name, whatever the signature. Inside a generic body a
// look-alike's signature can stay parametric (interface{ UnsafeAddr() T }),
// so matching signatures would miss it. Whether a reflect type could satisfy
// the interface is decided by name alone: every method of the interface must
// be an exported method of *reflect.Value, or every one of reflect.Type.
// net.Listener's Addr is left alone because no reflect type has Accept or
// Close. It returns nil for anything else.
func (r *reflectRule) use(fn *types.Func) *types.Func {
	if slices.Contains(r.refused, fn.Origin()) {
		return fn.Origin()
	}
	recv := fn.Signature().Recv()
	if recv == nil {
		return nil
	}
	iface, ok := recv.Type().Underlying().(*types.Interface)
	if !ok || !slices.ContainsFunc(r.holders, func(names map[string]bool) bool {
		for m := range iface.Methods() {
			if !names[m.Name()] {
				return false
			}
		}
		return true
	}) {
		return nil
	}
	for _, refused := range r.refused {
		if refused.Name() == fn.Name() {
			return refused
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
// The declaration-site rule and use's interface arm overlap on purpose: any
// look-alike declared in the module is refused where it is declared, and
// use's arm still covers an interface declared elsewhere. So the arm's
// mutations show only with the declaration rule off.
//
// Mutations that turn it red: drop the Types walk (the NewAt and SetPointer
// rows go quiet); drop the declaration-site rule (the interface-literal row
// goes quiet); with it off, drop use's interface arm (the look-alike and
// constraint rows go quiet), match that arm on an identical signature as
// well as the name (the three parametric rows go quiet), or check promotes
// by identity instead of through use (the three embedded-interface rows go
// quiet); drop the reflect-holder filter in use (net.Listener's Addr is
// reported); drop the embedding check (the embedded reflect.Type row goes
// quiet, and with the declaration rule off the other four adapter rows); or
// enter a named struct in holdsUnsafePointer (the clean reflect row reports
// reflect.Value); drop the refusedMemoryFuncs check, or WriteHeapDump from
// that list (the runtime/debug rows go quiet; a name the standard library no
// longer declares fails newTypedProbe); key the interface allowlist on the
// method alone, or on the literal text for a declared interface too (the
// allowlist row fails).
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
		{"generic interface look-alike", `type I[T any] interface{ UnsafeAddr() T }
func f(v reflect.Value) uintptr { return any(v).(I[uintptr]).UnsafeAddr() }`, true},
		{"generic interface as a constraint", `type I[T any] interface{ UnsafeAddr() T }
func f[V I[uintptr]](v V) uintptr { return v.UnsafeAddr() }`, true},
		{"parametric look-alike in a generic body", `type I[T any] interface{ UnsafeAddr() T }
func g[T any](x I[T]) T { return x.UnsafeAddr() }`, true},
		{"parametric look-alike as a constraint", `type I[T any] interface{ UnsafeAddr() T }
func g[T any, V I[T]](v V) T { return v.UnsafeAddr() }`, true},
		{"parametric look-alike method value", `type I[T any] interface{ UnsafeAddr() T }
func g[T any](x I[T]) func() T { return x.UnsafeAddr }`, true},
		{"InterfaceData call", `func f(v reflect.Value) [2]uintptr { return v.InterfaceData() }`, true},
		{"dynamic MethodByName", `func f(v reflect.Value) reflect.Value { return reflect.ValueOf(v).MethodByName("Addr") }`, true},
		{"reflect.Type MethodByName", `func f(v reflect.Value) (reflect.Method, bool) { return reflect.TypeOf(v).MethodByName("Addr") }`, true},
		{"reflect.NewAt as a value", `var g = reflect.NewAt`, true},
		{"SetPointer method expression", `var g = reflect.Value.SetPointer`, true},
		{"adapter embedding reflect.Value", `type ad struct{ reflect.Value }
func (ad) Extra() {}
func f(v reflect.Value) uintptr {
	return any(ad{v}).(interface{ UnsafeAddr() uintptr; Extra() }).UnsafeAddr()
}`, true},
		{"adapter behind a parametric interface", `type ad struct{ reflect.Value }
func (ad) Extra() {}
type I[T any] interface{ UnsafeAddr() T; Extra() }
func g[T any](x I[T]) T { return x.UnsafeAddr() }
var _ = g[uintptr](ad{})`, true},
		{"adapter reaching MethodByName", `type ad struct{ reflect.Value }
func (ad) Extra() {}
func f(v reflect.Value) reflect.Value {
	return any(ad{v}).(interface{ MethodByName(string) reflect.Value; Extra() }).MethodByName("Addr")
}`, true},
		{"adapter embedding *reflect.Value", `type ad struct{ *reflect.Value }
func (ad) Extra() {}
func f(v *reflect.Value) uintptr {
	return any(ad{v}).(interface{ Pointer() uintptr; Extra() }).Pointer()
}`, true},
		{"adapter embedding reflect.Type", `type ad struct{ reflect.Type }
func (ad) Extra() {}
var _ = ad{}`, true},
		{"embedded module interface carrying UnsafeAddr", `type U interface{ UnsafeAddr() uintptr }
type ad struct{ U }
func (ad) Extra() {}
type J interface{ UnsafeAddr() uintptr; Extra() }
func f(v reflect.Value) uintptr { return J(ad{v}).UnsafeAddr() }`, true},
		{"embedded generic interface carrying UnsafeAddr", `type U[T any] interface{ UnsafeAddr() T }
type ad struct{ U[uintptr] }
func (ad) Extra() {}
type J interface{ UnsafeAddr() uintptr; Extra() }
func f(v reflect.Value) uintptr { return J(ad{v}).UnsafeAddr() }`, true},
		{"embedded interface carrying MethodByName", `type U interface{ MethodByName(string) reflect.Value }
type ad struct{ U }
func (ad) Extra() {}
type J interface{ MethodByName(string) reflect.Value; Extra() }
func f(v reflect.Value) reflect.Value { return J(ad{v}).MethodByName("Addr") }`, true},
		{"interface literal in a parameter type", `func f(x interface{ Pointer() int; Extra() }) {}`, true},
		{"unrelated method on a generic interface", `type J[T any] interface{ Size() T }
func g[T any](x J[T]) T { return x.Size() }`, false},
		{"clean reflect use", `func f(a *sealedArg) (reflect.Kind, string, bool) {
	v := reflect.ValueOf(a).Elem().Field(0)
	return v.Kind(), v.Type().String(), reflect.DeepEqual(a, a)
}`, false},
	}
	probe := newTypedProbe(t)
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			got := probe.findings(t, prelude+row.src+"\n")
			if row.want && len(got) == 0 {
				t.Errorf("no finding; the matcher misses this route")
			}
			if !row.want && len(got) != 0 {
				t.Errorf("findings on a clean probe:\n%s", strings.Join(got, "\n"))
			}
		})
	}
	t.Run("net.Listener's Addr stays clean", func(t *testing.T) {
		// A stdlib interface is not declared in the module, so the
		// declaration-site rule leaves it alone, and no reflect type has
		// Accept or Close, so use does too.
		src := "package probe\n\nimport \"net\"\n\nfunc f(l net.Listener) string { return l.Addr().String() }\n"
		if got := probe.findings(t, src); len(got) != 0 {
			t.Errorf("findings on net.Listener's Addr:\n%s", strings.Join(got, "\n"))
		}
	})
	t.Run("runtime/debug's memory dumps", func(t *testing.T) {
		const head = "package probe\n\nimport \"runtime/debug\"\n\nvar _ = debug.ReadBuildInfo\n\n"
		for _, row := range []struct{ name, src string }{
			{"WriteHeapDump call", "func f(fd uintptr) { debug.WriteHeapDump(fd) }"},
			{"WriteHeapDump func value", "var g = debug.WriteHeapDump"},
			{"SetTraceback call", `func f() { debug.SetTraceback("crash") }`},
		} {
			if got := probe.findings(t, head+row.src+"\n"); len(got) == 0 {
				t.Errorf("%s: no finding; the matcher misses this route", row.name)
			}
		}
		renamed := "package probe\n\nimport d \"runtime/debug\"\n\nfunc f(fd uintptr) { d.WriteHeapDump(fd) }\n"
		if got := probe.findings(t, renamed); len(got) == 0 {
			t.Error("WriteHeapDump through a renamed import: no finding; the matcher misses this route")
		}
		clean := head + "func f() (string, bool) { bi, ok := debug.ReadBuildInfo(); return bi.GoVersion, ok }\n"
		if got := probe.findings(t, clean); len(got) != 0 {
			t.Errorf("findings on runtime/debug.ReadBuildInfo:\n%s", strings.Join(got, "\n"))
		}
	})
	t.Run("the interface allowlist is keyed on the interface type", func(t *testing.T) {
		// The two known false-positive shapes of the declaration-site rule
		// (typedFindings' doc) are refused, and one entry clears one
		// interface: I's entry leaves J, in the same file with the same
		// method, refused.
		src := "package probe\n\nimport \"net\"\n\n" +
			"type I interface{ net.Listener; Extra() }\n\n" +
			"type J interface{ Addr() net.Addr }\n"
		got := probe.findings(t, src)
		for _, want := range []string{"interface I with method Addr", "interface J with method Addr"} {
			if !slices.ContainsFunc(got, func(f string) bool { return strings.Contains(f, want) }) {
				t.Errorf("no finding naming %q; the declaration-site rule misses a documented shape:\n%s", want, strings.Join(got, "\n"))
			}
		}
		reflectMemoryAllowed["probe.go interface I.Addr"] = "probe: the allowlist key's shape"
		defer delete(reflectMemoryAllowed, "probe.go interface I.Addr")
		got = probe.findings(t, src)
		if slices.ContainsFunc(got, func(f string) bool { return strings.Contains(f, "interface I with") }) {
			t.Errorf("an entry keyed \"probe.go interface I.Addr\" does not clear I:\n%s", strings.Join(got, "\n"))
		}
		if !slices.ContainsFunc(got, func(f string) bool { return strings.Contains(f, "interface J with method Addr") }) {
			t.Errorf("I's entry cleared J too; the key is not on the interface type:\n%s", strings.Join(got, "\n"))
		}
		lit := "package probe\n\nfunc f(x interface{ Pointer() int }) {}\n"
		reflectMemoryAllowed["probe.go interface interface{Pointer() int}.Pointer"] = "probe: a literal's key"
		defer delete(reflectMemoryAllowed, "probe.go interface interface{Pointer() int}.Pointer")
		if got := probe.findings(t, lit); len(got) != 0 {
			t.Errorf("an interface literal's entry, keyed on its text, does not clear it:\n%s", strings.Join(got, "\n"))
		}
	})
	t.Run("a file importing unsafe is the import guard's", func(t *testing.T) {
		src := "package probe\n\nimport \"unsafe\"\n\nfunc f(p unsafe.Pointer) *int { return (*int)(p) }\n"
		if got := probe.findings(t, src); len(got) != 0 {
			t.Errorf("findings in an unsafe-importing file, which TestNoFileReachesPastTheTypeSystem refuses instead:\n%s", strings.Join(got, "\n"))
		}
	})
}

// typedProbe type-checks probe sources against the host's standard library,
// through one source importer so reflect is loaded once for every row.
type typedProbe struct {
	fset *token.FileSet
	imp  types.Importer
	rule *reflectRule
}

func newTypedProbe(t *testing.T) *typedProbe {
	t.Helper()
	fset := token.NewFileSet()
	imp := importer.ForCompiler(fset, "source", nil)
	reflectPkg, err := imp.Import("reflect")
	if err != nil {
		t.Fatal(err)
	}
	rule, err := newReflectRule(reflectPkg)
	if err != nil {
		t.Fatal(err)
	}
	// A renamed or removed memory-dump function would drop out of the rule
	// unseen, the way newReflectRule refuses a missing reflect method.
	for path, names := range refusedMemoryFuncs {
		pkg, err := imp.Import(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range names {
			if fn, ok := pkg.Scope().Lookup(name).(*types.Func); !ok || !refusedMemoryFunc(fn) {
				t.Fatalf("%s declares no func %s; refusedMemoryFuncs would refuse nothing", path, name)
			}
		}
	}
	return &typedProbe{fset: fset, imp: imp, rule: rule}
}

// findings type-checks src as one file and returns typedFindings over it.
func (p *typedProbe) findings(t *testing.T, src string) []string {
	t.Helper()
	file, err := parser.ParseFile(p.fset, "probe.go", src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	info := &types.Info{
		Types: map[ast.Expr]types.TypeAndValue{},
		Uses:  map[*ast.Ident]types.Object{},
	}
	conf := types.Config{Importer: p.imp}
	if _, err := conf.Check("probe", p.fset, []*ast.File{file}, info); err != nil {
		t.Fatalf("type-check probe: %v\n%s", err, src)
	}
	return typedFindings(p.fset, []*ast.File{file}, info, p.rule, func(s string) string { return s })
}
