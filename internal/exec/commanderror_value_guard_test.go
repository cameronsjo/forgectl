package exec

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// TestNoProductionExpressionIsACommandErrorValue pins #941 item 4: every
// formatter CommandError has that redacts (Error, Format, GoString,
// LogValue) is on the pointer receiver, so a CommandError VALUE (not a
// pointer) prints through fmt and slog field by field, Stderr and Output raw.
// No production site holds one today; this keeps it so. It is a row of the
// shared typed pass (typedGuardResults): on every platform in guardPlatforms,
// under either cgo setting, no production file may hold an expression or
// declare a variable, field, parameter or result whose type is a CommandError
// value, or holds one by value in an array, slice, map, channel, unnamed
// struct or result tuple, per commandErrorValueFindings. A pointer to one is
// fine, and so is &CommandError{…}: the literal's address is taken where it is
// built. Test files are not production files and are not checked.
//
// The alternative, moving the formatters to value receivers, was not taken:
// it changes how a nil *CommandError renders under %#v.
//
// It does not see two routes that make a value with no expression of that
// type in a production file (#952). A generic function body instantiated
// at CommandError (`func dump[T any](p *T) string { return
// fmt.Sprintf("%+v", *p) }` called as dump(ce)): *p has type T, and the rule
// does not enter type arguments (holdsCommandErrorValue). And
// reflect.ValueOf(ce).Elem(), whose Interface() is an any: refusing
// reflect.Value.Elem in TestNoFileReadsMemoryThroughReflect was weighed and
// not taken, because internal/cli/config_cmd.go dereferences config pointers
// through it. No production site does either today.
//
// An allowlist entry that names no function in any checked file fails the
// test (#952): a renamed or deleted function would otherwise leave an entry
// that clears whatever takes the name next.
//
// Mutations that turn it red: a production file in internal/tmux declaring
// `func f(ce *exec.CommandError) string { return fmt.Sprintf("%+v", *ce) }`,
// or `var zero exec.CommandError`; an entry
// "internal/exec/exec.go NoSuchFunc" in commandErrorValueAllowed.
func TestNoProductionExpressionIsACommandErrorValue(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	byFinding := map[string][]string{}
	results := typedGuardResults(t, root)
	for _, r := range results {
		for _, f := range r.cmdErrValues {
			byFinding[f] = append(byFinding[f], r.p.String())
		}
	}
	unmatchedAllowlistKeys(t, "commandErrorValueAllowed", commandErrorValueAllowed, results)
	keys := make([]string, 0, len(byFinding))
	for f := range byFinding {
		keys = append(keys, f)
	}
	slices.Sort(keys)
	for _, f := range keys {
		t.Errorf("%s [%s]", f, strings.Join(slices.Compact(byFinding[f]), " "))
	}
}

// commandErrorValueAllowed is the allowlist of findings
// TestNoProductionExpressionIsACommandErrorValue admits, keyed
// "<file relative to the module root> <func>", where <func> is the enclosing
// function's name, or "T.m" for a method. An entry clears every finding in
// that one function. Each existing entry copies a *CommandError to change one
// field and returns the copy's address; the value is never formatted.
var commandErrorValueAllowed = map[string]string{
	"internal/exec/exec.go WithoutOutput":   "copies *top to clear Output, then returns &cp",
	"internal/exec/fake.go issuedArgvError": "copies *cmdErr to restore the issued argv, then returns &cp",
}

// commandErrorValueFindings reports each expression (not a type expression)
// and each declared variable, field, parameter or named result in files
// whose type holds a value of target's underlying type
// (holdsCommandErrorValue), outside commandErrorValueAllowed. A composite
// literal whose address is taken on the spot (&CommandError{…}, parenthesized
// or not) is the one exception: it never exists as a value anyone can
// format. A literal written as an element of a []*CommandError, with its type
// elided, is recorded as the pointer type already, so it needs no exception.
//
// Every allowlist key that names a function in files is recorded in
// allowSeen, when it is not nil, for unmatchedAllowlistKeys.
func commandErrorValueFindings(fset *token.FileSet, files []*ast.File, info *types.Info, target types.Type, rel func(string) string, allowSeen map[string]bool) []string {
	var findings []string
	seen := map[string]bool{}
	report := func(pos token.Pos, what string, t types.Type) {
		position := fset.Position(pos)
		f := rel(position.Filename) + ":" + strconv.Itoa(position.Line) + ":" + strconv.Itoa(position.Column) + ": " + what + " has type " + t.String() +
			", which holds a CommandError value: its redacting formatters are on the pointer receiver, so fmt and slog render a value's Stderr and Output raw; hold a *CommandError instead (forgectl#941)"
		if !seen[f] {
			seen[f] = true
			findings = append(findings, f)
		}
	}
	holds := func(t types.Type) bool { return holdsCommandErrorValue(t, target, map[types.Type]bool{}) }
	for _, file := range files {
		name := rel(fset.Position(file.Pos()).Filename)
		addressed := map[ast.Expr]bool{}
		ast.Inspect(file, func(n ast.Node) bool {
			u, ok := n.(*ast.UnaryExpr)
			if !ok || u.Op != token.AND {
				return true
			}
			var chain []ast.Expr
			x := u.X
			for {
				chain = append(chain, x)
				paren, ok := x.(*ast.ParenExpr)
				if !ok {
					break
				}
				x = paren.X
			}
			if _, ok := x.(*ast.CompositeLit); ok {
				for _, e := range chain {
					addressed[e] = true
				}
			}
			return true
		})
		for _, decl := range file.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && commandErrorValueAllowed[name+" "+funcDeclName(fn)] != "" {
				if allowSeen != nil {
					allowSeen[name+" "+funcDeclName(fn)] = true
				}
				continue
			}
			ast.Inspect(decl, func(n ast.Node) bool {
				if id, ok := n.(*ast.Ident); ok {
					if v, ok := info.Defs[id].(*types.Var); ok && holds(v.Type()) {
						report(id.Pos(), "declared "+id.Name, v.Type())
					}
				}
				expr, ok := n.(ast.Expr)
				if !ok || addressed[expr] {
					return true
				}
				if tv, ok := info.Types[expr]; ok && !tv.IsType() && holds(tv.Type) {
					report(expr.Pos(), types.ExprString(expr), tv.Type)
				}
				return true
			})
		}
	}
	return findings
}

// funcDeclName is fn's name for commandErrorValueAllowed: "f", or "T.m" for
// a method on T or *T.
func funcDeclName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}
	recv := fn.Recv.List[0].Type
	if star, ok := recv.(*ast.StarExpr); ok {
		recv = star.X
	}
	switch r := recv.(type) {
	case *ast.IndexExpr:
		recv = r.X
	case *ast.IndexListExpr:
		recv = r.X
	}
	return types.ExprString(recv) + "." + fn.Name.Name
}

// holdsCommandErrorValue reports whether a value of type t is, or holds by
// value, a value whose type's underlying type is target's: target itself, a
// type defined on it (type T exec.CommandError, which drops the methods), or
// either as an element or key of an array, slice, map or channel, a field of
// an unnamed struct, or a member of a result tuple. A pointer, an interface
// and a function are not entered: none renders its referent through fmt's
// default struct printing. A named type on another underlying type is not
// entered either: a field of one that held a value is reported where it is
// declared. Nor are a generic type's type arguments, so Box[CommandError]
// is not seen: a type argument may sit behind a pointer
// (atomic.Pointer[CommandError]), and this rule has no allowlist to clear
// that false positive.
func holdsCommandErrorValue(t, target types.Type, seen map[types.Type]bool) bool {
	if t == nil || seen[t] {
		return false
	}
	seen[t] = true
	holds := func(t types.Type) bool { return holdsCommandErrorValue(t, target, seen) }
	switch t := types.Unalias(t).(type) {
	case *types.Named:
		return types.Identical(t.Underlying(), target.Underlying())
	case *types.Slice:
		return holds(t.Elem())
	case *types.Array:
		return holds(t.Elem())
	case *types.Chan:
		return holds(t.Elem())
	case *types.Map:
		return holds(t.Key()) || holds(t.Elem())
	case *types.Struct:
		for f := range t.Fields() {
			if holds(f.Type()) {
				return true
			}
		}
	case *types.Tuple:
		for v := range t.Variables() {
			if holds(v.Type()) {
				return true
			}
		}
	}
	return false
}

// commandErrorType is the CommandError type execPkg declares, for
// commandErrorValueFindings.
func commandErrorType(execPkg *types.Package) (types.Type, error) {
	tn, ok := execPkg.Scope().Lookup("CommandError").(*types.TypeName)
	if !ok {
		return nil, errors.New("internal/exec declares no CommandError type; the CommandError-value rule would check nothing")
	}
	if _, ok := tn.Type().Underlying().(*types.Struct); !ok {
		return nil, errors.New("CommandError is not a struct")
	}
	return tn.Type(), nil
}

// TestCommandErrorValueFindingsSeeEveryRoute pins commandErrorValueFindings
// against each way a CommandError value comes to exist, and the pointer
// shapes it leaves alone. The probe declares its own CommandError, a struct
// like exec's, and the rule is aimed at it.
//
// Mutations that turn it red: skip every CompositeLit rather than only an
// addressed one (the literal and result-tuple rows go quiet); follow no
// ParenExpr when marking an addressed literal (the parenthesized row
// reports); clear the whole probe file rather than one function (every
// finding row goes quiet); drop
// the Defs arm (the package-level var and struct field rows go quiet); check
// types.Identical against target rather than the underlying types (the
// defined-type row goes quiet); enter a *types.Pointer (the pointer rows
// report); drop the IsType skip (the pointer rows report their
// `CommandError` type expression).
func TestCommandErrorValueFindingsSeeEveryRoute(t *testing.T) {
	const prelude = "package probe\n\nimport \"fmt\"\n\nvar _ = fmt.Sprint\n\ntype CommandError struct{ Name, Stderr string }\n\nfunc (e *CommandError) Error() string { return e.Name }\n\n"
	rows := []struct {
		name, src string
		want      bool
	}{
		{"dereference", `func f(ce *CommandError) string { return fmt.Sprintf("%+v", *ce) }`, true},
		{"composite literal", `func f() string { return fmt.Sprint(CommandError{Name: "x"}) }`, true},
		{"package-level var, never used", `var zero CommandError`, true},
		{"struct field", `type wrap struct{ E CommandError }`, true},
		{"slice of values", `func f(xs []CommandError) int { return len(xs) }`, true},
		{"map value", `var m map[string]CommandError`, true},
		{"unnamed struct field", `var s struct{ E CommandError }`, true},
		{"result tuple", `func g() (CommandError, error) { return CommandError{}, nil }
func f() { _, _ = g() }`, true},
		{"type assertion to a value", `func f(v any) bool { _, ok := v.(CommandError); return ok }`, true},
		{"defined type drops the methods", `type raw CommandError
func f(ce *CommandError) string { return fmt.Sprintf("%+v", raw{Name: ce.Name}) }`, true},
		{"address of a literal", `func f() error { return &CommandError{Name: "x"} }`, false},
		{"parenthesized address of a literal", `func f() error { return &(CommandError{Name: "x"}) }`, false},
		{"pointer variable and new", `func f() *CommandError { var p *CommandError; if p == nil { p = new(CommandError) }; return p }`, false},
		{"elided pointer elements", `var xs = []*CommandError{{Name: "a"}, {Name: "b"}}`, false},
		{"field reads through a pointer", `func f(ce *CommandError) string { return ce.Name + ce.Stderr }`, false},
	}
	probe := newTypedProbe(t)
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			got := probe.commandErrorValueFindings(t, prelude+row.src+"\n")
			if row.want && len(got) == 0 {
				t.Errorf("no finding; the matcher misses this route")
			}
			if !row.want && len(got) != 0 {
				t.Errorf("findings on a clean probe:\n%s", strings.Join(got, "\n"))
			}
		})
	}
	t.Run("the allowlist is keyed on the enclosing function", func(t *testing.T) {
		src := prelude + "func (e *CommandError) clone() *CommandError { cp := *e; return &cp }\n\n" +
			"func other(e *CommandError) *CommandError { cp := *e; return &cp }\n"
		commandErrorValueAllowed["probe.go CommandError.clone"] = "probe: the allowlist key's shape"
		defer delete(commandErrorValueAllowed, "probe.go CommandError.clone")
		got := probe.commandErrorValueFindings(t, src)
		if len(got) == 0 {
			t.Fatal("clone's entry cleared other too; the key is not on the enclosing function")
		}
		for _, f := range got {
			if !strings.HasPrefix(f, "probe.go:13:") {
				t.Errorf("a finding outside other, which the entry for clone should have cleared: %s", f)
			}
		}
	})
}

// commandErrorValueFindings type-checks src as one file and returns
// commandErrorValueFindings over it, aimed at the CommandError src declares.
func (p *typedProbe) commandErrorValueFindings(t *testing.T, src string) []string {
	t.Helper()
	file, err := parser.ParseFile(p.fset, "probe.go", src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	info := &types.Info{
		Types: map[ast.Expr]types.TypeAndValue{},
		Defs:  map[*ast.Ident]types.Object{},
		Uses:  map[*ast.Ident]types.Object{},
	}
	conf := types.Config{Importer: p.imp}
	pkg, err := conf.Check("probe", p.fset, []*ast.File{file}, info)
	if err != nil {
		t.Fatalf("type-check probe: %v\n%s", err, src)
	}
	target, err := commandErrorType(pkg)
	if err != nil {
		t.Fatal(err)
	}
	return commandErrorValueFindings(p.fset, []*ast.File{file}, info, target, func(s string) string { return s }, nil)
}

// unmatchedAllowlistKeys fails t for each key of allowed that no platform's
// typed pass recorded in allowSeen: an entry naming a function that no
// longer exists (#952).
func unmatchedAllowlistKeys(t *testing.T, what string, allowed map[string]string, results []*typedResult) {
	t.Helper()
	keys := make([]string, 0, len(allowed))
	for k := range allowed {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		if !slices.ContainsFunc(results, func(r *typedResult) bool { return r.allowSeen[k] }) {
			t.Errorf("%s entry %q names no function in any checked production file on any platform; remove it", what, k)
		}
	}
}
