package cli

import (
	"fmt"
	"go/ast"
	"go/build"
	"go/constant"
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
// uncapped termsafe primitive, keyed "file.go:Func" ("file.go:Recv.Method"
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

	// Review before trust (#782): a display that decides whether something
	// runs must show all of it, escaped. A cut would let a hostile value push
	// the tail of what runs behind the truncation marker.
	"workflow.go:printPlan":             {2, "workflow run --dry-run is the review before blessing (#782): the plan's name and version print whole"},
	"workflow.go:printField":            {1, "workflow run --dry-run is the review before blessing (#782): cmd, repo, ref and every other step field print whole"},
	"workflow.go:quoteEach":             {1, "workflow run --dry-run is the review before blessing (#782, #816): args and globs print whole, each quoted"},
	"resume.go:resumeSession":           {2, "resume --dry-run's exec line is the review of what resume would run (#782's rule): the binary and argv print whole"},
	"resume_hooks.go:printHooksPreview": {1, "the hooks preview names the hook commands that would fire (#782's rule): each prints whole"},

	// Machine-parseable fields (#832): QuotePathIfUnsafe leaves an ordinary
	// path verbatim so a caller parsing the column gets it back, and a cut
	// would rewrite a long but ordinary one.
	"pr.go:newPrListCmd":                  {1, "pr list field 3 is fed to pr teardown (#832): a cut would rewrite the path"},
	"pr_findings.go:newPrFindingsListCmd": {1, "findings list rows are tab-separated for scripts (#832): a cut would rewrite the path"},
	"pr_findings.go:runPrFindingsCleanup": {2, "pruned/reclaimed paths are one per line for scripts (#832): a cut would rewrite the path"},
	"pr_queue.go:newPrQueueCmd":           {1, "queue rows are tab-separated for scripts (#832): a cut would rewrite the path"},
	"pr_repair.go:writePruneHuman":        {2, "prune rows are tab-separated for scripts (#832): a cut would rewrite the path"},
	"pr_repair.go:runRepairHistory":       {1, "history rows are tab-separated for scripts (#832): a cut would rewrite the path"},
	"pr_repair.go:writeRepairHuman":       {1, "repair rows are tab-separated for scripts (#832): a cut would rewrite the path"},

	"audit.go:auditShowPath": {1, "QuoteText only as a predicate (does quoting change the path?); what it prints is the raw path or the capped QuotePath"},
}

// cappedHelpers may use an uncapped primitive because they ARE the capped
// boundary; none does today, and an entry here needs the same scrutiny as
// the allowlist.
var cappedHelpers = map[string]bool{}

// TestTextPrintersUseCappedHelpers is #913's source pin, extended by #928.
// Every text printer in internal/cli must escape AND bound an untrusted
// value, so no function here may use an uncapped termsafe primitive except
// through the capped helpers in termcap.go or an allowlisted entry. Fixing
// uncapped sinks one at a time took three rounds (#889, #893, #912); this
// makes a fourth unnecessary.
//
// Uncapped means: a termsafe function named Safe* or Quote* without "Max"
// (SafeLine, QuoteText, QuotePathIfUnsafe) other than QuotePath, which caps
// by default; and any use of a *Max function that is not a call whose last
// argument is a positive constant (SafeLineMax(s, 0) and SafeLineMax(s, n)
// for a variable n are both flagged).
//
// Uses are resolved through go/types (Info.Uses), not by name, so an alias
// (`f := termsafe.SafeLine`), a method value, or a function value passed as
// an argument is caught as surely as a direct call, and a local that merely
// shares the name is not. JSON encoding is out by construction: it goes
// through termsafe.JSONEncoder, which is neither Safe* nor Quote*, so a
// --json path never reaches this pin. termsafe.Error is out too: it caps the
// paths inside an error, not the error's whole text, so an error printed as a
// line still goes through safeText.
//
// The files are checked as they build for linux, darwin and windows.
//
// Mutations that turn it red: print p.Title through termsafe.SafeLine in
// renderPRTable; bind `f := termsafe.SafeLine` in writeDrainHuman and print
// through f; add a second SafeLine use in writeK8sSafeLines (over its
// allowlisted count); cap the y.go history line (its entry goes stale);
// print the resume outdated version through termsafe.QuoteText; call
// SafeLineMax with a 0 cap in renderPRTable.
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
	for _, goos := range []string{"linux", "darwin", "windows"} {
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
			t.Errorf("%s uses an uncapped termsafe primitive %d time(s); print through a capped helper in termcap.go (safeLabel, safeTitle, safeSnippet, safeText, safePath, safeColumnPath) or a *Max call with a positive constant cap", fn, counts[fn])
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
// uncapped use of a termsafe primitive. Only termsafe is imported for real;
// every other import is an empty stand-in, and the type errors that follow
// are ignored: an identifier's use resolves to termsafe's objects wherever
// it names them, and a cap built from a termsafe or package constant still
// evaluates, which is all this needs. It keeps the pin from type-checking
// the whole dependency graph.
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
	info := &types.Info{Uses: map[*ast.Ident]types.Object{}, Types: map[ast.Expr]types.TypeAndValue{}}
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
			// A *Max function named in call position with a positive constant
			// cap is capped; any other use of one (a zero or variable cap, a
			// function value) is not.
			cappedCall := map[*ast.Ident]bool{}
			ast.Inspect(decl, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || len(call.Args) == 0 {
					return true
				}
				if id := calleeIdent(call.Fun); id != nil && positiveConstant(info, call.Args[len(call.Args)-1]) {
					cappedCall[id] = true
				}
				return true
			})
			ast.Inspect(decl, func(n ast.Node) bool {
				id, ok := n.(*ast.Ident)
				if !ok {
					return true
				}
				obj := info.Uses[id]
				if obj == nil {
					return true
				}
				switch termsafeKind(obj) {
				case kindUncapped:
				case kindMax:
					if cappedCall[id] {
						return true
					}
				default:
					return true
				}
				uses = append(uses, uncappedUse{pos: fset.Position(id.Pos()).String(), fn: fn})
				return true
			})
		}
	}
	return uses, nil
}

// calleeIdent is the identifier a call's function position names: f in f(x),
// Sel in pkg.Sel(x).
func calleeIdent(fun ast.Expr) *ast.Ident {
	switch f := fun.(type) {
	case *ast.Ident:
		return f
	case *ast.SelectorExpr:
		return f.Sel
	case *ast.ParenExpr:
		return calleeIdent(f.X)
	}
	return nil
}

// positiveConstant reports whether e is an integer constant above zero.
func positiveConstant(info *types.Info, e ast.Expr) bool {
	tv, ok := info.Types[e]
	if !ok || tv.Value == nil || tv.Value.Kind() != constant.Int {
		return false
	}
	n, exact := constant.Int64Val(tv.Value)
	return exact && n > 0
}

type primitiveKind int

const (
	kindOther primitiveKind = iota
	kindUncapped
	kindMax
)

// termsafeKind classifies a termsafe function: Safe* and Quote* without
// "Max" are uncapped, except QuotePath, which caps at PathEchoMaxRunes by
// default; a *Max function is capped only by the cap it is called with.
func termsafeKind(obj types.Object) primitiveKind {
	f, ok := obj.(*types.Func)
	if !ok || f.Pkg() == nil || f.Pkg().Path() != termsafeImportPath {
		return kindOther
	}
	name := f.Name()
	if !strings.HasPrefix(name, "Safe") && !strings.HasPrefix(name, "Quote") {
		return kindOther
	}
	if strings.Contains(name, "Max") {
		return kindMax
	}
	if name == "QuotePath" {
		return kindOther
	}
	return kindUncapped
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
// function value and the package-level var are missed; make positiveConstant
// always true, and the zero and variable caps pass; classify QuotePath as
// uncapped, and pathQuoted is flagged.
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

func pathQuoted(s string) { fmt.Println(ts.QuotePath(s)) }

func quotedCapped(s string) { fmt.Println(ts.QuoteTextMax(s, 10)) }

const zero = 0

func zeroCap(s string) { fmt.Println(ts.SafeLineMax(s, zero)) }

func variableCap(s string, n int) { fmt.Println(ts.SafeLineMax(s, n)) }

func maxAsValue(xs []string) { applyMax(xs, ts.SafeLineMax) }

func applyMax(xs []string, f func(string, int) string) {}

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
		"synthetic.go:maxAsValue",
		"synthetic.go:quoted",
		"synthetic.go:row.method",
		"synthetic.go:var",
		"synthetic.go:variableCap",
		"synthetic.go:viaAlias",
		"synthetic.go:zeroCap",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("uncapped uses = %q, want %q", got, want)
	}
}
