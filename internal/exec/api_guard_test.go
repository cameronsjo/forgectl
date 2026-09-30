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
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// updateAPI regenerates testdata/exported_api*.golden from the live package.
// Run it only after reviewing the surface change it records.
var updateAPI = flag.Bool("update", false, "rewrite testdata/exported_api*.golden from internal/exec's API surface")

// guardPlatform is one GOOS/GOARCH pair the package guards type-check
// internal/exec for, with cgo off as every release build has it.
type guardPlatform struct{ goos, goarch string }

func (p guardPlatform) String() string { return p.goos + "/" + p.goarch }

// guardPlatforms is every platform the guards type-check. It must include
// every GOOS/GOARCH pair .goreleaser.yaml ships
// (TestGuardPlatformsCoverReleaseTargets), so a file tagged for any shipped
// platform, or for its architecture alone, is type-checked.
var guardPlatforms = []guardPlatform{
	{"linux", "amd64"},
	{"linux", "arm64"},
	{"darwin", "amd64"},
	{"darwin", "arm64"},
	{"windows", "amd64"},
	{"freebsd", "amd64"},
}

const (
	apiGoldenDir    = "testdata"
	apiGoldenShared = "exported_api.golden"
	apiRegenerate   = "go test ./internal/exec -run TestExportedAPI -update"
)

// pinnedDirs are the directories whose complete file list the golden pins,
// relative to this package, with the module-relative name each is shown as.
var pinnedDirs = []struct{ dir, shown string }{
	{".", "internal/exec"},
	{filepath.Join("..", "tmux", "tmuxesc"), "internal/tmux/tmuxesc"},
}

// TestExportedAPI pins, in testdata/exported_api.golden, the surface through
// which a sealed payload could reach code outside internal/exec:
//
//   - every non-test file in internal/exec and internal/tmux/tmuxesc,
//     whatever its build constraint, with that constraint and a cgo mark, so
//     a new or retagged file fails until reviewed. A .go file that no
//     platform in guardPlatforms compiles with cgo off (one for a platform
//     guardPlatforms lacks, a cgo file, a `//go:build ignore` file) is
//     refused outright rather than pinned, because no guard type-checks it
//     and once pinned its contents could change unseen (forgectl#854);
//     assembly and object files are refused module-wide by
//     TestNoFileReachesPastTheTypeSystem;
//   - every exported func, var and const, with its full type;
//   - every named type declared at package level, exported or not, with its
//     full underlying type (every field, embed and tag) and its method set on
//     both T and *T: every method declared in this package, and every
//     exported method promoted from elsewhere.
//
// It is computed once per platform in guardPlatforms. When every platform
// agrees there is one shared golden; when a surface legitimately differs,
// -update writes exported_api_<goos>_<goarch>.golden per platform instead,
// and each platform is held to its own file.
//
// The golden records the surface; it does not judge it. Any change to it (a
// callback parameter of whatever shape, an accessor that returns a payload,
// an exported func var, a new unexported type reachable through an exported
// signature) fails here, and deciding that the change hands no payload to
// caller code is the review that precedes -update.
//
// Mutations that turn it red, each written in a production file:
//
//   - func (a Arg) Peek() string { return a.reveal() }  (an accessor)
//   - var Hook func(string) string                     (an exported func var)
//   - func Inspect(a Arg, v any)                        (an any parameter)
//   - type hook func() becoming func(string), used by an exported func
//     (an unexported named type's underlying type)
//   - an unexported embed inner{Secret string} in an exported struct
//   - func WinOnly() {} in a new x_windows.go            (a windows-only export)
//   - func ArmOnly() {} in a new x_arm64.go              (an arm64-only export)
//   - a new file that imports "C"                       (a cgo-only file)
//   - func Only386() {} in a new x_386.go, even after -update pins it
//     (a file no guard platform compiles)
func TestExportedAPI(t *testing.T) {
	files := renderPinnedFiles(t)
	got := map[guardPlatform]string{}
	for _, p := range guardPlatforms {
		got[p] = files + renderAPI(checkExecFor(t, p).pkg)
	}
	if *updateAPI {
		writeAPIGoldens(t, got)
		return
	}
	for _, p := range guardPlatforms {
		name := apiGoldenName(p)
		want, err := os.ReadFile(filepath.Clean(filepath.Join(apiGoldenDir, name)))
		if errors.Is(err, os.ErrNotExist) {
			name = apiGoldenShared
			want, err = os.ReadFile(filepath.Clean(filepath.Join(apiGoldenDir, name)))
		}
		if err != nil {
			t.Fatalf("read golden for %s: %v; after reviewing the surface, create it with: %s", p, err, apiRegenerate)
		}
		if diff := lineDiff(string(want), got[p]); diff != "" {
			t.Errorf("%s: internal/exec's API surface differs from testdata/%s:\n%s\n"+
				"Every name and file here is a way for a sealed payload to reach caller code. Review the change "+
				"(no callback parameter, no accessor or func var that yields a payload, no file that escapes the "+
				"type-checked platforms), then regenerate with:\n\t%s",
				p, name, diff, apiRegenerate)
		}
	}
}

func apiGoldenName(p guardPlatform) string {
	return "exported_api_" + p.goos + "_" + p.goarch + ".golden"
}

// writeAPIGoldens writes one shared golden when every platform agrees, and
// one golden per platform otherwise, removing whichever form no longer
// applies.
func writeAPIGoldens(t *testing.T, got map[guardPlatform]string) {
	t.Helper()
	if err := os.MkdirAll(apiGoldenDir, 0o750); err != nil {
		t.Fatal(err)
	}
	first := got[guardPlatforms[0]]
	shared := true
	for _, p := range guardPlatforms {
		shared = shared && got[p] == first
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
	for _, p := range guardPlatforms {
		if shared {
			remove(apiGoldenName(p))
		} else {
			write(apiGoldenName(p), got[p])
		}
	}
	if shared {
		write(apiGoldenShared, first)
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

// renderPinnedFiles lists every non-test file of each pinned directory, one
// line per file. A .go file carries its //go:build line (none: "any") and a
// cgo mark when it imports "C"; its name carries any GOOS/GOARCH suffix.
func renderPinnedFiles(t *testing.T) string {
	t.Helper()
	var lines []string
	for _, d := range pinnedDirs {
		entries, err := os.ReadDir(d.dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || strings.HasSuffix(name, "_test.go") {
				continue
			}
			line := "file " + d.shown + "/" + name
			if strings.HasSuffix(name, ".go") {
				if !compiledByAGuardPlatform(t, d.dir, name) {
					t.Errorf("%s/%s is compiled by no platform in guardPlatforms (cgo off), so no guard type-checks it "+
						"and pinning it would let its contents change unseen; delete it, or retag it so a platform in "+
						"guardPlatforms compiles it, or add its platform to guardPlatforms (every platform .goreleaser.yaml "+
						"ships must be there, but a guarded platform need not ship)", d.shown, name)
				}
				constraint, cgo := fileConstraint(t, filepath.Join(d.dir, name))
				line += " build=" + constraint
				if cgo {
					line += " cgo"
				}
			}
			lines = append(lines, line)
		}
	}
	if len(lines) == 0 {
		t.Fatal("listed no files in the pinned directories; the listing is broken")
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n") + "\n"
}

// archBaselineTags is the architecture-feature build tags the go tool sets
// for each guarded GOARCH at its default level (GOAMD64=v1, GOARM64=v8.0),
// which is what every release build uses since .goreleaser.yaml sets neither.
var archBaselineTags = map[string][]string{
	"amd64": {"amd64.v1"},
	"arm64": {"arm64.v8.0"},
}

// guardContext is the build.Context the guards read p through: cgo off as
// every release build has it, and tool tags built for p rather than copied
// from the host. build.Default.ToolTags carries the host's architecture
// level (amd64.v3 on a host built with GOAMD64=v3, or none of arm64's on an
// amd64 host), which would make a file tagged for a feature level match or
// miss by where the test runs. The toolchain's goexperiment tags are kept:
// they belong to the compiler, not the host. Use it for file selection
// (MatchFile, ImportDir on a local directory) only: go/build resolves an
// import path in module mode only for a context with the default ToolTags.
func guardContext(p guardPlatform) build.Context {
	ctx := build.Default
	ctx.GOOS, ctx.GOARCH, ctx.CgoEnabled = p.goos, p.goarch, false
	var tags []string
	for _, tag := range build.Default.ToolTags {
		if strings.HasPrefix(tag, "goexperiment.") {
			tags = append(tags, tag)
		}
	}
	ctx.ToolTags = append(tags, archBaselineTags[p.goarch]...)
	return ctx
}

// compiledByAGuardPlatform reports whether some platform in guardPlatforms,
// with cgo off as every release build has it, compiles dir/name, reading both
// its GOOS/GOARCH file-name suffix and its //go:build line. A file none of
// them compiles is one no guard type-checks (forgectl#854, D-N1).
func compiledByAGuardPlatform(t *testing.T, dir, name string) bool {
	t.Helper()
	for _, p := range guardPlatforms {
		ctx := guardContext(p)
		ok, err := ctx.MatchFile(dir, name)
		if err != nil {
			t.Fatalf("match %s for %s: %v", filepath.Join(dir, name), p, err)
		}
		if ok {
			return true
		}
	}
	return false
}

// fileConstraint returns path's //go:build expression ("any" without one)
// and whether it imports "C".
func fileConstraint(t *testing.T, path string) (string, bool) {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly|parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	constraint := "any"
	for _, g := range f.Comments {
		if g.Pos() >= f.Package {
			break
		}
		for _, c := range g.List {
			if expr, ok := strings.CutPrefix(c.Text, "//go:build "); ok {
				constraint = strconv.Quote(strings.TrimSpace(expr))
			}
		}
	}
	cgo := slices.ContainsFunc(f.Imports, func(imp *ast.ImportSpec) bool { return imp.Path.Value == `"C"` })
	return constraint, cgo
}

// renderAPI spells pkg's surface as sorted lines: each exported func, var
// and const, and each package-level named type with its method set.
func renderAPI(pkg *types.Package) string {
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
			renderType(pkg, obj, qual, add)
		}
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n") + "\n"
}

// renderType spells a named type, exported or not: its declaration with the
// full underlying type (every field, embed and tag, since TypeString prints
// them all), then its method set on T and *T. Every method declared in pkg
// is listed; a method promoted from another package is listed when it is
// exported, because an unexported one there is callable by neither pkg nor
// its importers, and listing it would pin the standard library's internals.
func renderType(pkg *types.Package, obj *types.TypeName, qual types.Qualifier, add func(string, ...any)) {
	name := obj.Name()
	if obj.IsAlias() {
		add("type %s = %s", name, types.TypeString(types.Unalias(obj.Type()), qual))
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
	add("type %s %s", head, types.TypeString(under, qual))
	if _, isIface := under.(*types.Interface); isIface {
		return // an interface's methods are part of its type line
	}
	valueSet := types.NewMethodSet(named)
	ptrSet := types.NewMethodSet(types.NewPointer(named))
	for i := 0; i < ptrSet.Len(); i++ {
		m := ptrSet.At(i).Obj()
		if !m.Exported() && m.Pkg() != pkg {
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

// TestGuardPlatformsCoverReleaseTargets fails when .goreleaser.yaml ships a
// GOOS/GOARCH pair guardPlatforms does not type-check, or builds with cgo on
// (the guards type-check with cgo off).
//
// Mutation that turns it red: drop {"darwin", "arm64"} from guardPlatforms.
func TestGuardPlatformsCoverReleaseTargets(t *testing.T) {
	raw, err := os.ReadFile(filepath.Clean(filepath.Join("..", "..", ".goreleaser.yaml")))
	if err != nil {
		t.Fatal(err)
	}
	findings, pairs, err := releaseTargetFindings(raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range findings {
		t.Error(f)
	}
	if pairs == 0 {
		t.Fatal("found no goreleaser build targets; the parse is broken")
	}
}

// releaseTargetFindings reads a goreleaser config the way goreleaser does
// (forgectl#854, D-N2) and returns a finding for each build that ships a
// platform guardPlatforms does not type-check or builds with cgo on, plus the
// number of GOOS/GOARCH pairs it checked.
//
//   - A build's targets list, when present, is what it builds, and its goos
//     and goarch lists are ignored, so the targets are what is checked. A
//     target is GOOS_GOARCH with an optional variant suffix (linux_amd64_v1,
//     linux_arm_7); a target it cannot split that way, such as the
//     go_first_class shorthand, is a finding rather than a guess.
//   - env is applied in order, so the LAST CGO_ENABLED entry is the one the
//     build gets, and it must be 0.
func releaseTargetFindings(raw []byte) ([]string, int, error) {
	var cfg struct {
		Builds []struct {
			ID      string   `yaml:"id"`
			Env     []string `yaml:"env"`
			GOOS    []string `yaml:"goos"`
			GOARCH  []string `yaml:"goarch"`
			Targets []string `yaml:"targets"`
		} `yaml:"builds"`
	}
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return nil, 0, err
	}
	var findings []string
	pairs := 0
	check := func(id, goos, goarch string) {
		pairs++
		if !slices.Contains(guardPlatforms, guardPlatform{goos, goarch}) {
			findings = append(findings, fmt.Sprintf("goreleaser build %q ships %s/%s, which guardPlatforms does not type-check", id, goos, goarch))
		}
	}
	for _, b := range cfg.Builds {
		cgo := ""
		for _, kv := range b.Env {
			if v, ok := strings.CutPrefix(kv, "CGO_ENABLED="); ok {
				cgo = v
			}
		}
		if cgo != "0" {
			findings = append(findings, fmt.Sprintf("goreleaser build %q does not end with CGO_ENABLED=0 in env (the last entry wins); the guards type-check with cgo off", b.ID))
		}
		if len(b.Targets) > 0 {
			for _, target := range b.Targets {
				parts := strings.Split(target, "_")
				if len(parts) < 2 || parts[0] == "go" {
					findings = append(findings, fmt.Sprintf("goreleaser build %q names target %q, which is not GOOS_GOARCH; list the targets explicitly so the guard can check coverage", b.ID, target))
					continue
				}
				check(b.ID, parts[0], parts[1])
			}
			continue
		}
		if len(b.GOOS) == 0 || len(b.GOARCH) == 0 {
			findings = append(findings, fmt.Sprintf("goreleaser build %q names no goos or goarch; list them so the guard can check coverage", b.ID))
		}
		for _, goos := range b.GOOS {
			for _, goarch := range b.GOARCH {
				check(b.ID, goos, goarch)
			}
		}
	}
	return findings, pairs, nil
}

// TestReleaseTargetFindingsReadsGoreleaserAsGoreleaserDoes pins the two ways
// the tie-in used to under-read a config (forgectl#854, D-N2): a targets list
// beside goos/goarch takes precedence, so its platforms are what ship; and of
// two CGO_ENABLED entries the last one wins.
//
// Mutations that turn it red: check goos x goarch even when targets is set
// (windows/arm64 goes unseen, and linux/386 is reported); accept the build
// when any env entry is CGO_ENABLED=0 (the cgo row passes).
func TestReleaseTargetFindingsReadsGoreleaserAsGoreleaserDoes(t *testing.T) {
	tests := []struct {
		name  string
		yaml  string
		want  []string
		pairs int
	}{
		{
			name: "targets win over goos and goarch",
			yaml: "builds:\n  - id: t\n    env: [CGO_ENABLED=0]\n    goos: [linux]\n    goarch: [\"386\"]\n" +
				"    targets: [linux_amd64_v1, windows_arm64]\n",
			want:  []string{`goreleaser build "t" ships windows/arm64, which guardPlatforms does not type-check`},
			pairs: 2,
		},
		{
			name:  "the last CGO_ENABLED wins",
			yaml:  "builds:\n  - id: c\n    env: [CGO_ENABLED=0, CGO_ENABLED=1]\n    goos: [linux]\n    goarch: [amd64]\n",
			want:  []string{`goreleaser build "c" does not end with CGO_ENABLED=0 in env (the last entry wins); the guards type-check with cgo off`},
			pairs: 1,
		},
		{
			name:  "a later CGO_ENABLED=0 turns cgo back off",
			yaml:  "builds:\n  - id: ok\n    env: [CGO_ENABLED=1, CGO_ENABLED=0]\n    goos: [darwin]\n    goarch: [arm64]\n",
			pairs: 1,
		},
		{
			name:  "a shorthand target is not guessed at",
			yaml:  "builds:\n  - id: s\n    env: [CGO_ENABLED=0]\n    targets: [go_first_class]\n",
			want:  []string{`goreleaser build "s" names target "go_first_class", which is not GOOS_GOARCH; list the targets explicitly so the guard can check coverage`},
			pairs: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, pairs, err := releaseTargetFindings([]byte(tt.yaml))
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, tt.want) || pairs != tt.pairs {
				t.Errorf("findings = %q (%d pairs), want %q (%d pairs)", got, pairs, tt.want, tt.pairs)
			}
		})
	}
}

// checkedPackage is internal/exec's production files type-checked for one
// platform.
type checkedPackage struct {
	fset  *token.FileSet
	files []*ast.File
	info  *types.Info
	pkg   *types.Package
}

var checkedByPlatform = map[guardPlatform]*checkedPackage{}

// checkExecFor type-checks internal/exec's production files as they build
// for p with cgo off, resolving every import from source under the same
// build.Context so a platform-tagged dependency file is read too. Results
// are cached per platform for the test binary's life.
func checkExecFor(t *testing.T, p guardPlatform) *checkedPackage {
	t.Helper()
	if c, ok := checkedByPlatform[p]; ok {
		return c
	}
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	// internal/exec's own files are selected with p's tool tags, not the
	// host's. Its imports resolve through a context that keeps the default
	// tool tags, because go/build hands module-mode resolution to the go
	// command only for a context whose ToolTags are the default ones.
	own := guardContext(p)
	bp, err := own.ImportDir(dir, 0)
	if err != nil {
		t.Fatalf("list internal/exec for %s: %v", p, err)
	}
	fset := token.NewFileSet()
	files, err := parseGoFiles(fset, dir, bp.GoFiles)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatalf("parsed no production files of internal/exec for %s", p)
	}
	info := &types.Info{
		Types:      map[ast.Expr]types.TypeAndValue{},
		Selections: map[*ast.SelectorExpr]*types.Selection{},
	}
	ctx := build.Default
	ctx.GOOS, ctx.GOARCH, ctx.CgoEnabled = p.goos, p.goarch, false
	imp := &sourceImporter{ctx: &ctx, fset: fset, pkgs: map[string]*types.Package{}, errs: map[string]error{}}
	conf := types.Config{Importer: imp, Sizes: types.SizesFor("gc", ctx.GOARCH)}
	pkg, err := conf.Check(execImportPath, fset, files, info)
	if err != nil {
		t.Fatalf("type-check internal/exec for %s: %v", p, err)
	}
	c := &checkedPackage{fset: fset, files: files, info: info, pkg: pkg}
	checkedByPlatform[p] = c
	return c
}

// sourceImporter type-checks imports from source under one build.Context,
// function bodies skipped. go/importer's "source" importer is fixed to
// build.Default, so it would read a dependency's host-platform files.
type sourceImporter struct {
	ctx  *build.Context
	fset *token.FileSet
	pkgs map[string]*types.Package // nil value: import in progress
	errs map[string]error          // a failed import, returned again as is
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
	key := bp.ImportPath
	if err, ok := s.errs[key]; ok {
		return nil, err
	}
	if p, ok := s.pkgs[key]; ok {
		if p == nil {
			return nil, fmt.Errorf("import cycle through %s", key)
		}
		return p, nil
	}
	s.pkgs[key] = nil
	p, err := s.check(bp)
	if err != nil {
		delete(s.pkgs, key)
		s.errs[key] = err
		return nil, err
	}
	s.pkgs[key] = p
	return p, nil
}

func (s *sourceImporter) check(bp *build.Package) (*types.Package, error) {
	files, err := parseGoFiles(s.fset, bp.Dir, bp.GoFiles)
	if err != nil {
		return nil, err
	}
	conf := types.Config{Importer: s, IgnoreFuncBodies: true, Sizes: types.SizesFor("gc", s.ctx.GOARCH)}
	p, err := conf.Check(bp.ImportPath, s.fset, files, nil)
	if err != nil {
		return nil, fmt.Errorf("type-check %s for %s/%s: %w", bp.ImportPath, s.ctx.GOOS, s.ctx.GOARCH, err)
	}
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

// TestGuardContextIsHostIndependent pins that each guard platform's tool tags
// name that platform's own architecture level, and no other, whatever the
// host is.
//
// Mutation that turns it red: copy build.Default.ToolTags wholesale (on an
// amd64 host, the arm64 contexts carry amd64.v1 and lack arm64.v8.0).
func TestGuardContextIsHostIndependent(t *testing.T) {
	for _, p := range guardPlatforms {
		want, ok := archBaselineTags[p.goarch]
		if !ok {
			t.Errorf("%s: archBaselineTags has no entry for %s; add its default feature-level tag", p, p.goarch)
			continue
		}
		var arch []string
		for _, tag := range guardContext(p).ToolTags {
			if !strings.HasPrefix(tag, "goexperiment.") {
				arch = append(arch, tag)
			}
		}
		if !slices.Equal(arch, want) {
			t.Errorf("%s: architecture tool tags = %q, want %q", p, arch, want)
		}
	}
}
