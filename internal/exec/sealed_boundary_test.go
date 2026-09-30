package exec

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"io"
	"os"
	osexec "os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

// TestSealedIsUnimportableOutsideExec proves the rule the whole seal rests on
// (forgectl#854): Go's internal-package rule lets only internal/exec and its
// subpackages import internal/exec/internal/sealed, so a package anywhere else
// that tries to reach a payload through sealed does not compile.
//
// It runs a real `go build` of a probe package that imports sealed, placed at
// module paths outside internal/exec (each must fail with the go tool's "use
// of internal package ... not allowed") and inside it (each must build, which
// shows the outside failures come from the boundary and not from a broken
// probe). The probe is supplied through -overlay, so no file is written into
// the tree: a probe directory that existed while other packages' tests walk
// the module could fail them, or be seen by the module guards here.
//
// Mutation that turns it red: move sealed.go to internal/sealed (outside
// internal/exec) and point sealedImportPath and internal/exec's imports at it;
// every outside row then builds.
func TestSealedIsUnimportableOutsideExec(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := osexec.LookPath("go"); err != nil {
		t.Fatalf("the go tool is not on PATH, and this proof needs it: %v", err)
	}
	backing := filepath.Join(t.TempDir(), "probe.go")
	src := "package zzsealprobe\n\nimport \"" + sealedImportPath + "\"\n\nvar _ = sealed.New\n"
	if err := os.WriteFile(backing, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	build := func(rel string) (string, error) {
		overlay, err := json.Marshal(map[string]map[string]string{
			"Replace": {filepath.Join(root, filepath.FromSlash(rel), "probe.go"): backing},
		})
		if err != nil {
			t.Fatal(err)
		}
		overlayPath := filepath.Join(t.TempDir(), "overlay.json")
		if err := os.WriteFile(overlayPath, overlay, 0o600); err != nil {
			t.Fatal(err)
		}
		cmd := osexec.CommandContext(t.Context(), "go", "build", "-overlay", overlayPath, "./"+rel) //nolint:gosec // G204: the go tool with a probe path this test chose; nothing is tainted
		cmd.Dir = root
		cmd.Env = append(os.Environ(), pinnedGoEnv...)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	for _, rel := range []string{"zzsealprobe", "internal/zzsealprobe", "internal/surface/zzsealprobe", "internal/execzz/zzsealprobe"} {
		out, err := build(rel)
		if err == nil {
			t.Errorf("%s imports sealed and built; the payload is reachable from outside internal/exec", rel)
			continue
		}
		if want := "use of internal package " + sealedImportPath + " not allowed"; !strings.Contains(out, want) {
			t.Errorf("%s failed to build, but not on the internal-package rule (want %q):\n%s", rel, want, out)
		}
	}
	for _, rel := range []string{"internal/exec/zzsealprobe", "internal/exec/internal/zzsealprobe"} {
		if out, err := build(rel); err != nil {
			t.Errorf("%s imports sealed from inside internal/exec and failed to build, so the probe is broken: %v\n%s", rel, err, out)
		}
	}
}

// TestSealedStartHasOneCaller pins, in internal/exec's production files on
// every platform in guardPlatforms, the one path to a process: sealed.Start,
// the one function that puts a payload into a process, is named only inside
// the func startSealed, and startSealed, which skips validate, is named only
// inside (*OSSensitiveRunner).RunSensitive. That the one call there comes
// after validate, and that startSealed is never used as a value, is
// TestValidateDominatesStartSealed's. Test files are not checked.
//
// This is not what keeps plaintext out of internal/exec: the compiler does
// that, since sealed.Start returns only a *sealed.Proc (wait and kill, no
// accessor, pinned by the golden) and no exported name of sealed returns an
// *exec.Cmd. It keeps process launches through the seam to the runner, so a
// second launch path, or one that skips validate, cannot appear without
// review. Both targets are package-level funcs, which can be reached only by
// naming them (a call or a func value), so a Uses walk sees every route;
// there is no interface or method-value shape that avoids it.
//
// Mutations that turn it red, each in sensitive.go: `var _ = sealed.Start`;
// `var _, _ = startSealed(&OSSensitiveRunner{}, SensitiveCommand{Path:
// Secret("sh"), Args: []Arg{Opaque("-c")}}, nil, nil)`.
func TestSealedStartHasOneCaller(t *testing.T) {
	for _, p := range guardPlatforms {
		c := checkExecFor(t, p)
		start, ok := c.sealed.Scope().Lookup("Start").(*types.Func)
		if !ok {
			t.Fatalf("[%s] sealed declares no Start func; the rule would check nothing", p)
		}
		for _, f := range namedOutside(c, start, "", "startSealed") {
			t.Errorf("[%s] %s: sealed.Start is named outside startSealed; launch sealed processes through the runner",
				p, f)
		}
		startSealed, ok := c.pkg.Scope().Lookup("startSealed").(*types.Func)
		if !ok {
			t.Fatalf("[%s] internal/exec declares no startSealed func; the rule would check nothing", p)
		}
		for _, f := range namedOutside(c, startSealed, "*OSSensitiveRunner", "RunSensitive") {
			t.Errorf("[%s] %s: startSealed is named outside (*OSSensitiveRunner).RunSensitive; it skips validate, so only RunSensitive may call it",
				p, f)
		}
	}
}

// TestValidateDominatesStartSealed pins the ordering TestSealedStartHasOneCaller
// cannot see (forgectl#888): inside (*OSSensitiveRunner).RunSensitive, on
// every platform in guardPlatforms, validate runs, and refuses, before the
// one startSealed call can be reached. The rule is deliberately simple
// enough to read at a glance:
//
//   - startSealed is named exactly once, as the function of a direct call
//     expression that sits outside every func literal, so it is never a
//     func value, never captured by a closure and never stored;
//   - a statement at the top level of RunSensitive's body is
//     `if err := sc.validate(); err != nil { ...; return ... }`, with no
//     else, whose sc is RunSensitive's own parameter, and the statement
//     holding the startSealed call comes after it in that same block;
//   - that call passes the same sc, and nothing in RunSensitive assigns to
//     sc, takes its address, or reaches a field of it on the left of an
//     assignment, so the command validated is the command started;
//   - RunSensitive holds no goto and no label, so no jump skips validate.
//
// A mutation of sc through a helper that is handed sc.Args stays a review
// property.
//
// Mutations that turn it red, each in RunSensitive: move the startSealed
// call above the validate statement; add `zzEsc = func() (*sealed.Proc,
// error) { return startSealed(r, sc, nil, nil) }` with a package var zzEsc;
// add `zzEsc = startSealed`; add `sc.Path = Secret("/bin/sh")` after
// validate.
func TestValidateDominatesStartSealed(t *testing.T) {
	for _, p := range guardPlatforms {
		for _, f := range validateDominanceFindings(checkExecFor(t, p)) {
			t.Errorf("[%s] %s", p, f)
		}
	}
}

// validateDominanceFindings returns every way c's RunSensitive breaks the
// rule TestValidateDominatesStartSealed states.
func validateDominanceFindings(c *checkedPackage) []string {
	startSealed, ok := c.pkg.Scope().Lookup("startSealed").(*types.Func)
	if !ok {
		return []string{"internal/exec declares no startSealed func; the rule would check nothing"}
	}
	var fd *ast.FuncDecl
	for _, f := range c.files {
		for _, decl := range f.Decls {
			d, ok := decl.(*ast.FuncDecl)
			if ok && d.Name.Name == "RunSensitive" && d.Recv != nil && len(d.Recv.List) == 1 &&
				types.ExprString(d.Recv.List[0].Type) == "*OSSensitiveRunner" && d.Body != nil {
				fd = d
			}
		}
	}
	if fd == nil {
		return []string{"no (*OSSensitiveRunner).RunSensitive with a body; the rule would check nothing"}
	}
	var findings []string
	pos := func(n ast.Node) string { return c.fset.Position(n.Pos()).String() }

	// sc is RunSensitive's SensitiveCommand parameter, found through its
	// uses (checkExecFor records no Defs).
	var sc types.Object
	for _, field := range fd.Type.Params.List {
		if types.ExprString(field.Type) != "SensitiveCommand" || len(field.Names) != 1 {
			continue
		}
		name := field.Names[0]
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && sc == nil {
				if obj := c.info.Uses[id]; obj != nil && obj.Pos() == name.Pos() {
					sc = obj
				}
			}
			return sc == nil
		})
	}
	if sc == nil {
		return []string{pos(fd) + ": RunSensitive has no single SensitiveCommand parameter used in its body; the rule would check nothing"}
	}
	isSC := func(e ast.Expr) bool {
		id, ok := ast.Unparen(e).(*ast.Ident)
		return ok && c.info.Uses[id] == sc
	}

	// Every use of startSealed must be the Fun of a call outside any func
	// literal; record the call.
	var calls []*ast.CallExpr
	var walk func(n ast.Node, inLit bool)
	walk = func(root ast.Node, inLit bool) {
		ast.Inspect(root, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.FuncLit:
				if n != root {
					walk(n, true)
					return false
				}
			case *ast.CallExpr:
				if id, ok := ast.Unparen(n.Fun).(*ast.Ident); ok && c.info.Uses[id] == startSealed {
					if inLit {
						findings = append(findings, pos(n)+": startSealed is called inside a func literal; a closure can carry it past validate")
					} else {
						calls = append(calls, n)
					}
					for _, arg := range n.Args {
						walk(arg, inLit)
					}
					return false
				}
			case *ast.Ident:
				if c.info.Uses[n] == startSealed {
					findings = append(findings, pos(n)+": startSealed is used as a value; only a direct call in RunSensitive may name it")
				}
			case *ast.BranchStmt:
				if n.Tok == token.GOTO {
					findings = append(findings, pos(n)+": goto in RunSensitive; a jump can skip validate")
				}
			case *ast.LabeledStmt:
				findings = append(findings, pos(n)+": label in RunSensitive; a jump can skip validate")
			case *ast.AssignStmt:
				for _, lhs := range n.Lhs {
					if isSC(rootOperand(lhs)) {
						findings = append(findings, pos(lhs)+": RunSensitive writes sc after it is validated")
					}
				}
			case *ast.IncDecStmt:
				if isSC(rootOperand(n.X)) {
					findings = append(findings, pos(n)+": RunSensitive writes sc after it is validated")
				}
			case *ast.RangeStmt:
				for _, e := range []ast.Expr{n.Key, n.Value} {
					if e != nil && n.Tok == token.ASSIGN && isSC(rootOperand(e)) {
						findings = append(findings, pos(e)+": RunSensitive writes sc after it is validated")
					}
				}
			case *ast.UnaryExpr:
				if n.Op == token.AND && isSC(rootOperand(n.X)) {
					findings = append(findings, pos(n)+": RunSensitive takes sc's address, so it can change after validate")
				}
			case *ast.SelectorExpr:
				if sel := c.info.Selections[n]; sel != nil && sel.Kind() != types.FieldVal && isSC(rootOperand(n.X)) {
					if sig, ok := sel.Obj().Type().(*types.Signature); ok && sig.Recv() != nil {
						if _, ptr := sig.Recv().Type().(*types.Pointer); ptr {
							findings = append(findings, pos(n)+": RunSensitive calls a pointer-receiver method on sc, so it can change after validate")
						}
					}
				}
			}
			return true
		})
	}
	walk(fd.Body, false)
	if len(calls) != 1 {
		findings = append(findings, fmt.Sprintf("%s: RunSensitive calls startSealed directly %d times, want exactly 1", pos(fd), len(calls)))
		return findings
	}
	call := calls[0]
	if len(call.Args) < 2 || !isSC(call.Args[1]) {
		findings = append(findings, pos(call)+": startSealed is not passed RunSensitive's own sc, the command validate checked")
	}

	validateAt, callAt := -1, -1
	for i, stmt := range fd.Body.List {
		if validateAt < 0 && isValidateGate(c, stmt, isSC) {
			validateAt = i
		}
		if stmt.Pos() <= call.Pos() && call.End() <= stmt.End() {
			callAt = i
		}
	}
	switch {
	case validateAt < 0:
		findings = append(findings, pos(fd)+": RunSensitive has no top-level `if err := sc.validate(); err != nil { ...; return ... }`")
	case callAt <= validateAt:
		findings = append(findings, pos(call)+": startSealed is called before the validate statement at "+pos(fd.Body.List[validateAt]))
	}
	return findings
}

// isValidateGate reports whether stmt is `if err := sc.validate(); err != nil
// { ...; return ... }` with no else, sc matching isSC and validate being
// SensitiveCommand's own method.
func isValidateGate(c *checkedPackage, stmt ast.Stmt, isSC func(ast.Expr) bool) bool {
	ifs, ok := stmt.(*ast.IfStmt)
	if !ok || ifs.Else != nil || len(ifs.Body.List) == 0 {
		return false
	}
	if _, ok := ifs.Body.List[len(ifs.Body.List)-1].(*ast.ReturnStmt); !ok {
		return false
	}
	init, ok := ifs.Init.(*ast.AssignStmt)
	if !ok || init.Tok != token.DEFINE || len(init.Lhs) != 1 || len(init.Rhs) != 1 {
		return false
	}
	errID, ok := init.Lhs[0].(*ast.Ident)
	if !ok {
		return false
	}
	call, ok := init.Rhs[0].(*ast.CallExpr)
	if !ok || len(call.Args) != 0 {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || !isSC(sel.X) {
		return false
	}
	fn, ok := c.info.Uses[sel.Sel].(*types.Func)
	if !ok || fn.Name() != "validate" || fn.Pkg() != c.pkg {
		return false
	}
	if recv := fn.Signature().Recv(); recv == nil || types.TypeString(recv.Type(), nil) != execImportPath+".SensitiveCommand" {
		return false
	}
	cond, ok := ifs.Cond.(*ast.BinaryExpr)
	if !ok || cond.Op != token.NEQ {
		return false
	}
	x, ok := cond.X.(*ast.Ident)
	if !ok || c.info.Uses[x] == nil || c.info.Uses[x].Pos() != errID.Pos() {
		return false
	}
	y, ok := cond.Y.(*ast.Ident)
	return ok && y.Name == "nil" && c.info.Uses[y] == types.Universe.Lookup("nil")
}

// rootOperand strips selectors, indexes, slices, derefs and parens down to
// the operand an lvalue is rooted at: sc for sc.Args[0].v.
func rootOperand(e ast.Expr) ast.Expr {
	for {
		switch x := e.(type) {
		case *ast.ParenExpr:
			e = x.X
		case *ast.SelectorExpr:
			e = x.X
		case *ast.IndexExpr:
			e = x.X
		case *ast.IndexListExpr:
			e = x.X
		case *ast.SliceExpr:
			e = x.X
		case *ast.StarExpr:
			e = x.X
		default:
			return e
		}
	}
}

// namedOutside returns the position of every use of target in c's production
// files that does not sit inside the func within (a method of recv when recv
// is not empty, a plain func when it is), plus
// one extra finding when there is no use inside within at all, since then the
// matcher, not the package, is what is clean.
func namedOutside(c *checkedPackage, target *types.Func, recv, within string) []string {
	var findings []string
	inside := 0
	for _, f := range c.files {
		for _, decl := range f.Decls {
			fd, isFunc := decl.(*ast.FuncDecl)
			door := isFunc && fd.Name.Name == within
			if door && recv == "" {
				door = fd.Recv == nil
			} else if door {
				door = fd.Recv != nil && len(fd.Recv.List) == 1 && types.ExprString(fd.Recv.List[0].Type) == recv
			}
			ast.Inspect(decl, func(n ast.Node) bool {
				id, ok := n.(*ast.Ident)
				if !ok || c.info.Uses[id] != target {
					return true
				}
				if door {
					inside++
				} else {
					findings = append(findings, c.fset.Position(id.Pos()).String())
				}
				return true
			})
		}
	}
	if inside == 0 {
		findings = append(findings, "(no use inside "+within+" at all: the matcher is broken, not the package clean)")
	}
	return findings
}

// sealedImporterAllowed is every package, by import path with any test-variant
// suffix removed, that may import internal/exec/internal/sealed: internal/exec
// itself, and the test binaries go test builds for sealed's own tests.
var sealedImporterAllowed = map[string]bool{
	execImportPath:             true,
	sealedImportPath + ".test": true,
	sealedImportPath + "_test": true,
}

// TestOnlyExecImportsSealed closes what the internal-package rule leaves open:
// that rule admits every package under internal/exec, so a future
// internal/exec/<sub> could import sealed and hold a payload in a package no
// other guard reads. Every package `go list -deps -test ./...` reports, for
// every guard platform with cgo off and on, may import sealed only if
// sealedImporterAllowed names it.
//
// The same scan is then run over an -overlay probe subpackage,
// internal/exec/zzsub, that imports sealed, and it must be reported there, so
// a clean result is shown to come from the module and not from a matcher
// that sees nothing.
//
// Mutation that turns it red: a real internal/exec/zzsub/probe.go importing
// sealed (the overlay probe is that file without touching the tree).
func TestOnlyExecImportsSealed(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	for _, imp := range sealedImporters(t, root, "") {
		t.Errorf("%s imports %s; only internal/exec may hold a sealed payload, so fold the code into internal/exec or add it to sealedImporterAllowed after review",
			imp, sealedImportPath)
	}

	backing := filepath.Join(t.TempDir(), "probe.go")
	src := "package zzsub\n\nimport \"" + sealedImportPath + "\"\n\nvar _ = sealed.New\n"
	if err := os.WriteFile(backing, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	overlay, err := json.Marshal(map[string]map[string]string{
		"Replace": {filepath.Join(root, "internal", "exec", "zzsub", "probe.go"): backing},
	})
	if err != nil {
		t.Fatal(err)
	}
	overlayPath := filepath.Join(t.TempDir(), "overlay.json")
	if err := os.WriteFile(overlayPath, overlay, 0o600); err != nil {
		t.Fatal(err)
	}
	probe := execImportPath + "/zzsub"
	if got := sealedImporters(t, root, overlayPath); !slices.Contains(got, probe) {
		t.Fatalf("the scan did not report the probe %s importing sealed (got %q); the matcher is broken, not the module clean", probe, got)
	}
}

// sealedImporters returns, sorted and deduplicated, every package outside
// sealedImporterAllowed that imports sealed, across every guard platform
// with cgo off and on, reading the go tool under pinnedGoEnv and, when
// overlay is non-empty, with -overlay.
func sealedImporters(t *testing.T, root, overlay string) []string {
	t.Helper()
	strip := func(path string) string {
		base, _, _ := strings.Cut(path, " ")
		return base
	}
	type run struct {
		out []byte
		err error
	}
	var runs []*run
	var wg sync.WaitGroup
	for _, p := range guardPlatforms {
		for _, cgo := range []string{"0", "1"} {
			r := &run{}
			runs = append(runs, r)
			wg.Go(func() {
				args := []string{"list", "-deps", "-test", "-json=ImportPath,Imports"}
				if overlay != "" {
					args = append(args, "-overlay", overlay)
				}
				cmd := osexec.CommandContext(t.Context(), "go", append(args, "./...")...) //nolint:gosec // G204: the go tool with fixed arguments and an overlay path this test wrote
				cmd.Dir = root
				cmd.Env = append(append(os.Environ(), pinnedGoEnv...), "GOOS="+p.goos, "GOARCH="+p.goarch, "CGO_ENABLED="+cgo)
				var stderr bytes.Buffer
				cmd.Stderr = &stderr
				r.out, r.err = cmd.Output()
				if r.err != nil {
					r.err = fmt.Errorf("go list for %s cgo=%s: %w\n%s", p, cgo, r.err, stderr.String())
				}
			})
		}
	}
	wg.Wait()
	found := map[string]bool{}
	listed := 0
	for _, r := range runs {
		if r.err != nil {
			t.Fatal(r.err)
		}
		dec := json.NewDecoder(bytes.NewReader(r.out))
		for {
			var pkg struct {
				ImportPath string
				Imports    []string
			}
			if err := dec.Decode(&pkg); errors.Is(err, io.EOF) {
				break
			} else if err != nil {
				t.Fatal(err)
			}
			listed++
			if sealedImporterAllowed[strip(pkg.ImportPath)] {
				continue
			}
			if slices.ContainsFunc(pkg.Imports, func(imp string) bool { return strip(imp) == sealedImportPath }) {
				found[strip(pkg.ImportPath)] = true
			}
		}
	}
	if listed == 0 {
		t.Fatal("go list reported no packages; the scan is broken, not the module clean")
	}
	out := make([]string, 0, len(found))
	for imp := range found {
		out = append(out, imp)
	}
	slices.Sort(out)
	return out
}
