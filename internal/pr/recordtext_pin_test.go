package pr

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
)

// recordTextFields are the record and report fields that carry free text:
// every value written to one is escaped and capped at the source (#934,
// #963). The breadcrumb's own fields need breadcrumbText, whose byte cap is
// what keeps the record inside maxBreadcrumbRecordBytes; the report fields
// take either capper.
var recordTextFields = map[string][]string{
	"Error":        {"recordText", "breadcrumbText"},
	"Refusal":      {"recordText", "breadcrumbText"},
	"LastError":    {"breadcrumbText"},
	"RepairReason": {"breadcrumbText"},
}

// uncappedRecordTextAllowlist is every function that may write a pinned
// field uncapped, keyed "file.go:Func", with the exact number of such writes
// and why none reaches a record, a report or a terminal.
var uncappedRecordTextAllowlist = map[string]struct {
	writes int
	reason string
}{
	"record.go:workspaceRoom": {2, "measures a record with both free-text fields at breadcrumbTextMaxBytes of encoded JSON; " +
		"the record is encoded to count its bytes and is never written"},
}

// TestRecordTextFieldsAreCappedAtTheSource is #963 B's pin: every write to
// Error, Refusal, LastError or RepairReason in the package — an assignment,
// a keyed or unkeyed composite-literal element — is capped at the source
// (recordTextWrites says what counts). The sites #963 named (the drain claim
// refusal and pr repair's four item.Error writes) reached --json raw because
// nothing checked this.
//
// Mutations that turn it red: set report.Refusal = err.Error() in
// claimQueued (drain.go); set item.Error = safeErrString(err) in any of
// repair.go's four failure arms, or route it through a local
// (x := safeErrString(err); item.Error = x), even one named lastError; set
// the exhausted-attempts RepairReason in settleDrainFailure with a bare
// fmt.Sprintf; write bc.RepairReason with recordText in
// markNeedsRepairLocked (the rune cap without the byte cap); pass
// cause.Error() as recordParkedAttempt's lastError.
func TestRecordTextFieldsAreCappedAtTheSource(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Clean(name), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	uncapped, checked := recordTextWrites(fset, files)
	counts := map[string]int{}
	for _, u := range uncapped {
		if _, ok := uncappedRecordTextAllowlist[u.fn]; ok {
			counts[u.fn]++
			continue
		}
		t.Error(u)
	}
	for fn, entry := range uncappedRecordTextAllowlist {
		if strings.TrimSpace(entry.reason) == "" {
			t.Errorf("allowlist entry %s carries no reason", fn)
		}
		if counts[fn] != entry.writes {
			t.Errorf("%s writes a pinned field uncapped %d time(s), allowlisted for %d (%s); a new write needs a capper",
				fn, counts[fn], entry.writes, entry.reason)
		}
	}
	// drain.go alone holds a Refusal, three LastError and three RepairReason
	// writes; far fewer means the walk is broken, not the package clean.
	if checked < 20 {
		t.Fatalf("checked %d writes; the walk found too few to mean anything", checked)
	}
}

// TestRecordTextWritesResolvesThroughTypes is the pin's own control, over a
// synthetic package: a value is capped by what it resolves to, not by the
// name it goes by.
//
// Mutations that turn it red: accept any identifier (the old name-keyed
// allowlist) in capped, and the laundered local is missed; skip an
// AssignStmt whose sides differ in length, and the tuple write is missed;
// skip a composite literal element with no key, and the unkeyed one is
// missed; accept a parameter without reading its call sites, and the
// uncapped caller is missed; match a capper call by name alone, and the
// shadowing local is missed; drop the escaped-function check, and the
// parameter of a function used as a value is missed; key the verdict
// cache by variable alone, and msg, capped for Error, reads as capped for
// LastError; drop the interface-dispatch check, and impl.put's parameter
// is vouched for by its one direct caller; cache an in-cycle verdict, and b
// reads as capped after a's root write turned out uncapped.
func TestRecordTextWritesResolvesThroughTypes(t *testing.T) {
	const src = `package p

import "errors"

type item struct {
	Ref   string
	Error string
}

const fixed = "a constant"

func recordText(s string) string { return s }
func safeErrString(err error) string { return err.Error() }
func two() (string, string) { return "", "" }

func ok(it *item, err error) {
	it.Error = recordText(err.Error())
	it.Error = "literal"
	it.Error = fixed
	x := recordText(err.Error())
	it.Error = x
	_ = item{Ref: "r", Error: recordText("e")}
	_ = item{"r", recordText("e")}
	viaParam(it, recordText("p"))
	var y string
	it.Error = y
	_ = errors.New
}

func viaParam(it *item, lastError string) { it.Error = lastError }

func laundered(it *item, err error) {
	x := safeErrString(err)
	it.Error = x // BAD laundered
}

func sameName(it *item, err error) {
	lastError := safeErrString(err)
	it.Error = lastError // BAD same-name
}

func reassigned(it *item, err error) {
	x := recordText("ok")
	x = err.Error()
	it.Error = x // BAD reassigned
}

func tuple(it *item) {
	var r string
	r, it.Error = two() // BAD tuple
	_ = r
}

func unkeyed(err error) item { return item{"r", err.Error()} } // BAD unkeyed

func uncappedCaller(it *item, err error) { paramSink(it, err.Error()) }

func paramSink(it *item, why string) { it.Error = why } // BAD caller

func shadow(it *item, err error) {
	recordText := func(s string) string { return s }
	it.Error = recordText(err.Error()) // BAD shadow
}

func escapedFunc(it *item) { paramEscapes(it, "lit"); f := paramEscapes; _ = f }

func paramEscapes(it *item, why string) { it.Error = why } // BAD escapes

func closure(it *item) { func(why string) { it.Error = why }("lit") } // BAD closure-param

type rec struct {
	Error     string
	LastError string
}

func capperSet(r *rec) {
	msg := recordText("x")
	r.Error = msg
	r.LastError = msg // BAD capper-set
}

type sink interface{ put(it *item, why string) }

type impl struct{}

func (impl) put(it *item, why string) { it.Error = why } // BAD interface

func viaIface(s sink, it *item, err error) { s.put(it, err.Error()) }

func direct(it *item) { impl{}.put(it, "lit") }

func cycle(it *item, err error) {
	a := recordText("x")
	b := a
	a = b
	a = err.Error()
	it.Error = a // BAD cycle-root
	it.Error = b // BAD cycle-member
}
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "p.go", src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	uncapped, _ := recordTextWrites(fset, []*ast.File{f})
	got := map[int]bool{}
	for _, u := range uncapped {
		got[u.pos.Line] = true
	}
	want := map[int]string{}
	for i, line := range strings.Split(src, "\n") {
		if _, tag, ok := strings.Cut(line, "// BAD "); ok {
			want[i+1] = tag
		}
	}
	for line, tag := range want {
		if !got[line] {
			t.Errorf("%s (line %d) was not reported; findings: %q", tag, line, uncapped)
		}
		delete(got, line)
	}
	for line := range got {
		t.Errorf("line %d reported, but it is capped; findings: %q", line, uncapped)
	}
}

// recordTextFinding is one write of a pinned field that is not capped, with
// its enclosing function as "file.go:Func".
type recordTextFinding struct {
	pos     token.Position
	fn      string
	field   string
	cappers []string
}

func (f recordTextFinding) String() string {
	return fmt.Sprintf("%s (%s): %s is assigned without %s; escape and cap it at the source",
		f.pos, f.fn, f.field, strings.Join(f.cappers, " or "))
}

// recordTextWrites type-checks files as one package and returns every write
// to a recordTextFields field whose value is not capped at the source, with
// the number of writes it checked. A value is
// capped when it is:
//
//   - a string literal or a constant;
//   - a call to the package-level function the field takes (recordText or
//     breadcrumbText), resolved through go/types so a local of that name does
//     not count;
//   - a variable every write to which is capped, a parameter included, whose
//     writes are the arguments at every call site of its function (a function
//     used other than by a call has callers the pin cannot see, so its
//     parameters are not capped); a variable declared with no value holds
//     "".
//
// A tuple assignment (a, rec.Error = f()) writes a field from a multi-value
// call, which no capper returns, so it is never capped.
//
// Every import is an empty stand-in and the resulting type errors are
// ignored: the pin needs only this package's own objects resolved, and a
// stand-in keeps it from type-checking the dependency graph.
func recordTextWrites(fset *token.FileSet, files []*ast.File) (uncapped []recordTextFinding, checked int) {
	info := &types.Info{
		Types: map[ast.Expr]types.TypeAndValue{},
		Defs:  map[*ast.Ident]types.Object{},
		Uses:  map[*ast.Ident]types.Object{},
	}
	conf := types.Config{Importer: stubImporter{}, Error: func(error) {}}
	pkg, _ := conf.Check("pin", fset, files, info)
	if pkg == nil {
		return []recordTextFinding{{fn: "type-check produced no package"}}, 0
	}

	r := &capResolver{info: info, pkg: pkg, writes: map[*types.Var][]ast.Expr{}, params: map[*types.Var]paramSite{}, calls: map[*types.Func][]*ast.CallExpr{}, escaped: map[*types.Func]bool{}, ifaceMethods: map[string]bool{"Error": true}, state: map[stateKey]int{}}
	r.index(files)

	fn := ""
	report := func(pos token.Pos, field string, cappers []string) {
		p := fset.Position(pos)
		uncapped = append(uncapped, recordTextFinding{p, filepath.Base(p.Filename) + ":" + fn, field, cappers})
	}
	check := func(field string, value ast.Expr) {
		cappers, ok := recordTextFields[field]
		if !ok {
			return
		}
		checked++
		if !r.capped(value, cappers) {
			report(value.Pos(), field, cappers)
		}
	}
	for _, f := range files {
		for _, decl := range f.Decls {
			fn = pinDeclName(decl)
			inspectRecordTextWrites(decl, info, check, report)
		}
	}
	return uncapped, checked
}

// pinDeclName names a top-level declaration: a function by its name, a
// method as Recv.Method, anything else as "var".
func pinDeclName(decl ast.Decl) string {
	fd, ok := decl.(*ast.FuncDecl)
	if !ok {
		return "var"
	}
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return fd.Name.Name
	}
	recv := fd.Recv.List[0].Type
	if star, ok := recv.(*ast.StarExpr); ok {
		recv = star.X
	}
	if id, ok := recv.(*ast.Ident); ok {
		return id.Name + "." + fd.Name.Name
	}
	return fd.Name.Name
}

// inspectRecordTextWrites hands every pinned-field write in decl to check,
// and every tuple write, which no capper can make, to report.
func inspectRecordTextWrites(decl ast.Decl, info *types.Info, check func(string, ast.Expr), report func(token.Pos, string, []string)) {
	ast.Inspect(decl, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.AssignStmt:
			for i, lhs := range n.Lhs {
				sel, ok := ast.Unparen(lhs).(*ast.SelectorExpr)
				if !ok {
					continue
				}
				if len(n.Lhs) != len(n.Rhs) {
					if cappers, ok := recordTextFields[sel.Sel.Name]; ok {
						report(sel.Pos(), sel.Sel.Name, cappers)
					}
					continue
				}
				check(sel.Sel.Name, n.Rhs[i])
			}
		case *ast.CompositeLit:
			st, _ := typeUnder(info, n).(*types.Struct)
			for i, elt := range n.Elts {
				if kv, ok := elt.(*ast.KeyValueExpr); ok {
					if key, ok := kv.Key.(*ast.Ident); ok {
						check(key.Name, kv.Value)
					}
					continue
				}
				if st != nil && i < st.NumFields() {
					check(st.Field(i).Name(), elt)
				}
			}
		}
		return true
	})
}

// typeUnder is the underlying type of a composite literal, through a pointer
// for an elided &T{} element, or nil when it did not resolve.
func typeUnder(info *types.Info, lit *ast.CompositeLit) types.Type {
	tv, ok := info.Types[lit]
	if !ok || tv.Type == nil {
		return nil
	}
	t := tv.Type.Underlying()
	if p, ok := t.(*types.Pointer); ok {
		t = p.Elem().Underlying()
	}
	return t
}

// paramSite is where a parameter sits: its function and its index.
type paramSite struct {
	fn    *types.Func
	index int
}

// capResolver answers whether a value is capped, following variables to
// their writes and parameters to their call sites.
type capResolver struct {
	info    *types.Info
	pkg     *types.Package
	writes  map[*types.Var][]ast.Expr // nil entry: a write no capper can make
	params  map[*types.Var]paramSite
	calls   map[*types.Func][]*ast.CallExpr
	escaped map[*types.Func]bool
	// ifaceMethods are the method names some interface the pin can see
	// declares, or some call dispatches through an interface: a method of
	// that name can be reached by a call that names the interface's method,
	// not it, so its call sites are not all in calls.
	ifaceMethods map[string]bool
	// state is the settled verdict for a variable under one capper set
	// (stateKey): a value capped for Error by recordText is not capped for
	// LastError, which takes breadcrumbText alone.
	state map[stateKey]int // 1 resolving, 2 capped, 3 not
	// tentative counts the in-cycle verdicts assumed so far; a capped
	// verdict that rests on one is not settled, so it is not cached.
	tentative int
}

// stateKey is a variable and the capper set it is resolved against.
type stateKey struct {
	v       *types.Var
	cappers string
}

func (r *capResolver) index(files []*ast.File) {
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.FuncDecl:
				fn, _ := r.info.Defs[n.Name].(*types.Func)
				if fn == nil {
					return true
				}
				i := 0
				for _, field := range n.Type.Params.List {
					_, variadic := field.Type.(*ast.Ellipsis)
					for _, name := range field.Names {
						if v, ok := r.info.Defs[name].(*types.Var); ok && !variadic {
							r.params[v] = paramSite{fn, i}
						}
						i++
					}
					if len(field.Names) == 0 {
						i++
					}
				}
			case *ast.AssignStmt:
				for i, lhs := range n.Lhs {
					v := r.varOf(lhs)
					if v == nil {
						continue
					}
					if len(n.Lhs) != len(n.Rhs) || (n.Tok != token.ASSIGN && n.Tok != token.DEFINE) {
						r.writes[v] = append(r.writes[v], nil)
						continue
					}
					r.writes[v] = append(r.writes[v], n.Rhs[i])
				}
			case *ast.ValueSpec:
				for i, name := range n.Names {
					v, ok := r.info.Defs[name].(*types.Var)
					if !ok {
						continue
					}
					switch {
					case len(n.Values) == 0:
						r.writes[v] = append(r.writes[v], &ast.BasicLit{Kind: token.STRING, Value: `""`})
					case len(n.Values) != len(n.Names):
						r.writes[v] = append(r.writes[v], nil)
					default:
						r.writes[v] = append(r.writes[v], n.Values[i])
					}
				}
			case *ast.RangeStmt:
				for _, e := range []ast.Expr{n.Key, n.Value} {
					if v := r.varOf(e); v != nil {
						r.writes[v] = append(r.writes[v], nil)
					}
				}
			case *ast.UnaryExpr:
				// &x hands the variable to code that can write anything.
				if v := r.varOf(n.X); v != nil && n.Op == token.AND {
					r.writes[v] = append(r.writes[v], nil)
				}
			case *ast.CallExpr:
				if fn := r.funcOf(n.Fun); fn != nil {
					r.calls[fn] = append(r.calls[fn], n)
					if recv := fn.Signature().Recv(); recv != nil && types.IsInterface(recv.Type()) {
						r.ifaceMethods[fn.Name()] = true
					}
				}
			case *ast.InterfaceType:
				if it, ok := r.info.Types[n].Type.(*types.Interface); ok {
					for i := range it.NumMethods() {
						r.ifaceMethods[it.Method(i).Name()] = true
					}
				}
			}
			return true
		})
		// A function named anywhere but a call's Fun is used as a value.
		callees := map[*ast.Ident]bool{}
		ast.Inspect(f, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if id := calleeIdentOf(call.Fun); id != nil {
					callees[id] = true
				}
			}
			return true
		})
		ast.Inspect(f, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && !callees[id] {
				if fn, ok := r.info.Uses[id].(*types.Func); ok {
					r.escaped[fn] = true
				}
			}
			return true
		})
	}
}

func calleeIdentOf(fun ast.Expr) *ast.Ident {
	switch f := ast.Unparen(fun).(type) {
	case *ast.Ident:
		return f
	case *ast.SelectorExpr:
		return f.Sel
	}
	return nil
}

// varOf is the variable e names, when it is a plain identifier.
func (r *capResolver) varOf(e ast.Expr) *types.Var {
	id, ok := ast.Unparen(e).(*ast.Ident)
	if !ok {
		return nil
	}
	if v, ok := r.info.Defs[id].(*types.Var); ok {
		return v
	}
	v, _ := r.info.Uses[id].(*types.Var)
	return v
}

// funcOf is the function or method a call's Fun names.
func (r *capResolver) funcOf(fun ast.Expr) *types.Func {
	id := calleeIdentOf(fun)
	if id == nil {
		return nil
	}
	fn, _ := r.info.Uses[id].(*types.Func)
	return fn
}

func (r *capResolver) capped(e ast.Expr, cappers []string) bool {
	if e == nil {
		return false
	}
	switch v := ast.Unparen(e).(type) {
	case *ast.BasicLit:
		return v.Kind == token.STRING
	case *ast.CallExpr:
		fn := r.funcOf(v.Fun)
		if fn == nil || fn.Pkg() != r.pkg || fn.Parent() != r.pkg.Scope() {
			return false
		}
		for _, c := range cappers {
			if fn.Name() == c {
				return true
			}
		}
		return false
	case *ast.Ident:
		switch obj := r.info.Uses[v].(type) {
		case *types.Const:
			return true
		case *types.Var:
			return r.cappedVar(obj, cappers)
		}
	}
	return false
}

// cappedVar reports whether every write to v is capped. A variable met again
// while its own writes are being resolved (x = x) adds nothing, so it counts
// as capped there; its other writes still decide.
func (r *capResolver) cappedVar(v *types.Var, cappers []string) bool {
	key := stateKey{v, strings.Join(cappers, ",")}
	switch r.state[key] {
	case 1:
		r.tentative++
		return true
	case 2:
		return true
	case 3:
		return false
	}
	r.state[key] = 1
	before := r.tentative
	ok := true
	if site, isParam := r.params[v]; isParam {
		if r.escaped[site.fn] || len(r.calls[site.fn]) == 0 || r.dispatchedByInterface(site.fn) {
			ok = false
		}
		for _, call := range r.calls[site.fn] {
			if site.index >= len(call.Args) || !r.capped(call.Args[site.index], cappers) {
				ok = false
				break
			}
		}
	} else if !isLocal(v, r.pkg) || len(r.writes[v]) == 0 {
		// A package-level variable or a field is not followed, and a local
		// with no write the pin can see (a closure's parameter, a type
		// switch's binding) holds a value it cannot vouch for.
		ok = false
	}
	for _, w := range r.writes[v] {
		if !ok {
			break
		}
		ok = r.capped(w, cappers)
	}
	switch {
	case !ok:
		// A false verdict is settled whatever was assumed: an assumption
		// only ever makes a value read as capped.
		r.state[key] = 3
	case r.tentative > before:
		// Capped only on an in-cycle assumption that its root may yet
		// overturn; resolve it afresh next time.
		delete(r.state, key)
	default:
		r.state[key] = 2
	}
	return ok
}

// dispatchedByInterface reports whether fn is a method an interface call
// can reach (ifaceMethods), whose callers are not all in calls.
func (r *capResolver) dispatchedByInterface(fn *types.Func) bool {
	return fn.Signature().Recv() != nil && r.ifaceMethods[fn.Name()]
}

func isLocal(v *types.Var, pkg *types.Package) bool {
	return v.Parent() != nil && v.Parent() != pkg.Scope() && v.Parent() != types.Universe
}

// stubImporter stands in an empty, complete package for every import.
type stubImporter struct{}

func (stubImporter) Import(importPath string) (*types.Package, error) {
	p := types.NewPackage(importPath, path.Base(importPath))
	p.MarkComplete()
	return p, nil
}
