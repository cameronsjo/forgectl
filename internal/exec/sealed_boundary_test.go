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
// (*OSSensitiveRunner).RunSensitive. That what it starts was validated is the
// compiler's: startSealed accepts only a validated.Command, which only
// validated.New builds. That it is never used as a value is
// TestValidateDominatesStartSealed's. Test files are not checked.
//
// This is not what keeps plaintext out of internal/exec: the compiler does
// that, since sealed.Start returns only a *sealed.Proc (wait and kill, no
// accessor, pinned by the golden) and no exported name of sealed returns an
// *exec.Cmd. It keeps process launches through the seam to the runner, so a
// second launch path cannot appear without review. Both targets are
// package-level funcs, which can be reached only by naming them (a call or a
// func value), so a Uses walk sees every route; there is no interface or
// method-value shape that avoids it.
//
// Mutations that turn it red, each in sensitive.go: `var _ = sealed.Start`;
// `var _, _ = startSealed(&OSSensitiveRunner{}, validated.Command{}, nil,
// nil)`.
func TestSealedStartHasOneCaller(t *testing.T) {
	for _, p := range guardConfigs {
		c := checkExec(t, p)
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

// TestValidatedNewHasOneCaller pins, in internal/exec's production files on
// every platform in guardPlatforms, that validated.New is named only inside
// (SensitiveCommand).validated. New runs the path, argv and environment
// checks, but the kind, capture-mode and cap checks run in validated around
// it, so a second caller could build a Command those checks never saw.
// validated.New is a package-level func, reachable only by naming it, so a
// Uses walk sees every route; inside validated it must also be one direct
// call, never a func value (callShapeFindings, the rule startSealed has in
// RunSensitive). Test files are not checked.
//
// Mutations that turn it red: in sensitive.go, `var _, _ =
// validated.New(sealed.New("/bin/sh"), nil, nil, false)`; inside validated,
// `zzNew = validated.New` with a package var zzNew of its type.
func TestValidatedNewHasOneCaller(t *testing.T) {
	for _, p := range guardConfigs {
		c := checkExec(t, p)
		newFn, ok := c.validated.Scope().Lookup("New").(*types.Func)
		if !ok {
			t.Fatalf("[%s] validated declares no New func; the rule would check nothing", p)
		}
		for _, f := range namedOutside(c, newFn, "SensitiveCommand", "validated") {
			t.Errorf("[%s] %s: validated.New is named outside (SensitiveCommand).validated; build a Command through validated so every check runs",
				p, f)
		}
		for _, f := range callShapeFindings(c, newFn, "SensitiveCommand", "validated") {
			t.Errorf("[%s] %s", p, f)
		}
	}
}

// TestValidateDominatesStartSealed pins, on every platform in
// guardPlatforms, that validation cannot be skipped or outrun on the way to a
// process (forgectl#888). Most of that is the compiler's: startSealed
// accepts only a validated.Command, and the validated package's boundary lets
// only validated.New build a non-zero one, so moving the call above
// validation does not compile, a Command cannot be forged from a struct
// literal, and a write to sc after validation cannot reach the copy that is
// started. What is left for this test is the shape of the
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
	for _, p := range guardConfigs {
		for _, f := range startSealedCallFindings(checkExec(t, p)) {
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
	return callShapeFindings(c, startSealed, "*OSSensitiveRunner", "RunSensitive")
}

// callShapeFindings returns every way the func within (a method of recv when
// recv is not empty) breaks the call-shape rule for target: target is named
// there exactly once, as the function of a direct call (plain or package-
// qualified) outside every func literal, and never as a value. Uses outside
// within are namedOutside's to report.
func callShapeFindings(c *checkedPackage, target *types.Func, recv, within string) []string {
	var fd *ast.FuncDecl
	for _, f := range c.files {
		for _, decl := range f.Decls {
			d, ok := decl.(*ast.FuncDecl)
			if !ok || d.Name.Name != within || d.Body == nil {
				continue
			}
			if recv == "" && d.Recv == nil ||
				recv != "" && d.Recv != nil && len(d.Recv.List) == 1 && types.ExprString(d.Recv.List[0].Type) == recv {
				fd = d
			}
		}
	}
	if fd == nil {
		return []string{"no " + within + " with a body; the rule would check nothing"}
	}
	name := target.Name()
	var findings []string
	pos := func(n ast.Node) string { return c.fset.Position(n.Pos()).String() }
	callee := func(fun ast.Expr) *ast.Ident {
		switch f := ast.Unparen(fun).(type) {
		case *ast.Ident:
			return f
		case *ast.SelectorExpr:
			return f.Sel
		}
		return nil
	}
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
				if id := callee(n.Fun); id != nil && c.info.Uses[id] == target {
					if inLit {
						findings = append(findings, pos(n)+": "+name+" is called inside a func literal, which can be stored and run elsewhere")
					} else {
						calls++
					}
					for _, arg := range n.Args {
						walk(arg, inLit)
					}
					return false
				}
			case *ast.Ident:
				if c.info.Uses[n] == target {
					findings = append(findings, pos(n)+": "+name+" is used as a value; only a direct call in "+within+" may name it")
				}
			}
			return true
		})
	}
	walk(fd.Body, false)
	if calls != 1 {
		findings = append(findings, fmt.Sprintf("%s: %s calls %s directly %d times, want exactly 1", pos(fd), within, name, calls))
	}
	return findings
}

// TestValidatedCommandIsACopy pins that the validated.Command is a copy: a
// write to the caller's Args or Env backing array after validation reaches
// neither the Command nor the argv of the process startSealed starts from it
// (forgectl#888). It starts this test binary in
// its argv helper mode through startSealed and reads the argv back.
//
// A shallow copy is no longer expressible: SensitiveCommand.validated
// converts each element into a new []validated.Arg, and validated.New keeps
// only slices of its own (TestCommandKeepsOnlyWhatNewChecked mutation-tests
// that half). This end-to-end run pins that the process gets the Command's
// argv.
//
// Mutation that turns it red: startSealed passing vc.Args()[:0] as argv.
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
	if env := vc.Env(); len(env) != 1 || env[0].Key != ReplaceCmuxSocketPath("x").key {
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
// itself, validated (which checks sealed values), and the test binaries go
// test builds for sealed's own tests.
var sealedImporterAllowed = map[string]bool{
	execImportPath:             true,
	validatedImportPath:        true,
	sealedImportPath + ".test": true,
	sealedImportPath + "_test": true,
}

// validatedImporterAllowed is every package that may import
// internal/exec/internal/validated: internal/exec itself, and validated's own
// test binaries.
var validatedImporterAllowed = map[string]bool{
	execImportPath:                true,
	validatedImportPath + ".test": true,
	validatedImportPath + "_test": true,
}

// TestOnlyExecImportsSealed closes what the internal-package rule leaves open:
// that rule admits every package under internal/exec, so a future
// internal/exec/<sub> could import sealed and hold a payload in a package no
// other guard reads. Every package `go list -deps -test ./...` reports, for
// every guard platform with cgo off and on, may import sealed only if
// sealedImporterAllowed names it.
//
// The same scan is then run over an -overlay probe subpackage,
// internal/exec/zzsub, that imports sealed and validated, and it must be
// reported there, so a clean result is shown to come from the module and not
// from a matcher that sees nothing.
//
// Mutation that turns it red: a real internal/exec/zzsub/probe.go importing
// sealed (the overlay probe is that file without touching the tree).
func TestOnlyExecImportsSealed(t *testing.T) {
	checkOnlyExecImports(t, sealedImportPath, sealedImporterAllowed,
		"only internal/exec may hold a sealed payload, so fold the code into internal/exec or add it to sealedImporterAllowed after review")
}

// TestOnlyExecImportsValidated is TestOnlyExecImportsSealed for
// internal/exec/internal/validated (forgectl#888): only internal/exec may
// build the command startSealed starts, so a subpackage cannot become a
// second place that feeds one.
//
// Mutation that turns it red: a real internal/exec/zzsub/probe.go importing
// validated.
func TestOnlyExecImportsValidated(t *testing.T) {
	checkOnlyExecImports(t, validatedImportPath, validatedImporterAllowed,
		"only internal/exec may build a validated command, so fold the code into internal/exec or add it to validatedImporterAllowed after review")
}

// checkOnlyExecImports reports every importer of target outside allowed, from
// the shared module scan, and fails when the shared overlay probe that imports
// target is not reported.
func checkOnlyExecImports(t *testing.T, target string, allowed map[string]bool, why string) {
	t.Helper()
	scan := scanImports(t)
	for _, imp := range importersOf(scan.module, target, allowed) {
		t.Errorf("%s imports %s; %s", imp, target, why)
	}
	probe := execImportPath + "/zzsub"
	if got := importersOf(scan.probed, target, allowed); !slices.Contains(got, probe) {
		t.Fatalf("the scan did not report the probe %s importing %s (got %q); the matcher is broken, not the module clean", probe, target, got)
	}
}

// importScan is the import graph `go list -deps -test ./...` reports, once
// for the module as it is and once with the overlay probe added: package
// import path (test-variant suffix removed) to the paths it imports.
type importScan struct{ module, probed map[string][]string }

var (
	importScanOnce   sync.Once
	importScanResult importScan
	importScanErr    error
)

// scanImports runs the two import-graph listings once per test binary; both
// importer guards read them.
func scanImports(t *testing.T) importScan {
	t.Helper()
	importScanOnce.Do(func() {
		root, err := filepath.Abs(filepath.Join("..", ".."))
		if err != nil {
			importScanErr = err
			return
		}
		dir, err := os.MkdirTemp("", "zzsub-probe")
		if err != nil {
			importScanErr = err
			return
		}
		defer func() { _ = os.RemoveAll(dir) }()
		backing := filepath.Join(dir, "probe.go")
		src := "package zzsub\n\nimport (\n\t\"" + sealedImportPath + "\"\n\t\"" + validatedImportPath +
			"\"\n)\n\nvar _ = sealed.New\n\nvar _ = validated.New\n"
		if err := os.WriteFile(backing, []byte(src), 0o600); err != nil {
			importScanErr = err
			return
		}
		overlay, err := json.Marshal(map[string]map[string]string{
			"Replace": {filepath.Join(root, "internal", "exec", "zzsub", "probe.go"): backing},
		})
		if err != nil {
			importScanErr = err
			return
		}
		overlayPath := filepath.Join(dir, "overlay.json")
		if err := os.WriteFile(overlayPath, overlay, 0o600); err != nil {
			importScanErr = err
			return
		}
		if importScanResult.module, err = importGraph(root, ""); err != nil {
			importScanErr = err
			return
		}
		importScanResult.probed, importScanErr = importGraph(root, overlayPath)
	})
	if importScanErr != nil {
		t.Fatal(importScanErr)
	}
	return importScanResult
}

// importersOf returns, sorted, every package in graph outside allowed that
// imports target.
func importersOf(graph map[string][]string, target string, allowed map[string]bool) []string {
	var out []string
	for pkg, imports := range graph {
		if !allowed[pkg] && slices.Contains(imports, target) {
			out = append(out, pkg)
		}
	}
	slices.Sort(out)
	return out
}

// importGraph lists every package across every guard platform with cgo off
// and on, under pinnedGoEnv and, when overlay is non-empty, with -overlay,
// and returns each package's imports, test-variant suffixes removed.
func importGraph(root, overlay string) (map[string][]string, error) {
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
				cmd := osexec.Command("go", append(args, "./...")...) //nolint:gosec,noctx // G204: the go tool with fixed arguments and an overlay path this test wrote; run once per test binary, outside any one test's context
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
	graph := map[string][]string{}
	for _, r := range runs {
		if r.err != nil {
			return nil, r.err
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
				return nil, err
			}
			path := strip(pkg.ImportPath)
			for _, imp := range pkg.Imports {
				if imp = strip(imp); !slices.Contains(graph[path], imp) {
					graph[path] = append(graph[path], imp)
				}
			}
			if _, ok := graph[path]; !ok {
				graph[path] = nil
			}
		}
	}
	if len(graph) == 0 {
		return nil, errors.New("go list reported no packages; the scan is broken, not the module clean")
	}
	return graph, nil
}
