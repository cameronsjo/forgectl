package cli

import (
	"fmt"
	"go/ast"
	"go/build"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

const termsafeImportPath = "github.com/cameronsjo/forgectl/internal/termsafe"

// uncappedAllowlist is every function in internal/cli that may use an
// uncapped termsafe.Safe* primitive, keyed "file.go:Func" ("file.go:Recv.Method"
// for a method), with the exact number of uses it may hold, so a new use in
// an allowlisted function is still caught. Each entry carries its reason.
var uncappedAllowlist = map[string]struct {
	uses   int
	reason string
}{
	"clean.go:cleanCommandFailure": {2, "escapes under its own head/tail cap (cleanDiagnosticMaxRunes); SafeLine is the escaping step, not the sink"},
	"clean.go:cleanDiagnostic":     {1, "escapes under its own head/tail cap (cleanDiagnosticMaxRunes); SafeLine is the escaping step, not the sink"},
	"clean.go:escapedPieces":       {1, "per-rune escape pieces for cleanDiagnostic's cap: one rune in, one escape out"},
	"update.go:writeStepDetail":    {1, "writes the transcript FILE, never the terminal: it is the whole recoverable detail the capped terminal line points at"},
	"update.go:writeOutputLines":   {1, "writes the transcript FILE, never the terminal: it is the whole recoverable detail the capped terminal line points at"},
	"k8s.go:writeK8sSafeLines":     {1, "passes kubectl describe/get/events output through line by line; the operator asked to see it whole, and a cut would corrupt it"},
	"y.go:newYLastCmd":             {1, "prints the operator's shell history for reuse; a cut would corrupt the command they copy"},
	"execute.go:Execute":           {1, "#911 in flight; sweep after it lands"},
	"execute.go:runHubVerb":        {1, "#911 in flight; sweep after it lands"},
}

// cappedHelpers may use an uncapped primitive because they ARE the capped
// boundary; none does today, and an entry here needs the same scrutiny as
// the allowlist.
var cappedHelpers = map[string]bool{}

// TestTextPrintersUseCappedHelpers is #913's source pin. Every text printer
// in internal/cli must escape AND bound an untrusted value, so no function
// here may use termsafe.SafeLine, or any future termsafe.Safe* primitive
// without "Max" in its name, except through the capped helpers in termcap.go
// or an allowlisted entry. Fixing uncapped sinks one at a time took three
// rounds (#889, #893, #912); this makes a fourth unnecessary.
//
// Uses are resolved through go/types (Info.Uses), not by name, so an alias
// (`f := termsafe.SafeLine`), a method value, or a function value passed as
// an argument is caught as surely as a direct call, and a local that merely
// shares the name is not. JSON encoding is out by construction: it goes
// through termsafe.JSONEncoder, which is not a Safe* primitive, so a --json
// path never reaches this pin. termsafe.Error and the Quote* family are not
// Safe* primitives either; they are capped on their own terms.
//
// The old package-local safeTerm alias is gone; a new wrapper like it is
// caught in its own body, which uses termsafe.SafeLine.
//
// Mutations that turn it red: print p.Title through termsafe.SafeLine in
// renderPRTable; bind `f := termsafe.SafeLine` in writeDrainHuman and print
// through f; add a second SafeLine use in writeK8sSafeLines (over its
// allowlisted count); cap the y.go history line (its entry goes stale).
func TestTextPrintersUseCappedHelpers(t *testing.T) {
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	termsafePkg := newTermsafeImporter(fset)
	seen := map[string]bool{}
	counts := map[string]int{}
	var total int
	for _, goos := range []string{"linux", "windows"} {
		ctx := build.Default
		ctx.GOOS = goos
		bp, err := ctx.ImportDir(dir, 0)
		if err != nil {
			t.Fatalf("list internal/cli for %s: %v", goos, err)
		}
		uses, err := uncappedUses(fset, termsafePkg, dir, bp.GoFiles)
		if err != nil {
			t.Fatal(err)
		}
		for _, u := range uses {
			if seen[u.pos] {
				continue
			}
			seen[u.pos] = true
			total++
			counts[u.fn]++
		}
	}
	if total == 0 {
		t.Fatal("found no uncapped use at all; the allowlisted ones exist, so the resolver is broken, not the package clean")
	}
	var keys []string
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, fn := range keys {
		if cappedHelpers[fn] {
			continue
		}
		entry, ok := uncappedAllowlist[fn]
		if !ok {
			t.Errorf("%s uses an uncapped termsafe.Safe* primitive %d time(s); print through a capped helper in termcap.go (safeLabel, safeTitle, safeSnippet, safeText, safePath, safeColumnPath)", fn, counts[fn])
			continue
		}
		if counts[fn] != entry.uses {
			t.Errorf("%s uses an uncapped primitive %d time(s), allowlisted for %d (%s); a new use needs a capped helper", fn, counts[fn], entry.uses, entry.reason)
		}
	}
	for fn, entry := range uncappedAllowlist {
		if strings.TrimSpace(entry.reason) == "" {
			t.Errorf("allowlist entry %s carries no reason", fn)
		}
		if counts[fn] == 0 {
			t.Errorf("allowlist entry %s matches no use; delete it", fn)
		}
	}
}

// uncappedUse is one use of an uncapped primitive: its position and the
// "file.go:Func" of its enclosing declaration.
type uncappedUse struct {
	pos string
	fn  string
}

// uncappedUses type-checks files in dir as internal/cli and returns every
// use of an uncapped termsafe.Safe* object. Only termsafe is imported for
// real; every other import is an empty stand-in, and the type errors that
// follow are ignored: an identifier's use resolves to termsafe's objects
// wherever it names them, which is all this needs, and it keeps the pin from
// type-checking the whole dependency graph.
func uncappedUses(fset *token.FileSet, termsafePkg types.ImporterFrom, dir string, names []string) ([]uncappedUse, error) {
	files := make([]*ast.File, 0, len(names))
	for _, name := range names {
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, err
		}
		files = append(files, f)
	}
	return uncappedUsesIn(fset, termsafePkg, files)
}

// newTermsafeImporter imports from source, for termsafe alone; fset must be
// the one the checked files are parsed into.
func newTermsafeImporter(fset *token.FileSet) types.ImporterFrom {
	return importer.ForCompiler(fset, "source", nil).(types.ImporterFrom)
}

func uncappedUsesIn(fset *token.FileSet, termsafePkg types.ImporterFrom, files []*ast.File) ([]uncappedUse, error) {
	imp := &pinImporter{real: termsafePkg, fake: map[string]*types.Package{}}
	info := &types.Info{Uses: map[*ast.Ident]types.Object{}}
	conf := types.Config{Importer: imp, Error: func(error) {}}
	_, _ = conf.Check("github.com/cameronsjo/forgectl/internal/cli", fset, files, info)
	if imp.termsafe == nil {
		return nil, fmt.Errorf("internal/cli did not import %s; the pin resolved nothing", termsafeImportPath)
	}

	var uses []uncappedUse
	for _, file := range files {
		fileName := filepath.Base(fset.Position(file.Pos()).Filename)
		for _, decl := range file.Decls {
			fn := fileName + ":" + declName(decl)
			ast.Inspect(decl, func(n ast.Node) bool {
				id, ok := n.(*ast.Ident)
				if !ok {
					return true
				}
				if obj := info.Uses[id]; obj != nil && isUncappedPrimitive(obj) {
					uses = append(uses, uncappedUse{pos: fset.Position(id.Pos()).String(), fn: fn})
				}
				return true
			})
		}
	}
	return uses, nil
}

// isUncappedPrimitive reports whether obj is a termsafe function named
// Safe* without "Max": the escaping primitives that leave length unbounded.
func isUncappedPrimitive(obj types.Object) bool {
	f, ok := obj.(*types.Func)
	if !ok || f.Pkg() == nil || f.Pkg().Path() != termsafeImportPath {
		return false
	}
	name := f.Name()
	return strings.HasPrefix(name, "Safe") && !strings.Contains(name, "Max")
}

// declName names a top-level declaration for the allowlist: a function by
// its name, a method as Recv.Method, anything else (a var block) as "var".
func declName(decl ast.Decl) string {
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

// pinImporter imports termsafe from source and stands in an empty package
// for every other import.
type pinImporter struct {
	real     types.ImporterFrom
	termsafe *types.Package
	fake     map[string]*types.Package
}

func (p *pinImporter) Import(importPath string) (*types.Package, error) {
	return p.ImportFrom(importPath, "", 0)
}

func (p *pinImporter) ImportFrom(importPath, dir string, mode types.ImportMode) (*types.Package, error) {
	if importPath == termsafeImportPath {
		if p.termsafe == nil {
			pkg, err := p.real.ImportFrom(importPath, dir, mode)
			if err != nil {
				return nil, err
			}
			p.termsafe = pkg
		}
		return p.termsafe, nil
	}
	if pkg, ok := p.fake[importPath]; ok {
		return pkg, nil
	}
	name := path.Base(importPath)
	if strings.HasPrefix(name, "v") && strings.Trim(name[1:], "0123456789") == "" && name != "v" {
		name = path.Base(path.Dir(importPath))
	}
	pkg := types.NewPackage(importPath, name)
	pkg.MarkComplete()
	p.fake[importPath] = pkg
	return pkg, nil
}

// TestUncappedUsesResolvesAliasesAndMethodValues is the pin's own control:
// it runs the resolver over a synthetic file holding each shape the pin must
// catch, and each it must not, and asserts the exact set.
//
// Mutations that turn it red: match identifiers by name (id.Name ==
// "SafeLine") instead of through info.Uses, and the shadowing local is
// flagged; count only a call's function position, and the alias, the
// function value and the package-level var are missed.
func TestUncappedUsesResolvesAliasesAndMethodValues(t *testing.T) {
	const src = `package cli

import (
	"fmt"
	ts "github.com/cameronsjo/forgectl/internal/termsafe"
)

type row struct{}

var alias = ts.SafeLine

func direct(s string) { fmt.Println(ts.SafeLine(s)) }

func viaAlias(s string) { f := ts.SafeLine; fmt.Println(f(s)) }

func asValue(xs []string) { apply(xs, ts.SafeLine) }

func (row) method(s string) string { return ts.SafeLine(s) }

func capped(s string) { fmt.Println(ts.SafeLineMax(s, 10)) }

func shadowed(s string) {
	SafeLine := func(v string) string { return v }
	fmt.Println(SafeLine(s))
}

func quoted(s string) { fmt.Println(ts.QuoteText(s)) }

func apply(xs []string, f func(string) string) {}
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "synthetic.go", src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	uses, err := uncappedUsesIn(fset, newTermsafeImporter(fset), []*ast.File{f})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, u := range uses {
		got = append(got, u.fn)
	}
	sort.Strings(got)
	want := []string{
		"synthetic.go:asValue",
		"synthetic.go:direct",
		"synthetic.go:row.method",
		"synthetic.go:var",
		"synthetic.go:viaAlias",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("uncapped uses = %q, want %q", got, want)
	}
}
