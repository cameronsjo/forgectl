package exec

import (
	"go/ast"
	"go/build"
	"go/build/constraint"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// buildConstraintAllowed is the allowlist of production files, relative to
// the module root, that may carry a build constraint no guard configuration
// satisfies. It is empty: every production file builds on some guard
// platform today (forgectl#897), and a new entry must name why the file
// needs code no guard type-checks.
var buildConstraintAllowed = map[string]string{}

// unixGOOS is go/build's unix set: the GOOS values the "unix" build tag
// matches. guardConfigTags reads it only for the platforms in guardPlatforms.
var unixGOOS = map[string]bool{
	"aix": true, "android": true, "darwin": true, "dragonfly": true, "freebsd": true, "hurd": true,
	"illumos": true, "ios": true, "linux": true, "netbsd": true, "openbsd": true, "solaris": true,
}

// guardConfig is one configuration the guards type-check: a platform in
// guardPlatforms with cgo off or on.
type guardConfig struct {
	p   guardPlatform
	cgo bool
}

// guardConfigTags is the set of build tags that hold under c: GOOS, GOARCH,
// "unix" where it applies, "gc", "cgo" with cgo on, the toolchain's release
// tags, and guardContext's tool tags (the architecture baseline and the
// goexperiment tags), which is what the go tool sets for a release build.
func guardConfigTags(c guardConfig) map[string]bool {
	tags := map[string]bool{c.p.goos: true, c.p.goarch: true, "gc": true}
	if unixGOOS[c.p.goos] {
		tags["unix"] = true
	}
	if c.cgo {
		tags["cgo"] = true
	}
	ctx := guardContext(c.p)
	for _, tag := range slices.Concat(build.Default.ReleaseTags, ctx.ToolTags) {
		tags[tag] = true
	}
	return tags
}

// TestEveryProductionFileBuildsOnAGuardConfiguration closes the gap
// forgectl#897 names: the typed guard type-checks exactly what the go tool
// compiles for guardPlatforms, cgo off and on, so a production file tagged
// for any other GOOS or GOARCH, or behind a custom tag (//go:build
// forgectl_dev), is never type-checked, and a local build with that tag or
// platform compiles code every guard skipped. None of that ships (the
// release platforms are guard platforms, TestGuardPlatformsCoverReleaseTargets),
// so this pins the gap shut rather than chasing a live leak.
//
// It walks the module tree for every non-test .go file, as the tree walk in
// TestModuleTreeHidesNoGoSource does (skipping another module, and the
// dot-directories and testdata that test refuses source in), and refuses one
// that no guard configuration builds, outside buildConstraintAllowed. A file
// builds under a configuration when its name's GOOS/GOARCH suffix matches
// (build.Context.MatchFile over a bare package clause, so only the name
// decides), its //go:build expression holds under guardConfigTags (read with
// go/build/constraint, a // +build line only when there is no //go:build),
// and, for a file that imports "C", cgo is on.
//
// That evaluator is itself pinned against the go tool: every production file
// it says some configuration builds must be in moduleCompiledFiles when go
// list compiles its directory at all (a package nothing imports, under a
// directory ./... skips, is dead code, not a disagreement), and every one
// moduleCompiledFiles lists must be one it says so of. A missing
// tag in guardConfigTags, or a filename rule it gets wrong, shows up there
// as a disagreement rather than as a quiet pass.
//
// Mutations that turn it red: a production file internal/tmux/zz_probe.go
// tagged //go:build forgectl_zz; the same file tagged //go:build 386; the
// same file untagged but named zz_probe_plan9.go; drop "unix" from
// guardConfigTags (every _unix.go file disagrees with go list).
func TestEveryProductionFileBuildsOnAGuardConfiguration(t *testing.T) {
	rootDir, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(rootDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	fsys := root.FS()
	var paths []string
	err = fs.WalkDir(fsys, ".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if path == "." {
				return nil
			}
			name := entry.Name()
			if name == "testdata" || strings.HasPrefix(name, ".") {
				return fs.SkipDir
			}
			if _, statErr := fs.Stat(fsys, path+"/go.mod"); statErr == nil {
				return fs.SkipDir // another module
			}
			return nil
		}
		if entry.Type().IsRegular() && strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") {
			paths = append(paths, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var configs []guardConfig
	for _, p := range guardPlatforms {
		configs = append(configs, guardConfig{p, false}, guardConfig{p, true})
	}
	tagSets := make([]map[string]bool, len(configs))
	for i, c := range configs {
		tagSets[i] = guardConfigTags(c)
	}
	compiled, compiledDirs := map[string]bool{}, map[string]bool{}
	for _, f := range moduleCompiledFiles(t) {
		compiled[f.rel] = true
		compiledDirs[filepath.ToSlash(filepath.Dir(f.rel))] = true
	}
	fset := token.NewFileSet()
	builds := 0
	for _, path := range paths {
		src, err := fs.ReadFile(fsys, path)
		if err != nil {
			t.Fatal(err)
		}
		expr, cgo, err := fileBuildConstraint(fset, path, src)
		if err != nil {
			t.Errorf("%s: %v", path, err)
			continue
		}
		dir, name := filepath.Split(filepath.Join(rootDir, filepath.FromSlash(path)))
		ok := false
		for i, c := range configs {
			if (!cgo || c.cgo) && nameMatches(t, c.p, dir, name) && (expr == nil || expr.Eval(func(tag string) bool { return tagSets[i][tag] })) {
				ok = true
				break
			}
		}
		switch {
		case ok && !compiled[path] && compiledDirs[filepath.ToSlash(filepath.Dir(path))]:
			t.Errorf("%s: this evaluator says a guard configuration builds it, but go list compiles it for none; the evaluator is wrong, so its clean verdicts are not to be trusted", path)
		case !ok && compiled[path]:
			t.Errorf("%s: go list compiles it for a guard configuration, but this evaluator says none builds it; the evaluator is missing a tag or a filename rule", path)
		case !ok && buildConstraintAllowed[path] == "":
			t.Errorf("%s: no guard configuration (%d platforms, cgo off and on) builds it, so no guard type-checks it, and a local build with its tag or platform compiles code every guard skipped; retag it for a guard platform, delete it, or add it to buildConstraintAllowed with a reason only after review", path, len(guardPlatforms))
		}
		if ok {
			builds++
		}
	}
	if builds == 0 || !slices.Contains(paths, "internal/exec/mask.go") {
		t.Fatalf("walked %d production files, %d of them built; the walk is broken, not the module clean", len(paths), builds)
	}
}

// fileBuildConstraint returns src's build expression (nil without one) and
// whether it imports "C". The expression is its //go:build line, or, when it
// has none, the AND of its // +build lines, from the comments above the
// package clause, which is where the go tool reads them.
func fileBuildConstraint(fset *token.FileSet, path string, src []byte) (constraint.Expr, bool, error) {
	f, err := parser.ParseFile(fset, path, src, parser.ImportsOnly|parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return nil, false, err
	}
	var goBuild constraint.Expr
	var plusBuild []constraint.Expr
	for _, g := range f.Comments {
		if g.Pos() >= f.Package {
			break
		}
		for _, c := range g.List {
			switch {
			case constraint.IsGoBuild(c.Text):
				x, err := constraint.Parse(c.Text)
				if err != nil {
					return nil, false, err
				}
				goBuild = x
			case constraint.IsPlusBuild(c.Text):
				x, err := constraint.Parse(c.Text)
				if err != nil {
					return nil, false, err
				}
				plusBuild = append(plusBuild, x)
			}
		}
	}
	expr := goBuild
	if expr == nil {
		for _, x := range plusBuild {
			if expr == nil {
				expr = x
			} else {
				expr = &constraint.AndExpr{X: expr, Y: x}
			}
		}
	}
	cgo := slices.ContainsFunc(f.Imports, func(imp *ast.ImportSpec) bool {
		p, _ := strconv.Unquote(imp.Path.Value)
		return p == "C"
	})
	return expr, cgo, nil
}

// nameMatches reports whether the go tool's filename rules (a _GOOS, _GOARCH
// or _GOOS_GOARCH suffix, and a leading _ or . that excludes the file) admit
// name under p. It runs build.Context.MatchFile over a bare package clause,
// so the file's own build lines play no part: those are
// fileBuildConstraint's.
func nameMatches(t *testing.T, p guardPlatform, dir, name string) bool {
	t.Helper()
	ctx := guardContext(p)
	ctx.OpenFile = func(string) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("package p\n")), nil
	}
	ok, err := ctx.MatchFile(dir, name)
	if err != nil {
		t.Fatalf("match %s for %s: %v", name, p, err)
	}
	return ok
}
