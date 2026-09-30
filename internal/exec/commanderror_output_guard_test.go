package exec

import (
	"errors"
	"go/ast"
	"go/constant"
	"go/parser"
	"go/token"
	"go/types"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// redactImportPath is the package whose Text and Stdout a read of
// CommandError.Output must pass through.
const redactImportPath = modulePath + "/internal/redact"

// commandErrorOutputAllowed is the allowlist of findings
// TestEveryCommandErrorOutputReadIsRedacted admits, keyed
// "<file relative to the module root> <func>" as commandErrorValueAllowed
// is. An entry clears every read in that one function, and must name a
// function that exists (unmatchedAllowlistKeys).
var commandErrorOutputAllowed = map[string]string{
	"internal/branch/branch.go isGhNotFound": "compares the status code token of gh api -i's first line with 404 and never renders it",
	"internal/herdr/probe.go CheckFork":      "matches herdr's --help text against tabMoveUsage and never renders it",
}

// TestEveryCommandErrorOutputReadIsRedacted pins #952's typed-guard row: a
// failed command's stdout (CommandError.Output) is the child's text, and
// only internal/exec renders it redacted on its own. Outside internal/exec,
// every read of the field in a production file must be:
//
//   - an argument of redact.Text or redact.Stdout, parenthesized or not;
//   - an emptiness test: Output compared with "" (== or !=),
//     strings.TrimSpace(Output) compared with "", or len(Output) compared
//     with a constant;
//   - or the target of an assignment, which writes it.
//
// It is a row of the shared typed pass (typedGuardResults): every platform
// in guardPlatforms, under either cgo setting, resolved with go/types, so a
// promoted field (a struct embedding *CommandError) and a renamed import of
// redact are seen. It does not see a read through reflect, or through a
// generic body instantiated at CommandError, the routes
// TestNoProductionExpressionIsACommandErrorValue's doc names; nor a
// CommandError value formatted whole, which that test refuses.
//
// Mutations that turn it red: in internal/update/steps.go, return
// cmdErr.Output rather than redact.Stdout(cmdErr.Output); in
// internal/cli/update.go, compare strings.Contains(res.Output, cmdErr.Output);
// drop the herdr entry from commandErrorOutputAllowed; add an entry for a
// function that does not exist.
func TestEveryCommandErrorOutputReadIsRedacted(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	byFinding := map[string][]string{}
	results := typedGuardResults(t, root)
	for _, r := range results {
		for _, f := range r.outputReads {
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
	unmatchedAllowlistKeys(t, "commandErrorOutputAllowed", commandErrorOutputAllowed, results)
}

// commandErrorOutputField is the Output field of cmdErr, CommandError's
// type.
func commandErrorOutputField(cmdErr types.Type) (*types.Var, error) {
	st, ok := cmdErr.Underlying().(*types.Struct)
	if !ok {
		return nil, errors.New("CommandError is not a struct")
	}
	for f := range st.Fields() {
		if f.Name() == "Output" {
			return f, nil
		}
	}
	return nil, errors.New("CommandError has no Output field; the Output-read rule would check nothing")
}

// commandErrorOutputFindings reports each read of field in files outside
// internal/exec that is not one of the shapes
// TestEveryCommandErrorOutputReadIsRedacted admits, outside
// commandErrorOutputAllowed. Every allowlist key that names a function in
// files is recorded in allowSeen, when it is not nil.
func commandErrorOutputFindings(fset *token.FileSet, files []*ast.File, info *types.Info, field *types.Var, rel func(string) string, allowSeen map[string]bool) []string {
	var findings []string
	for _, file := range files {
		name := rel(fset.Position(file.Pos()).Filename)
		if strings.HasPrefix(name, "internal/exec/") {
			continue
		}
		for _, decl := range file.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && commandErrorOutputAllowed[name+" "+funcDeclName(fn)] != "" {
				if allowSeen != nil {
					allowSeen[name+" "+funcDeclName(fn)] = true
				}
				continue
			}
			var stack []ast.Node
			ast.Inspect(decl, func(n ast.Node) bool {
				if n == nil {
					stack = stack[:len(stack)-1]
					return true
				}
				if sel, ok := n.(*ast.SelectorExpr); ok && info.Uses[sel.Sel] == field && !outputReadAdmitted(info, stack, sel) {
					pos := fset.Position(sel.Pos())
					findings = append(findings, name+":"+strconv.Itoa(pos.Line)+":"+strconv.Itoa(pos.Column)+": reads "+types.ExprString(sel)+
						", a failed command's raw stdout, outside redact.Text or redact.Stdout; pass it through one of them, or test only whether it is empty (forgectl#952)")
				}
				stack = append(stack, n)
				return true
			})
		}
	}
	return findings
}

// outputReadAdmitted reports whether sel, whose ancestors are stack
// (outermost first), is an admitted read: see
// TestEveryCommandErrorOutputReadIsRedacted.
func outputReadAdmitted(info *types.Info, stack []ast.Node, sel *ast.SelectorExpr) bool {
	// parent returns the nearest ancestor above child that is not a
	// ParenExpr, with the child as that ancestor sees it.
	i := len(stack)
	var child ast.Node = sel
	parent := func() ast.Node {
		for i > 0 {
			i--
			if p, ok := stack[i].(*ast.ParenExpr); ok {
				child = p
				continue
			}
			return stack[i]
		}
		return nil
	}
	p := parent()
	// len(Output) compared with a constant.
	if call, ok := p.(*ast.CallExpr); ok {
		if b, ok := info.Uses[calledIdent(call)].(*types.Builtin); ok && b.Name() == "len" {
			child = call
			bin, ok := parent().(*ast.BinaryExpr)
			if !ok {
				return false
			}
			switch bin.Op {
			case token.EQL, token.NEQ, token.LSS, token.LEQ, token.GTR, token.GEQ:
				tv, ok := info.Types[otherOperand(bin, child)]
				return ok && tv.Value != nil
			}
			return false
		}
	}
	switch p := p.(type) {
	case *ast.AssignStmt:
		// A plain assignment writes the field; += reads it too.
		return p.Tok == token.ASSIGN && slices.Contains(p.Lhs, ast.Expr(child.(ast.Expr)))
	case *ast.BinaryExpr:
		return (p.Op == token.EQL || p.Op == token.NEQ) && isEmptyString(info, otherOperand(p, child))
	case *ast.CallExpr:
		fn := calledFunc(info, p)
		if fn == nil || !slices.Contains(p.Args, ast.Expr(child.(ast.Expr))) {
			return false
		}
		switch {
		case fn.Pkg() != nil && fn.Pkg().Path() == redactImportPath && (fn.Name() == "Text" || fn.Name() == "Stdout"):
			return true
		case fn.Pkg() != nil && fn.Pkg().Path() == "strings" && fn.Name() == "TrimSpace":
			child = p
			b, ok := parent().(*ast.BinaryExpr)
			return ok && (b.Op == token.EQL || b.Op == token.NEQ) && isEmptyString(info, otherOperand(b, child))
		}
	}
	return false
}

// otherOperand is b's operand that is not child.
func otherOperand(b *ast.BinaryExpr, child ast.Node) ast.Expr {
	if ast.Node(b.X) == child {
		return b.Y
	}
	return b.X
}

// isEmptyString reports whether e is the constant "".
func isEmptyString(info *types.Info, e ast.Expr) bool {
	tv, ok := info.Types[e]
	return ok && tv.Value != nil && tv.Value.Kind() == constant.String && constant.StringVal(tv.Value) == ""
}

// calledIdent is the identifier naming call's function: f in f(…), and Sel
// in pkg.f(…).
func calledIdent(call *ast.CallExpr) *ast.Ident {
	fun := call.Fun
	for {
		paren, ok := fun.(*ast.ParenExpr)
		if !ok {
			break
		}
		fun = paren.X
	}
	switch f := fun.(type) {
	case *ast.Ident:
		return f
	case *ast.SelectorExpr:
		return f.Sel
	}
	return nil
}

// calledFunc is the package-level function call calls, or nil.
func calledFunc(info *types.Info, call *ast.CallExpr) *types.Func {
	fn, ok := info.Uses[calledIdent(call)].(*types.Func)
	if !ok || fn.Signature().Recv() != nil {
		return nil
	}
	return fn
}

// TestCommandErrorOutputFindingsSeeEveryRoute pins
// commandErrorOutputFindings against each read shape, admitted or not. The
// probe declares its own CommandError, Text and Stdout, and is type-checked
// under redact's import path, so its Text and Stdout stand in for redact's.
//
// Mutations that turn it red: admit every CallExpr argument (the
// strings.Contains row goes quiet); admit any BinaryExpr (the "+" row goes
// quiet); drop the ParenExpr walk (the parenthesized rows report); admit
// any assignment (the += row goes quiet); drop the TrimSpace arm (its row
// reports); drop the len arm (its row reports).
func TestCommandErrorOutputFindingsSeeEveryRoute(t *testing.T) {
	const prelude = "package probe\n\nimport \"strings\"\n\nvar _ = strings.TrimSpace\n\n" +
		"type CommandError struct{ Name, Output string }\n\ntype wrap struct{ *CommandError }\n\n" +
		"func Text(s string) string { return s }\n\nfunc Stdout(s string) string { return s }\n\n"
	rows := []struct {
		name, src string
		want      bool
	}{
		{"returned raw", `func f(ce *CommandError) string { return ce.Output }`, true},
		{"passed to another function", `func f(ce *CommandError, s string) bool { return strings.Contains(s, ce.Output) }`, true},
		{"concatenated", `func f(ce *CommandError) string { return "x" + ce.Output }`, true},
		{"defined into a variable", `func f(ce *CommandError) int { out := ce.Output; return len(out) }`, true},
		{"promoted through an embedding", `func f(w wrap) string { return w.Output }`, true},
		{"compared with a non-empty string", `func f(ce *CommandError) bool { return ce.Output == "x" }`, true},
		{"through Text", `func f(ce *CommandError) string { return Text(ce.Output) }`, false},
		{"through Stdout, parenthesized", `func f(ce *CommandError) string { return Stdout((ce.Output)) }`, false},
		{"compared with empty", `func f(ce *CommandError) bool { return ce.Output == "" || "" != (ce.Output) }`, false},
		{"TrimSpace compared with empty", `func f(ce *CommandError) bool { return strings.TrimSpace(ce.Output) != "" }`, false},
		{"len compared with a constant", `func f(ce *CommandError) bool { return len(ce.Output) > 0 }`, false},
		{"read by +=", `func f(ce *CommandError) { ce.Output += "x" }`, true},
		{"written", `func f(ce *CommandError) { ce.Output = "" }`, false},
	}
	probe := newTypedProbe(t)
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			got := probe.commandErrorOutputFindings(t, prelude+row.src+"\n")
			if row.want && len(got) == 0 {
				t.Errorf("no finding; the matcher misses this route")
			}
			if !row.want && len(got) != 0 {
				t.Errorf("findings on an admitted read:\n%s", strings.Join(got, "\n"))
			}
		})
	}
	t.Run("the allowlist is keyed on the enclosing function", func(t *testing.T) {
		src := prelude + "func probeOK(ce *CommandError) string { return ce.Output }\n\nfunc other(ce *CommandError) string { return ce.Output }\n"
		commandErrorOutputAllowed["probe.go probeOK"] = "probe: the allowlist key's shape"
		defer delete(commandErrorOutputAllowed, "probe.go probeOK")
		seen := map[string]bool{}
		got := probe.commandErrorOutputFindingsSeen(t, src, seen)
		if len(got) != 1 || !strings.Contains(got[0], "probe.go:17:") {
			t.Errorf("want exactly other's read reported, got:\n%s", strings.Join(got, "\n"))
		}
		if !seen["probe.go probeOK"] {
			t.Error("the matched entry was not recorded, so unmatchedAllowlistKeys would call it stale")
		}
	})
}

// commandErrorOutputFindings type-checks src as one file and returns
// commandErrorOutputFindings over it, aimed at the CommandError src
// declares, with the probe's own Text and Stdout standing in for redact's.
func (p *typedProbe) commandErrorOutputFindings(t *testing.T, src string) []string {
	t.Helper()
	return p.commandErrorOutputFindingsSeen(t, src, nil)
}

func (p *typedProbe) commandErrorOutputFindingsSeen(t *testing.T, src string, seen map[string]bool) []string {
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
	pkg, err := conf.Check(redactImportPath, p.fset, []*ast.File{file}, info)
	if err != nil {
		t.Fatalf("type-check probe: %v\n%s", err, src)
	}
	target, err := commandErrorType(pkg)
	if err != nil {
		t.Fatal(err)
	}
	field, err := commandErrorOutputField(target)
	if err != nil {
		t.Fatal(err)
	}
	return commandErrorOutputFindings(p.fset, []*ast.File{file}, info, field, func(s string) string { return s }, seen)
}
