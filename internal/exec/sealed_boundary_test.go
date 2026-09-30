package exec

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
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
// the func startSealed, and startSealed is named only inside
// (*OSSensitiveRunner).RunSensitive. That what it starts was validated is
// the validatedCommand type state's (TestOnlyValidatedBuildsAValidatedCommand),
// and that it is never used as a value is TestValidateDominatesStartSealed's.
// Test files are not checked.
//
// This is not what keeps plaintext out of internal/exec: the compiler does
// that, since sealed.Start returns only a *sealed.Proc (wait and kill, no
// accessor, pinned by the golden) and no exported name of sealed returns an
// *exec.Cmd. It keeps process launches through the seam to the runner, so a
// second launch path cannot appear without review. Both targets are package-level funcs, which can be reached only by
// naming them (a call or a func value), so a Uses walk sees every route;
// there is no interface or method-value shape that avoids it.
//
// Mutations that turn it red, each in sensitive.go: `var _ = sealed.Start`;
// `var _, _ = startSealed(&OSSensitiveRunner{}, validatedCommand{}, nil,
// nil)`.
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
			t.Errorf("[%s] %s: startSealed is named outside (*OSSensitiveRunner).RunSensitive; launch sealed processes through the runner",
				p, f)
		}
	}
}

// TestValidateDominatesStartSealed pins, on every platform in
// guardPlatforms, that validation cannot be skipped or outrun on the way to a
// process (forgectl#888). Most of that is now the compiler's: startSealed
// accepts only a validatedCommand, and TestOnlyValidatedBuildsAValidatedCommand
// pins that only SensitiveCommand.validated builds one, so moving the call
// above validation does not compile, and a write to sc after it cannot reach
// the copy that is started. What is left for this test is the shape of the
// one call: inside (*OSSensitiveRunner).RunSensitive, startSealed is named
// exactly once, as the function of a direct call outside every func literal,
// so it is never a func value, never captured by a closure and never stored
// where a launch could skip the runner's pipes and bounds.
//
// Mutations that turn it red, each in RunSensitive: add `zzEsc = func()
// (*sealed.Proc, error) { return startSealed(r, vc, nil, nil) }` with a
// package var zzEsc; add `zzEscV = startSealed` with a package var zzEscV of
// its type.
func TestValidateDominatesStartSealed(t *testing.T) {
	for _, p := range guardPlatforms {
		for _, f := range startSealedCallFindings(checkExecFor(t, p)) {
			t.Errorf("[%s] %s", p, f)
		}
	}
}

// startSealedCallFindings returns every way c's RunSensitive breaks the rule
// TestValidateDominatesStartSealed states.
func startSealedCallFindings(c *checkedPackage) []string {
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
	calls := 0
	var walk func(root ast.Node, inLit bool)
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
						findings = append(findings, pos(n)+": startSealed is called inside a func literal, which can be stored and run outside the runner")
					} else {
						calls++
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
			}
			return true
		})
	}
	walk(fd.Body, false)
	if calls != 1 {
		findings = append(findings, fmt.Sprintf("%s: RunSensitive calls startSealed directly %d times, want exactly 1", pos(fd), calls))
	}
	return findings
}

// TestOnlyValidatedBuildsAValidatedCommand pins the constructor half of the
// type state startSealed relies on (forgectl#888), in internal/exec's
// production files on every platform in guardPlatforms: the type
// validatedCommand, and each of its fields, is named only inside
// (SensitiveCommand).validated, which builds it from a validated copy, and
// startSealed, which reads it. So no other code can build one (a composite
// literal, a var declaration, new, a conversion) or rewrite one it was
// handed, and whatever startSealed starts came out of validated.
//
// Mutations that turn it red, each in sensitive_run.go: in RunSensitive,
// `vc = validatedCommand{path: sc.Path}` after validation; `vc.args =
// sc.Args` after validation.
func TestOnlyValidatedBuildsAValidatedCommand(t *testing.T) {
	doors := []funcRef{{recv: "SensitiveCommand", name: "validated"}, {name: "startSealed"}}
	for _, p := range guardPlatforms {
		c := checkExecFor(t, p)
		tn, ok := c.pkg.Scope().Lookup("validatedCommand").(*types.TypeName)
		if !ok {
			t.Fatalf("[%s] internal/exec declares no validatedCommand type; the rule would check nothing", p)
		}
		st, ok := tn.Type().Underlying().(*types.Struct)
		if !ok || st.NumFields() == 0 {
			t.Fatalf("[%s] validatedCommand is not a struct with fields; the rule would check nothing", p)
		}
		targets := []types.Object{tn}
		for f := range st.Fields() {
			targets = append(targets, f)
		}
		for _, target := range targets {
			for _, f := range usedOutside(c, target, doors) {
				t.Errorf("[%s] %s: %s is named outside validated and startSealed; only validated may build a validatedCommand",
					p, f, target.Name())
			}
		}
	}
}

// funcRef names a func declaration: a method of recv (its receiver type
// expression, pointer star included) or, when recv is empty, a plain func.
type funcRef struct{ recv, name string }

func (r funcRef) matches(fd *ast.FuncDecl) bool {
	if fd.Name.Name != r.name {
		return false
	}
	if r.recv == "" {
		return fd.Recv == nil
	}
	return fd.Recv != nil && len(fd.Recv.List) == 1 && types.ExprString(fd.Recv.List[0].Type) == r.recv
}

// usedOutside returns the position of every use of target in c's production
// files outside the funcs doors names, plus one extra finding when no door
// uses it at all, since then the matcher, not the package, is what is clean.
func usedOutside(c *checkedPackage, target types.Object, doors []funcRef) []string {
	var findings []string
	inside := 0
	for _, f := range c.files {
		for _, decl := range f.Decls {
			fd, isFunc := decl.(*ast.FuncDecl)
			door := isFunc && slices.ContainsFunc(doors, func(r funcRef) bool { return r.matches(fd) })
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
		findings = append(findings, "(no use inside the allowed funcs at all: the matcher is broken, not the package clean)")
	}
	return findings
}

// TestValidatedCommandIsACopy pins that validated copies before it checks:
// a write to the caller's Args or Env backing array after validation
// reaches neither the validatedCommand nor the argv of the process
// startSealed starts from it (forgectl#888). It starts this test binary in
// its argv helper mode through startSealed and reads the argv back.
//
// Mutation that turns it red: in validated, drop the two slices.Clone lines
// (a shallow copy shares the caller's backing arrays).
func TestValidatedCommandIsACopy(t *testing.T) {
	runner, self := helperRunner(t, "argv", defaultRetireBound)
	sc := helperCommand(KindCmuxProbe, self, 4096, ReplaceCmuxSocketPath("/validated/socket"))
	sc.Args = []Arg{Opaque("validated-arg")}
	vc, err := sc.validated()
	if err != nil {
		t.Fatalf("validated: %v", err)
	}
	sc.Args[0] = Opaque("mutated-arg")
	sc.Env[0] = UnsetTmux()
	if len(vc.env) != 1 || vc.env[0].key != ReplaceCmuxSocketPath("x").key {
		t.Errorf("a write to sc.Env after validation reached the validated command's env")
	}

	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	proc, err := startSealed(runner, vc, outW, errW)
	closeAll(outW, errW)
	if err != nil {
		closeAll(outR, errR)
		t.Fatalf("startSealed: %v", err)
	}
	out, readErr := io.ReadAll(outR)
	closeAll(outR, errR)
	if err := proc.Wait(); err != nil {
		t.Fatalf("helper exited: %v", err)
	}
	if readErr != nil {
		t.Fatal(readErr)
	}
	argv := strings.Split(string(out), "\x00")
	if !slices.Equal(argv[1:], []string{"validated-arg"}) {
		t.Errorf("started argv = %q, want the validated argument only; a write after validation reached the process", argv[1:])
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
