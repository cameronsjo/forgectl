package termsafe_test

import (
	"errors"
	"fmt"
	"go/ast"
	"go/build"
	"go/constant"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

const (
	modulePath         = "github.com/cameronsjo/forgectl"
	termsafeImportPath = modulePath + "/internal/termsafe"
)

// maxConstantCap is the largest cap the pin accepts as a bound (#934). A cap
// is a promise about how much of the terminal one value can take, and a
// constant far past any screen keeps none: SafeLineMax(s, 1<<62) would
// otherwise pass as capped. 4096 runes is the largest cap in the tree
// (privdir's reasonMaxRunes, a filesystem-refusal reason carrying a path) and
// about fifty 80-column lines; a sink that needs more is printing something
// whole, which is an allowlist entry with its reason, not a cap.
const maxConstantCap = 4096

// skippedPackages are production directories the pin does not scan, each with
// its reason.
var skippedPackages = map[string]string{
	"internal/termsafe":              "the primitives themselves: SafeLineMax is SafeLine plus a cap, and QuotePath is QuotePathMax with the default",
	"internal/termsafe/termsafetest": "test support: its helpers build hostile fixtures and assert on them, and only tests import it",
}

// uncappedAllowlist is every function that may use an uncapped termsafe
// primitive, keyed "pkgdir/file.go:Func" ("pkgdir/file.go:Recv.Method" for a
// method) with pkgdir relative to the module root, holding the exact number
// of uses it may make, so a new use in an allowlisted function is still
// caught. Each entry carries its reason.
var uncappedAllowlist = map[string]struct {
	uses   int
	reason string
}{
	"internal/cli/clean.go:cleanCommandFailure": {2, "escapes under its own head/tail cap (cleanDiagnosticMaxRunes); SafeLine is the escaping step, not the sink"},
	"internal/cli/clean.go:cleanDiagnostic":     {1, "escapes under its own head/tail cap (cleanDiagnosticMaxRunes); SafeLine is the escaping step, not the sink"},
	"internal/cli/clean.go:escapedPieces":       {1, "per-rune escape pieces for cleanDiagnostic's cap: one rune in, one escape out"},
	"internal/cli/update.go:writeStepDetail":    {1, "writes the transcript FILE, never the terminal: it is the whole recoverable detail the capped terminal line points at"},
	"internal/cli/update.go:writeOutputLines":   {1, "writes the transcript FILE, never the terminal: it is the whole recoverable detail the capped terminal line points at"},
	"internal/cli/k8s.go:writeK8sSafeLines":     {1, "passes kubectl describe/get/events output through line by line; the operator asked to see it whole, and a cut would corrupt it"},
	"internal/cli/y.go:newYLastCmd":             {1, "prints the operator's shell history for reuse; a cut would corrupt the command they copy"},

	// Review before trust (#782): a display that decides whether something
	// runs must show all of it, escaped. A cut would let a hostile value push
	// the tail of what runs behind the truncation marker.
	"internal/cli/workflow.go:printPlan":             {2, "workflow run --dry-run is the review before blessing (#782): the plan's name and version print whole"},
	"internal/cli/workflow.go:printField":            {1, "workflow run --dry-run is the review before blessing (#782): cmd, repo, ref and every other step field print whole"},
	"internal/cli/workflow.go:quoteEach":             {1, "workflow run --dry-run is the review before blessing (#782, #816): args and globs print whole, each quoted"},
	"internal/cli/resume.go:resumeSession":           {2, "resume --dry-run's exec line is the review of what resume would run (#782's rule): the binary and argv print whole"},
	"internal/cli/resume_hooks.go:printHooksPreview": {1, "the hooks preview names the hook commands that would fire (#782's rule): each prints whole"},

	// Whole paths (#832): QuotePathIfUnsafe leaves an ordinary path verbatim
	// and escapes the rest uncut. pr list, findings and queue print columns a
	// caller parses back; the pr repair views name a path the operator acts on.
	"internal/cli/pr.go:newPrListCmd":                  {1, "pr list field 3 is fed to pr teardown (#832): a cut would rewrite the path"},
	"internal/cli/pr_findings.go:newPrFindingsListCmd": {1, "findings list rows are tab-separated for scripts (#832): a cut would rewrite the path"},
	"internal/cli/pr_findings.go:runPrFindingsCleanup": {2, "pruned/reclaimed paths are one per line for scripts (#832): a cut would rewrite the path"},
	"internal/cli/pr_queue.go:newPrQueueCmd":           {1, "queue rows are tab-separated for scripts (#832): a cut would rewrite the path"},
	"internal/cli/pr_repair.go:writePruneHuman":        {2, "prune's rows, --dry-run's included, name the set-aside records it unlinks and the log it rewrites; #832's design caps a path by default (QuotePath) and has a caller that must render the whole path quote it whole, and a path the operator approves destroying is one (#782)"},
	"internal/cli/pr_repair.go:runRepairHistory":       {1, "a history row's record path is the recovery pointer for what a repair moved or removed; #832's design caps a path by default (QuotePath) and has a caller that must render the whole path quote it whole, so a cut cannot point recovery at the wrong file"},
	"internal/cli/pr_repair.go:writeRepairHuman":       {1, "names the record each repair --apply would act on, shown before the operator applies it; #832's design caps a path by default (QuotePath) and has a caller that must render the whole path quote it whole, and this is one (#782)"},

	"internal/cli/audit.go:auditShowPath": {1, "QuoteText only as a predicate (does quoting change the path?); what it prints is the raw path or the capped QuotePath"},

	// Outside internal/cli (#934).
	"internal/k8s/logs.go:logLineTransform":                 {1, "streams kubectl logs line by line; the operator asked to see them whole, and a cut would corrupt them (as internal/cli writeK8sSafeLines)"},
	"internal/k8s/logs.go:safeLineTransform":                {1, "streams kubectl logs line by line; the operator asked to see them whole, and a cut would corrupt them (as internal/cli writeK8sSafeLines)"},
	"internal/k8s/logs.go:safeFragmentWriter.WriteFragment": {1, "streams a partial kubectl log line as it arrives; a per-fragment cap would cut the middle of a line the operator asked to see whole"},
	"internal/resume/hooks.go:outputTail":                   {1, "escapes under its own input cap (hookTailRunes, keeping the tail); SafeLine is the escaping step, not the sink"},
	"internal/tui/hub.go:capSafe":                           {1, "per-rune escape pieces under capSafe's own maxRunes cap: one rune in, one escape out"},
	"internal/tui/hub.go:tailSafe":                          {1, "per-rune escape pieces under tailSafe's own maxRunes cap: one rune in, one escape out"},
	"internal/tui/hub.go:displayToken":                      {3, "the \"$ forgectl …\" echo (DisplayArgv) shows exactly the argv that will run (#782's rule); a cut would hide part of what runs"},
	"internal/pr/repair.go:cappedRecordBytes":               {1, "escapes under its own byte cap (maxAuditRecordBytes, applied before and after); SafeLine is the escaping step, not the sink"},

	// Review before trust (#782) in pr repair: the confirmation prompts, and
	// the non-TTY refusals that name what --yes would approve or what the
	// operator must inspect by hand, show the path or ref whole.
	"internal/pr/prune.go:prunePrompt":                     {1, "the prune confirmation names the log it rewrites; a path the operator approves acting on is never cut"},
	"internal/pr/prune.go:Client.pruneLocked":              {1, "the non-TTY refusal names the log --yes would rewrite, whole like the prompt it stands in for"},
	"internal/pr/repair.go:rollbackPrompt":                 {3, "the rollback confirmation names the ref, clean room and record it removes; a cut would hide what is deleted"},
	"internal/pr/repair.go:setAsidePrompt":                 {1, "the set-aside confirmation names the record it renames; a path the operator approves acting on is never cut"},
	"internal/pr/repair.go:Client.repairRollbackLocked":    {2, "the refusals name the clean room --yes would remove, or the one to inspect by hand; whole like the prompt"},
	"internal/pr/repair.go:Client.repairUndecodableLocked": {1, "the non-TTY refusal names the record --yes would set aside, whole like the prompt it stands in for"},
}

// TestTextPrintersUseCappedHelpers is #913's source pin, extended by #928 and
// carried to every production package by #934. Every text a sink writes must
// escape AND bound an untrusted value, so no function may use an uncapped
// termsafe primitive except an allowlisted entry. Fixing uncapped sinks one
// at a time took three rounds (#889, #893, #912); this makes a fourth
// unnecessary. internal/cli reaches the capped forms through its helpers in
// termcap.go (safeLabel, safeTitle, safeSnippet, safeText, safePath,
// safeColumnPath); every other package calls a *Max form with its own
// constant cap.
//
// Uncapped means: a termsafe function named Safe* or Quote* without "Max"
// (SafeLine, QuoteText, QuotePathIfUnsafe) other than QuotePath, which caps
// by default; and any use of a *Max function that is not a call whose last
// argument is a constant cap in (0, maxConstantCap]. QuoteArgMax,
// QuotePathMax and SafePathMax read a cap below 1 as their default cap, so
// for them a constant below 1 is capped too; SafeLineMax and QuoteTextMax
// read it as no cap, so SafeLineMax(s, 0) is flagged, as are SafeLineMax(s,
// n) for a variable n and SafeLineMax(s, 1<<62).
//
// Uses are resolved through go/types (Info.Uses), not by name, so an alias
// (`f := termsafe.SafeLine`), a method value, or a function value passed as
// an argument is caught as surely as a direct call, and a local that merely
// shares the name is not. JSON encoding is out by construction: it goes
// through termsafe.JSONEncoder, which is neither Safe* nor Quote*, so a
// --json path never reaches this pin. termsafe.Error is out too: it caps the
// paths inside an error, not the error's whole text, which is why a text
// printer that renders one routes it through a capped form (safeText in
// internal/cli) rather than printing it with a bare %v; the pin sees only
// termsafe primitives, so a raw %v of an error is the helper-level tests'
// concern (TestErrorLinesAreCapped in internal/cli), not this one's.
//
// The files are checked as they build for linux, darwin and windows.
//
// Mutations that turn it red: print p.Title through termsafe.SafeLine in
// internal/cli renderPRTable; bind `f := termsafe.SafeLine` in
// writeDrainHuman and print through f; add a second SafeLine use in
// writeK8sSafeLines (over its allowlisted count); cap the y.go history line
// (its entry goes stale); print a tmux window name in internal/tmux through
// termsafe.SafeLine; call SafeLineMax with a 1<<62 cap in internal/tui.
func TestTextPrintersUseCappedHelpers(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	dirs, err := productionDirs(root)
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	termsafePkg := newTermsafeImporter(fset)
	seen := map[string]bool{}
	counts := map[string]int{}
	scanned := map[string]bool{}
	for _, rel := range dirs {
		dir := filepath.Join(root, filepath.FromSlash(rel))
		for _, goos := range []string{"linux", "darwin", "windows"} {
			ctx := build.Default
			ctx.GOOS = goos
			bp, err := ctx.ImportDir(dir, 0)
			var noGo *build.NoGoError
			if errors.As(err, &noGo) {
				continue
			}
			if err != nil {
				t.Fatalf("list %s for %s: %v", rel, goos, err)
			}
			if !importsTermsafe(bp.Imports) {
				continue
			}
			scanned[rel] = true
			uses, err := uncappedUses(fset, termsafePkg, dir, rel, bp.GoFiles)
			if err != nil {
				t.Fatal(err)
			}
			for _, u := range uses {
				if seen[u.pos] {
					continue
				}
				seen[u.pos] = true
				counts[u.fn]++
			}
		}
	}
	for _, want := range []string{"internal/cli", "internal/tui", "internal/pr", "internal/tmux"} {
		if !scanned[want] {
			t.Fatalf("%s was not scanned; the walk is broken, not the package clean", want)
		}
	}
	if len(counts) == 0 {
		t.Fatal("found no uncapped use at all; the allowlisted ones exist, so the resolver is broken, not the tree clean")
	}
	var keys []string
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, fn := range keys {
		entry, ok := uncappedAllowlist[fn]
		if !ok {
			t.Errorf("%s uses an uncapped termsafe primitive %d time(s); print through a *Max call with a constant cap in (0, %d] (internal/cli: a capped helper in termcap.go)", fn, counts[fn], maxConstantCap)
			continue
		}
		if counts[fn] != entry.uses {
			t.Errorf("%s uses an uncapped primitive %d time(s), allowlisted for %d (%s); a new use needs a capped form", fn, counts[fn], entry.uses, entry.reason)
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

// productionDirs lists every module directory holding Go files, relative to
// root in slash form, minus testdata, hidden directories (worktrees under
// .claude), and skippedPackages.
func productionDirs(root string) ([]string, error) {
	var dirs []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		name := d.Name()
		if p != root && (strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") || name == "testdata" || name == "vendor" || name == "node_modules") {
			return filepath.SkipDir
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if _, skip := skippedPackages[rel]; skip {
			return nil
		}
		dirs = append(dirs, rel)
		return nil
	})
	return dirs, err
}

func importsTermsafe(imports []string) bool {
	for _, imp := range imports {
		if imp == termsafeImportPath {
			return true
		}
	}
	return false
}

// uncappedUse is one use of an uncapped primitive: its position and the
// "pkgdir/file.go:Func" of its enclosing declaration.
type uncappedUse struct {
	pos string
	fn  string
}

// uncappedUses type-checks files in dir as the package at rel and returns
// every uncapped use of a termsafe primitive. Only termsafe is imported for
// real; every other import is an empty stand-in, and the type errors that
// follow are ignored: an identifier's use resolves to termsafe's objects
// wherever it names them, and a cap built from a termsafe or package constant
// still evaluates, which is all this needs. It keeps the pin from
// type-checking the whole dependency graph.
func uncappedUses(fset *token.FileSet, termsafePkg types.ImporterFrom, dir, rel string, names []string) ([]uncappedUse, error) {
	files := make([]*ast.File, 0, len(names))
	for _, name := range names {
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, err
		}
		files = append(files, f)
	}
	return uncappedUsesIn(fset, termsafePkg, path.Join(modulePath, rel), rel, files)
}

// newTermsafeImporter imports from source, for termsafe alone; fset must be
// the one the checked files are parsed into.
func newTermsafeImporter(fset *token.FileSet) types.ImporterFrom {
	return importer.ForCompiler(fset, "source", nil).(types.ImporterFrom)
}

func uncappedUsesIn(fset *token.FileSet, termsafePkg types.ImporterFrom, pkgPath, rel string, files []*ast.File) ([]uncappedUse, error) {
	imp := &pinImporter{real: termsafePkg, fake: map[string]*types.Package{}}
	info := &types.Info{Uses: map[*ast.Ident]types.Object{}, Types: map[ast.Expr]types.TypeAndValue{}}
	conf := types.Config{Importer: imp, Error: func(error) {}}
	_, _ = conf.Check(pkgPath, fset, files, info)
	if imp.termsafe == nil {
		return nil, fmt.Errorf("%s did not import %s; the pin resolved nothing", pkgPath, termsafeImportPath)
	}

	var uses []uncappedUse
	for _, file := range files {
		fileName := path.Join(rel, filepath.Base(fset.Position(file.Pos()).Filename))
		for _, decl := range file.Decls {
			fn := fileName + ":" + declName(decl)
			// A *Max function named in call position with an accepted
			// constant cap is capped; any other use of one (a zero, variable
			// or unbounded cap, a function value) is not.
			cappedCall := map[*ast.Ident]bool{}
			ast.Inspect(decl, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || len(call.Args) == 0 {
					return true
				}
				id := calleeIdent(call.Fun)
				if id == nil {
					return true
				}
				if acceptedCap(info, info.Uses[id], call.Args[len(call.Args)-1]) {
					cappedCall[id] = true
				}
				return true
			})
			// termsafe.Error handed straight to a fmt print call (not Errorf,
			// which wraps it for a sink further on): its text is escaped with
			// its paths capped, but the whole text is not.
			ast.Inspect(decl, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || !isFmtPrint(info, call.Fun) {
					return true
				}
				for _, arg := range call.Args {
					if id := termsafeErrorIn(info, arg); id != nil {
						uses = append(uses, uncappedUse{pos: fset.Position(id.Pos()).String(), fn: fn})
					}
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

// isFmtPrint reports whether fun names a fmt function that formats a value
// into text (Print*, Fprint*, Sprint*, Append*), as opposed to Errorf, whose
// %w keeps the error for a sink further on.
func isFmtPrint(info *types.Info, fun ast.Expr) bool {
	sel, ok := fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok {
		return false
	}
	name, ok := info.Uses[pkg].(*types.PkgName)
	if !ok || name.Imported().Path() != "fmt" {
		return false
	}
	return sel.Sel.Name != "Errorf"
}

// termsafeErrorIn returns the termsafe.Error identifier when arg is
// termsafe.Error(x) or termsafe.Error(x).Error(), and nil otherwise.
func termsafeErrorIn(info *types.Info, arg ast.Expr) *ast.Ident {
	call, ok := ast.Unparen(arg).(*ast.CallExpr)
	if !ok {
		return nil
	}
	if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Error" && len(call.Args) == 0 {
		if inner, ok := ast.Unparen(sel.X).(*ast.CallExpr); ok {
			call = inner
		}
	}
	id := calleeIdent(call.Fun)
	if id == nil {
		return nil
	}
	f, ok := info.Uses[id].(*types.Func)
	if !ok || f.Pkg() == nil || f.Pkg().Path() != termsafeImportPath || f.Name() != "Error" {
		return nil
	}
	return id
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

// defaultCapped are the *Max functions that read a cap below 1 as their
// default cap (ArgEchoMaxRunes, PathEchoMaxRunes) rather than as no cap.
var defaultCapped = map[string]bool{
	"QuoteArgMax":  true,
	"QuotePathMax": true,
	"SafePathMax":  true,
}

// acceptedCap reports whether e, the last argument of a call to callee, is a
// cap the pin accepts: an integer constant in (0, maxConstantCap], or, for a
// defaultCapped function, any integer constant up to maxConstantCap.
func acceptedCap(info *types.Info, callee types.Object, e ast.Expr) bool {
	tv, ok := info.Types[e]
	if !ok || tv.Value == nil || tv.Value.Kind() != constant.Int {
		return false
	}
	n, exact := constant.Int64Val(tv.Value)
	if !exact || n > maxConstantCap {
		return false
	}
	if n > 0 {
		return true
	}
	return callee != nil && defaultCapped[callee.Name()]
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
// function value and the package-level var are missed; make acceptedCap
// always true, and the zero, variable and unbounded caps pass; drop its
// maxConstantCap bound, and hugeCap and hugeArgCap pass; drop defaultCapped,
// and argDefault and pathDefault are flagged; classify QuotePath as
// uncapped, and pathQuoted is flagged; make isFmtPrint always false, and
// errPrinted and errTextPrinted pass; let it accept Errorf, and errWrapped is
// flagged.
func TestUncappedUsesResolvesAliasesAndMethodValues(t *testing.T) {
	const src = `package synth

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

func cappedAtBound(s string) { fmt.Println(ts.SafeLineMax(s, 4096)) }

func shadowed(s string) {
	SafeLine := func(v string) string { return v }
	fmt.Println(SafeLine(s))
}

func quoted(s string) { fmt.Println(ts.QuoteText(s)) }

func pathQuoted(s string) { fmt.Println(ts.QuotePath(s)) }

func quotedCapped(s string) { fmt.Println(ts.QuoteTextMax(s, 10)) }

func argDefault(s string) { fmt.Println(ts.QuoteArgMax(s, 0)) }

func pathDefault(s string) { fmt.Println(ts.QuotePathMax(s, 0), ts.SafePathMax(s, 0)) }

const zero = 0

func zeroCap(s string) { fmt.Println(ts.SafeLineMax(s, zero)) }

func zeroTextCap(s string) { fmt.Println(ts.QuoteTextMax(s, 0)) }

func variableCap(s string, n int) { fmt.Println(ts.SafeLineMax(s, n)) }

func hugeCap(s string) { fmt.Println(ts.SafeLineMax(s, 1<<62)) }

func hugeArgCap(s string) { fmt.Println(ts.QuoteArgMax(s, 4097)) }

func maxAsValue(xs []string) { applyMax(xs, ts.SafeLineMax) }

func errPrinted(err error) { fmt.Println(ts.Error(err)) }

func errTextPrinted(err error) string { return fmt.Sprintf("%s", ts.Error(err).Error()) }

func errWrapped(err error) error { return fmt.Errorf("x: %w", ts.Error(err)) }

func errCapped(err error) { fmt.Println(ts.SafeLineMax(ts.Error(err).Error(), 10)) }

func applyMax(xs []string, f func(string, int) string) {}

func apply(xs []string, f func(string) string) {}
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "synthetic.go", src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	uses, err := uncappedUsesIn(fset, newTermsafeImporter(fset), modulePath+"/internal/synth", "internal/synth", []*ast.File{f})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, u := range uses {
		got = append(got, strings.TrimPrefix(u.fn, "internal/synth/"))
	}
	sort.Strings(got)
	want := []string{
		"synthetic.go:asValue",
		"synthetic.go:direct",
		"synthetic.go:errPrinted",
		"synthetic.go:errTextPrinted",
		"synthetic.go:hugeArgCap",
		"synthetic.go:hugeCap",
		"synthetic.go:maxAsValue",
		"synthetic.go:quoted",
		"synthetic.go:row.method",
		"synthetic.go:var",
		"synthetic.go:variableCap",
		"synthetic.go:viaAlias",
		"synthetic.go:zeroCap",
		"synthetic.go:zeroTextCap",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("uncapped uses = %q, want %q", got, want)
	}
}
