package audit

import (
	"errors"
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/quarantine"
)

// mkfile writes an empty file at root/rel, creating parents.
func mkfile(t *testing.T, root, rel string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

// mkrepo makes root/rel a git working tree (a .git directory is all the
// scan looks for).
func mkrepo(t *testing.T, root, rel string) string {
	t.Helper()
	dir := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o750); err != nil {
		t.Fatal(err)
	}
	return dir
}

// concretize turns one quarantine target into a path that exists on disk
// under repo, returning the path relative to repo. It fails the test for an
// entry shape it cannot build, so a new kind of entry forces this pin to be
// taught about it rather than silently skipping it.
func concretize(t *testing.T, repo, target string) string {
	t.Helper()
	if strings.ContainsAny(target, "[\\") {
		t.Fatalf("quarantine target %q has a shape this pin cannot build; teach concretize about it", target)
	}
	rel := strings.NewReplacer("*", "x", "?", "x").Replace(target)
	if strings.HasSuffix(rel, "/") {
		rel = strings.TrimSuffix(rel, "/")
		mkfile(t, repo, rel+"/inner.md")
		return rel
	}
	mkfile(t, repo, rel)
	return rel
}

func scan(t *testing.T, opts Options) Report {
	t.Helper()
	r, err := ScanInjection(opts)
	if err != nil {
		t.Fatalf("ScanInjection: %v", err)
	}
	return r
}

func byPath(r Report) map[string]Finding {
	m := make(map[string]Finding, len(r.Findings))
	for _, f := range r.Findings {
		m[f.Path] = f
	}
	return m
}

// TestScanInjection_MatchesQuarantineList is the drift pin between the
// inventory and the quarantine list (forgectl#14: "keep their file-class
// lists in sync"). It builds one concrete carrier per DefaultTargets entry
// plus a nested AGENTS.md, then requires:
//
//  1. every DefaultTargets entry is reported as some finding's Target, and
//  2. the repo-root carrier set equals what quarantine.ExpandTargets resolves
//     on the same tree, so the two cannot disagree about what a carrier is.
//
// It goes red if the inventory keeps its own list, drops a target shape, or
// quarantine's predicates change without the matcher following.
func TestScanInjection_MatchesQuarantineList(t *testing.T) {
	root := t.TempDir()
	repo := mkrepo(t, root, "github.com/o/r")
	for _, target := range quarantine.DefaultTargets {
		concretize(t, repo, target)
	}
	mkfile(t, repo, "src/pkg/AGENTS.md")
	mkfile(t, repo, "README.md")
	mkfile(t, repo, ".windsurfrules") // not a quarantine class, so not a carrier

	r := scan(t, Options{Root: root})

	gotTargets := map[string]bool{}
	var gotRel []string
	for _, f := range r.Findings {
		gotTargets[f.Target] = true
		rel, err := filepath.Rel(repo, f.Path)
		if err != nil {
			t.Fatal(err)
		}
		gotRel = append(gotRel, filepath.ToSlash(rel))
	}
	for _, target := range quarantine.DefaultTargets {
		if !gotTargets[target] {
			t.Errorf("quarantine target %q has a carrier on disk but the inventory reported none", target)
		}
	}

	expanded, err := quarantine.ExpandTargets(repo, quarantine.PrefixUnderscore, quarantine.DefaultTargets)
	if err != nil {
		t.Fatalf("ExpandTargets: %v", err)
	}
	var wantRel []string
	for _, e := range expanded {
		if _, err := os.Lstat(filepath.Join(repo, e)); err == nil {
			wantRel = append(wantRel, filepath.ToSlash(e))
		}
	}
	sort.Strings(gotRel)
	sort.Strings(wantRel)
	if strings.Join(gotRel, "\n") != strings.Join(wantRel, "\n") {
		t.Errorf("inventory and quarantine disagree on the carrier set\n inventory:  %v\n quarantine: %v", gotRel, wantRel)
	}
}

// TestScanInjection_FoldsCaseLikeQuarantine pins ASCII case folding: on the
// case-insensitive filesystems forgectl runs on, `Claude.Md` is read as
// CLAUDE.md, and quarantine matches it (the fixture uses mixed case so an unfolded compare against the folded list misses it); so must the inventory.
func TestScanInjection_FoldsCaseLikeQuarantine(t *testing.T) {
	root := t.TempDir()
	repo := mkrepo(t, root, "r")
	mkfile(t, repo, "sub/Claude.Md")
	mkfile(t, repo, ".gemini/MCP.json")

	got := byPath(scan(t, Options{Root: root}))
	if f, ok := got[filepath.Join(repo, "sub", "Claude.Md")]; !ok || f.Target != "CLAUDE.md" {
		t.Errorf("sub/Claude.Md: got %+v, want target CLAUDE.md", f)
	}
	if f, ok := got[filepath.Join(repo, ".gemini", "MCP.json")]; !ok || f.Target != ".*/mcp.json" {
		t.Errorf(".gemini/MCP.json: got %+v, want target .*/mcp.json", f)
	}
}

// TestScanInjection_NeverFollowsSymlinks pins confinement. A symlinked
// carrier is reported as a symlink, a symlinked directory is never walked
// (in-root or escaping), and nothing outside the root is ever reported.
func TestScanInjection_NeverFollowsSymlinks(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "projects")
	outside := filepath.Join(base, "outside")
	mkfile(t, outside, "AGENTS.md")
	mkfile(t, outside, "secret-dir/CLAUDE.md")
	repo := mkrepo(t, root, "r")
	mkfile(t, repo, "sub/AGENTS.md")

	if err := os.Symlink(filepath.Join(outside, "AGENTS.md"), filepath.Join(repo, "CLAUDE.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret-dir"), filepath.Join(repo, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(repo, "sub"), filepath.Join(repo, "alias")); err != nil {
		t.Fatal(err)
	}

	r := scan(t, Options{Root: root})
	got := byPath(r)
	for p := range got {
		if !strings.HasPrefix(p, root+string(filepath.Separator)) {
			t.Errorf("finding %q is outside the scan root %q", p, root)
		}
	}
	if f, ok := got[filepath.Join(repo, "CLAUDE.md")]; !ok || f.Type != TypeSymlink {
		t.Errorf("symlinked CLAUDE.md: got %+v, want a %q finding", f, TypeSymlink)
	}
	if _, ok := got[filepath.Join(repo, "alias", "AGENTS.md")]; ok {
		t.Error("walked an in-root symlinked directory (alias/AGENTS.md reported)")
	}
	if _, ok := got[filepath.Join(repo, "escape", "CLAUDE.md")]; ok {
		t.Error("walked a symlinked directory that escapes the root")
	}
	if _, ok := got[filepath.Join(repo, "sub", "AGENTS.md")]; !ok {
		t.Error("sub/AGENTS.md missing: the walk is vacuous")
	}
	if r.Unreadable != 0 {
		t.Errorf("Unreadable = %d, want 0: a symlink was treated as a directory to list", r.Unreadable)
	}
	if len(r.Findings) != 2 {
		t.Errorf("findings = %d, want 2 (CLAUDE.md symlink, sub/AGENTS.md): %+v", len(r.Findings), r.Findings)
	}
}

// TestScanInjection_SymlinkedRootKeepsCallerSpelling: a projects root that
// is itself a symlink is walked (os.OpenRoot follows the root path), and the
// report keeps the caller's spelling of it.
func TestScanInjection_SymlinkedRootKeepsCallerSpelling(t *testing.T) {
	base := t.TempDir()
	realDir := filepath.Join(base, "real")
	repo := mkrepo(t, realDir, "r")
	mkfile(t, repo, "AGENTS.md")
	link := filepath.Join(base, "link")
	if err := os.Symlink(realDir, link); err != nil {
		t.Fatal(err)
	}
	r := scan(t, Options{Root: link})
	if r.Root != link || len(r.Findings) != 1 || r.Findings[0].Path != filepath.Join(link, "r", "AGENTS.md") {
		t.Fatalf("root=%q findings=%v, want root %q and one finding under it", r.Root, r.Findings, link)
	}
}

func TestScanInjection_Anomalies(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	old := now.Add(-30 * 24 * time.Hour)
	repo := mkrepo(t, root, "r")
	paths := map[string][]string{
		"CLAUDE.md":                     {},
		"docs/AGENTS.md":                {},
		"sub/.claude/settings.json":     nil, // the .claude dir is the carrier
		"node_modules/pkg/CLAUDE.md":    {AnomalyVendored},
		"vendor/x/.github/instructions": nil,
		"fresh/AGENTS.md":               {AnomalyRecent},
	}
	for rel := range paths {
		mkfile(t, repo, rel)
	}
	mkfile(t, root, "loose/.mcp.json") // outside any repo
	mkfile(t, root, "loose/AGENTS.md")
	// Age everything, then freshen the one recent carrier.
	var all []string
	_ = filepath.Walk(root, func(p string, _ os.FileInfo, err error) error {
		if err == nil {
			all = append(all, p)
		}
		return nil
	})
	for _, p := range all {
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chtimes(filepath.Join(repo, "fresh", "AGENTS.md"), now.Add(-time.Hour), now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}

	got := byPath(scan(t, Options{Root: root, Now: now}))
	want := map[string][]string{
		filepath.Join(repo, "CLAUDE.md"):                              {},
		filepath.Join(repo, "docs", "AGENTS.md"):                      {},
		filepath.Join(repo, "sub", ".claude"):                         {AnomalyOffRoot},
		filepath.Join(repo, "node_modules", "pkg", "CLAUDE.md"):       {AnomalyVendored},
		filepath.Join(repo, "vendor", "x", ".github", "instructions"): {AnomalyVendored, AnomalyOffRoot},
		filepath.Join(repo, "fresh", "AGENTS.md"):                     {AnomalyRecent},
		filepath.Join(root, "loose", ".mcp.json"):                     {AnomalyOffRoot},
		filepath.Join(root, "loose", "AGENTS.md"):                     {},
	}
	if len(got) != len(want) {
		t.Errorf("findings = %d, want %d: %+v", len(got), len(want), got)
	}
	for p, anomalies := range want {
		f, ok := got[p]
		if !ok {
			t.Errorf("missing finding %q", p)
			continue
		}
		if strings.Join(f.Anomalies, ",") != strings.Join(anomalies, ",") {
			t.Errorf("%q anomalies = %v, want %v", p, f.Anomalies, anomalies)
		}
	}
	if f := got[filepath.Join(repo, "CLAUDE.md")]; f.Repo != repo {
		t.Errorf("repo attribution = %q, want %q", f.Repo, repo)
	}
	if f := got[filepath.Join(root, "loose", "AGENTS.md")]; f.Repo != "" {
		t.Errorf("a carrier outside any repo has Repo %q, want empty", f.Repo)
	}
}

func TestScanInjection_Caps(t *testing.T) {
	root := t.TempDir()
	repo := mkrepo(t, root, "r")
	mkfile(t, repo, "a/AGENTS.md")
	mkfile(t, repo, "b/AGENTS.md")
	mkfile(t, repo, "c/d/e/f/AGENTS.md")

	if r := scan(t, Options{Root: root}); r.Truncated || len(r.Findings) != 3 {
		t.Fatalf("uncapped: truncated=%v findings=%d, want false/3", r.Truncated, len(r.Findings))
	}
	if r := scan(t, Options{Root: root, MaxFindings: 1}); !r.Truncated || len(r.Findings) != 1 || !r.Stopped() || strings.Join(r.CappedBy, ",") != CapFindings {
		t.Errorf("MaxFindings=1: truncated=%v findings=%d stopped=%v cappedBy=%v, want true/1/true/[findings]", r.Truncated, len(r.Findings), r.Stopped(), r.CappedBy)
	}
	if r := scan(t, Options{Root: root, MaxEntries: 3}); !r.Truncated || r.Entries > 4 || !r.Stopped() || strings.Join(r.CappedBy, ",") != CapEntries {
		t.Errorf("MaxEntries=3: truncated=%v entries=%d cappedBy=%v, want true, at most one over, [entries]", r.Truncated, r.Entries, r.CappedBy)
	}
	r := scan(t, Options{Root: root, MaxDepth: 4})
	if !r.Truncated || r.Stopped() || strings.Join(r.CappedBy, ",") != CapDepth || r.DepthSkipped != 1 {
		t.Errorf("MaxDepth=4: truncated=%v stopped=%v cappedBy=%v depthSkipped=%d, want true/false/[depth]/1", r.Truncated, r.Stopped(), r.CappedBy, r.DepthSkipped)
	}
	if _, ok := byPath(r)[filepath.Join(repo, "c", "d", "e", "f", "AGENTS.md")]; ok {
		t.Error("MaxDepth=4 still reported a carrier six levels down")
	}
}

func TestScanInjection_MissingRootErrors(t *testing.T) {
	if _, err := ScanInjection(Options{Root: filepath.Join(t.TempDir(), "nope")}); err == nil {
		t.Fatal("want an error for a missing root")
	}
}

// TestScanInjection_DepthCapBoundary pins the off-by-one: with MaxDepth N the
// walk lists directories at depths 0..N-1 (the root is 0), so a carrier inside
// a depth-(N-1) directory is found and one a level lower is not.
func TestScanInjection_DepthCapBoundary(t *testing.T) {
	root := t.TempDir()
	repo := mkrepo(t, root, "r")       // depth 1
	mkfile(t, repo, "c/d/AGENTS.md")   // inside depth 3
	mkfile(t, repo, "c/d/e/AGENTS.md") // inside depth 4
	got := byPath(scan(t, Options{Root: root, MaxDepth: 4}))
	if _, ok := got[filepath.Join(repo, "c", "d", "AGENTS.md")]; !ok {
		t.Error("MaxDepth=4 missed a carrier inside a depth-3 directory")
	}
	if _, ok := got[filepath.Join(repo, "c", "d", "e", "AGENTS.md")]; ok {
		t.Error("MaxDepth=4 walked a depth-4 directory")
	}
}

// TestScanInjection_SkipsGitAndSorts pins that .git is never walked or
// classified, and that findings come back in path order, which differs from
// walk order ("a-c/..." sorts before "a/..." but is walked after it).
func TestScanInjection_SkipsGitAndSorts(t *testing.T) {
	root := t.TempDir()
	repo := mkrepo(t, root, "r")
	mkfile(t, repo, ".git/CLAUDE.md")
	mkfile(t, repo, ".git/hooks/AGENTS.md")
	mkfile(t, repo, "a/CLAUDE.md")
	mkfile(t, repo, "a-c/CLAUDE.md")
	r := scan(t, Options{Root: root})
	var paths []string
	for _, f := range r.Findings {
		if strings.Contains(f.Path, string(filepath.Separator)+".git"+string(filepath.Separator)) {
			t.Errorf("reported a carrier inside .git: %s", f.Path)
		}
		paths = append(paths, f.Path)
	}
	if len(paths) != 2 || !sort.StringsAreSorted(paths) {
		t.Errorf("findings = %v, want the two non-.git carriers in path order", paths)
	}
}

// fixedInfo is an fs.FileInfo double for the metadata seam tests.
type fixedInfo struct {
	name string
	mode fs.FileMode
}

func (i fixedInfo) Name() string       { return i.name }
func (i fixedInfo) Size() int64        { return 0 }
func (i fixedInfo) Mode() fs.FileMode  { return i.mode }
func (i fixedInfo) ModTime() time.Time { return time.Time{} }
func (i fixedInfo) IsDir() bool        { return i.mode.IsDir() }
func (i fixedInfo) Sys() any           { return nil }

// TestScanInjection_MetadataOnlyThroughRoot pins Important-1 of the #901
// review: every entry's type comes from the fsOps lstat (bound to
// os.Root.Lstat in production), never from a DirEntry or an absolute-path
// stat. With a lstat that fails for everything, a tree full of carriers must
// yield no findings; with a counting lstat, every non-.git entry is stat'd
// exactly once through it.
func TestScanInjection_MetadataOnlyThroughRoot(t *testing.T) {
	root := t.TempDir()
	repo := mkrepo(t, root, "r")
	mkfile(t, repo, "CLAUDE.md")
	mkfile(t, repo, "sub/AGENTS.md")
	r, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()

	ops := rootOps(r)
	ops.lstat = func(string) (fs.FileInfo, error) { return nil, errors.New("denied") }
	rep, err := scanWith(root, ops, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Findings) != 0 || rep.Entries == 0 {
		t.Errorf("with every lstat failing: findings=%d entries=%d, want 0 and >0 (metadata reached the scan another way)", len(rep.Findings), rep.Entries)
	}
	// Only the root is listed (r cannot be stat'd, so it is never walked):
	// entries are just "r", and its failed lstat is counted.
	if rep.Unreadable != rep.Entries {
		t.Errorf("with every lstat failing: unreadable=%d entries=%d, want every failed lstat counted", rep.Unreadable, rep.Entries)
	}

	ops = rootOps(r)
	calls := 0
	inner := ops.lstat
	ops.lstat = func(name string) (fs.FileInfo, error) { calls++; return inner(name) }
	rep, err = scanWith(root, ops, Options{})
	if err != nil {
		t.Fatal(err)
	}
	// Entries: r, r/.git, r/CLAUDE.md, r/sub, r/sub/AGENTS.md; .git is skipped before the stat.
	if len(rep.Findings) != 2 || calls != rep.Entries-1 {
		t.Errorf("counting lstat: findings=%d calls=%d entries=%d, want 2 and calls == entries-1", len(rep.Findings), calls, rep.Entries)
	}

	// A seam that reports a directory named like a carrier as a plain dir must
	// be believed over the disk: the type is the seam's.
	ops = rootOps(r)
	ops.lstat = func(name string) (fs.FileInfo, error) { return fixedInfo{name: path.Base(name), mode: fs.ModeDir}, nil }
	rep, err = scanWith(root, ops, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range rep.Findings {
		if f.Type != TypeDir {
			t.Errorf("%s: type %q did not come from the lstat seam", f.Path, f.Type)
		}
	}
}

// TestScanInjection_UnreadableCounted: a directory that cannot be listed is
// counted and skipped; the rest of the tree is still scanned.
func TestScanInjection_UnreadableCounted(t *testing.T) {
	root := t.TempDir()
	repo := mkrepo(t, root, "r")
	mkfile(t, repo, "locked/AGENTS.md")
	mkfile(t, repo, "open/AGENTS.md")
	r, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	ops := rootOps(r)
	names := ops.names
	ops.names = func(dir string) ([]string, error) {
		if dir == "r/locked" {
			return nil, fs.ErrPermission
		}
		return names(dir)
	}
	rep, err := scanWith(root, ops, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Unreadable != 1 || len(rep.Findings) != 1 {
		t.Errorf("unreadable=%d findings=%d, want 1/1", rep.Unreadable, len(rep.Findings))
	}
}

// modulePath prefixes this module's own import paths.
const modulePath = "github.com/cameronsjo/forgectl/"

// auditImports is the import-set allowlist for the package's shipped files:
// exactly what they import today. A filesystem path that needs no call the
// use check below names (io/ioutil, os/exec, net, plugin, another internal
// package) is refused by its import, so the pin covers indirect reach and not
// only direct calls. Widening this list is a deliberate edit, audited here.
var auditImports = map[string]bool{
	"errors":                           true,
	"fmt":                              true,
	"io/fs":                            true,
	"path":                             true,
	"path/filepath":                    true,
	"sort":                             true,
	"time":                             true,
	modulePath + "internal/quarantine": true,
	modulePath + "internal/termsafe":   true,
}

// rootopsImports are the imports only the rootops files may add.
var rootopsImports = map[string]bool{"os": true, "syscall": true}

// internalAllowed names every function and package-level variable the
// package may use from another internal package. Each one does no
// filesystem I/O; quarantine.ExpandTargets, which lists directories, is the
// kind of helper this refuses. Types, constants and struct fields are data
// and need no entry; neither allowed package exports a func-typed field, which
// would be the one field a call could go through.
var internalAllowed = map[string]bool{
	"internal/quarantine.DefaultTargets":             true,
	"internal/quarantine.NewCarrierMatcher":          true,
	"internal/quarantine.CarrierMatcher.Match":       true,
	"internal/quarantine.CarrierMatcher.MatchPrefix": true,
	"internal/quarantine.CarrierMatcher.MaxSegments": true,
	"internal/termsafe.QuotePath":                    true,
	"internal/termsafe.Error":                        true,
}

// confinementUses is what unconfinedUses reports: every violation, plus how
// many allowlisted os uses and internal-package function uses it resolved,
// so a caller can prove neither half of the check was vacuous (an importer
// failure would resolve none).
type confinementUses struct {
	bad          []string
	allowedOS    int
	internalUses int
}

// unconfinedUses type-checks files and returns every use of a filesystem
// function that could resolve outside the os.Root, outside the allowlisted
// rootops files. It resolves identifiers through go/types (Info.Uses), so an
// aliased import, a method value, or a syscall spelling is caught, not only
// the naive `os.Lstat(` text:
//   - any package-level function of os, syscall or io/fs;
//   - path/filepath's EvalSymlinks, Glob, Walk and WalkDir;
//   - any method of an os type (*os.File, *os.Root);
//   - the io/fs interface methods that stat, list or open (DirEntry.Info,
//     DirEntry.Type, FS.Open, ReadDir, ReadFile, Stat, Glob, Sub).
//
// Indirect reach is refused too, in every file including rootops:
//   - an import outside auditImports (plus rootopsImports in the rootops
//     files), so io/ioutil and os/exec never enter the package;
//   - a function, method or package-level variable of another internal
//     package that is not in internalAllowed.
func unconfinedUses(t *testing.T, fset *token.FileSet, files []*ast.File) confinementUses {
	t.Helper()
	var out confinementUses
	for _, file := range files {
		base := filepath.Base(fset.Position(file.Pos()).Filename)
		for _, imp := range file.Imports {
			p, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Fatalf("%s: import %s: %v", base, imp.Path.Value, err)
			}
			if auditImports[p] || (isRootops(base) && rootopsImports[p]) {
				continue
			}
			out.bad = append(out.bad, fmt.Sprintf("%s: import %q is not in the audit import allowlist", fset.Position(imp.Pos()), p))
		}
	}
	info := &types.Info{Uses: map[*ast.Ident]types.Object{}}
	conf := types.Config{Importer: importer.ForCompiler(fset, "gc", exportData), Error: func(error) {}}
	_, _ = conf.Check("audit", fset, files, info) // a refused import may not resolve; every other use still does
	fsMethods := map[string]bool{"Info": true, "Type": true, "Open": true, "ReadDir": true, "ReadFile": true, "Stat": true, "Glob": true, "Sub": true}
	filepathFuncs := map[string]bool{"EvalSymlinks": true, "Glob": true, "Walk": true, "WalkDir": true}
	for id, obj := range info.Uses {
		if obj.Pkg() == nil {
			continue
		}
		pkg := obj.Pkg().Path()
		pos := fset.Position(id.Pos())
		if strings.HasPrefix(pkg, modulePath) {
			if key, gated := internalKey(obj); gated {
				out.internalUses++
				if !internalAllowed[key] {
					out.bad = append(out.bad, fmt.Sprintf("%s: %s is not in the internal-helper allowlist", pos, key))
				}
			}
			continue
		}
		fn, ok := obj.(*types.Func)
		if !ok {
			continue
		}
		sig, _ := fn.Type().(*types.Signature)
		method := sig != nil && sig.Recv() != nil
		var banned bool
		switch {
		case !method && (pkg == "os" || pkg == "syscall" || pkg == "io/fs"):
			banned = true
		case !method && pkg == "path/filepath":
			banned = filepathFuncs[fn.Name()]
		case method && pkg == "os":
			banned = true
		case method && pkg == "io/fs":
			banned = fsMethods[fn.Name()]
		}
		if !banned {
			continue
		}
		if isRootops(filepath.Base(pos.Filename)) {
			if pkg == "os" {
				out.allowedOS++
			}
			continue
		}
		out.bad = append(out.bad, fmt.Sprintf("%s: %s.%s", pos, pkg, fn.Name()))
	}
	sort.Strings(out.bad)
	return out
}

// exportData opens the compiler export data `go list -export` reports for an
// import path, this module's internal packages included. importer.Default
// finds only the standard library's, and without an internal package's types
// every internal use would resolve to nothing and pass the allowlist unseen
// (the internalUses count catches that).
func exportData(importPath string) (io.ReadCloser, error) {
	exportFilesMu.Lock()
	file, ok := exportFiles[importPath]
	exportFilesMu.Unlock()
	if !ok {
		out, err := exec.Command("go", "list", "-export", "-f", "{{.Export}}", "--", importPath).Output() //nolint:gosec,noctx // G204: the go tool resolving an import path the type checker asked for
		if err != nil {
			return nil, fmt.Errorf("go list -export %s: %w", importPath, err)
		}
		file = strings.TrimSpace(string(out))
		exportFilesMu.Lock()
		exportFiles[importPath] = file
		exportFilesMu.Unlock()
	}
	if file == "" {
		return nil, fmt.Errorf("go list -export %s: no export data", importPath)
	}
	return os.Open(filepath.Clean(file))
}

var (
	exportFilesMu sync.Mutex
	exportFiles   = map[string]string{}
)

func isRootops(base string) bool {
	return strings.HasPrefix(base, "rootops") && !strings.HasSuffix(base, "_test.go")
}

// internalKey names an internal package's function, method or package-level
// variable as internalAllowed spells it ("internal/pkg.Func",
// "internal/pkg.Type.Method"). gated is false for the objects that are data
// (types, constants, struct fields), which need no entry.
func internalKey(obj types.Object) (key string, gated bool) {
	prefix := strings.TrimPrefix(obj.Pkg().Path(), modulePath) + "."
	switch o := obj.(type) {
	case *types.Func:
		if sig, _ := o.Type().(*types.Signature); sig != nil && sig.Recv() != nil {
			recv := sig.Recv().Type()
			if ptr, ok := recv.(*types.Pointer); ok {
				recv = ptr.Elem()
			}
			if named, ok := recv.(*types.Named); ok {
				return prefix + named.Obj().Name() + "." + o.Name(), true
			}
			return prefix + "?." + o.Name(), true
		}
		return prefix + o.Name(), true
	case *types.Var:
		if o.IsField() {
			return "", false
		}
		return prefix + o.Name(), true
	}
	return "", false
}

// TestAuditSource_NoUnconfinedFilesystemCalls is the static half of the
// confinement pin: outside rootops*.go, the package's shipped source makes no
// filesystem call that could resolve outside the os.Root, and no file reaches
// the filesystem indirectly through an unlisted import or internal helper.
func TestAuditSource_NoUnconfinedFilesystemCalls(t *testing.T) {
	fset := token.NewFileSet()
	dirents, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var files []*ast.File
	for _, d := range dirents {
		if !strings.HasSuffix(d.Name(), ".go") || strings.HasSuffix(d.Name(), "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Clean(d.Name()), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		// Build constraints are not evaluated here, so only this platform's
		// dirOpenFlags file may join the check or the two would collide.
		if (strings.HasSuffix(d.Name(), "_unix.go") || strings.HasSuffix(d.Name(), "_other.go")) && d.Name() != dirOpenFlagsFile() {
			continue
		}
		files = append(files, file)
	}
	uses := unconfinedUses(t, fset, files)
	for _, b := range uses.bad {
		t.Errorf("unconfined filesystem reach: %s", b)
	}
	if uses.allowedOS == 0 {
		t.Fatal("resolved no os use in rootops.go; the type check is vacuous")
	}
	if uses.internalUses == 0 {
		t.Fatal("resolved no internal-package function use; the internal-helper check is vacuous")
	}
}

func dirOpenFlagsFile() string {
	if runtime.GOOS == "windows" || runtime.GOOS == "plan9" || runtime.GOOS == "js" || runtime.GOOS == "wasip1" {
		return "rootops_other.go"
	}
	return "rootops_unix.go"
}

// TestAuditSource_ProbesGoRed proves the static pin sees through each evasion
// the naive text check missed. Every probe must be refused. A direct call
// probe must not be refused in an allowlisted rootops file; an indirect-reach
// probe (a banned import, an unlisted internal helper) is refused there too.
//
// Mutation that turns it red: add "io/ioutil" or "os/exec" to auditImports,
// or "internal/quarantine.ExpandTargets" to internalAllowed.
func TestAuditSource_ProbesGoRed(t *testing.T) {
	type probe struct {
		src      string
		indirect bool // refused in rootops.go as well
	}
	probes := map[string]probe{
		"aliased import": {src: `package audit
import xos "os"
func f() { _, _ = xos.Lstat("x") }`},
		"method value": {src: `package audit
import "io/fs"
func f(e fs.DirEntry) { g := e.Info; _ = g }`},
		"EvalSymlinks": {src: `package audit
import "path/filepath"
func f() { _, _ = filepath.EvalSymlinks("x") }`},
		"os.Root method outside rootops": {src: `package audit
import "os"
func f(r *os.Root) { _, _ = r.Lstat("x") }`},
		"io/ioutil": {indirect: true, src: `package audit
import "io/ioutil"
func f() { _, _ = ioutil.ReadDir("x") }`},
		"os/exec": {indirect: true, src: `package audit
import "os/exec"
func f() { _ = exec.Command("stat", "x").Run() }`},
		"blank os/exec": {indirect: true, src: `package audit
import _ "os/exec"`},
		"internal helper": {indirect: true, src: `package audit
import "github.com/cameronsjo/forgectl/internal/quarantine"
func f() { _, _ = quarantine.ExpandTargets("x", quarantine.PrefixUnderscore, nil) }`},
	}
	if runtime.GOOS != "windows" {
		probes["syscall.Lstat"] = probe{src: `package audit
import "syscall"
func f() { var st syscall.Stat_t; _ = syscall.Lstat("x", &st) }`}
	}
	check := func(filename, src string) []string {
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, filename, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", filename, err)
		}
		return unconfinedUses(t, fset, []*ast.File{file}).bad
	}
	for name, p := range probes {
		if bad := check("probe.go", p.src); len(bad) == 0 {
			t.Errorf("probe %q was not refused", name)
		}
		bad := check("rootops.go", p.src)
		if p.indirect && len(bad) == 0 {
			t.Errorf("probe %q was not refused in rootops.go", name)
		}
		if !p.indirect && len(bad) != 0 {
			t.Errorf("probe %q refused even in the allowlisted rootops.go: %v", name, bad)
		}
	}
}

// symlinkCase builds repo/<link> -> dest, with file created under dest.
type symlinkCase struct {
	name, link, dest, file, target string
	outside                        bool
}

// TestScanInjection_SymlinkedCarrierDirsMatchQuarantine extends the drift pin
// to trees where a multi-segment carrier sits behind a symlinked directory
// (Important-2 of the #901 review). ExpandTargets lists such a carrier through
// an in-root link and refuses an escaping one; either way the inventory must
// report the link itself, unfollowed, with the entry it prefixes and the
// symlink anomaly. The last case is the stated limit: with no carrier behind
// the link, ExpandTargets reports nothing and the inventory still reports the
// link, because it cannot look behind it without following.
func TestScanInjection_SymlinkedCarrierDirsMatchQuarantine(t *testing.T) {
	cases := []symlinkCase{
		{name: "in-root pattern", link: ".gemini", dest: "cfg", file: "mcp.json", target: ".*/mcp.json"},
		{name: "in-root literal", link: ".github", dest: "shared", file: "instructions/x.md", target: ".github/instructions/"},
		{name: "escaping pattern", link: ".windsurf", file: "mcp.json", target: ".*/mcp.json", outside: true},
		{name: "escaping literal", link: ".github", file: "instructions/x.md", target: ".github/instructions/", outside: true},
		{name: "nothing behind (limit)", link: ".gemini", dest: "cfg", file: "other.txt", target: ".*/mcp.json"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			base := t.TempDir()
			root := filepath.Join(base, "projects")
			repo := mkrepo(t, root, "repo")
			dest := filepath.Join(repo, c.dest)
			if c.outside {
				dest = filepath.Join(base, "outside")
			}
			mkfile(t, dest, c.file)
			if err := os.Symlink(dest, filepath.Join(repo, c.link)); err != nil {
				t.Fatal(err)
			}

			expanded, expandErr := quarantine.ExpandTargets(repo, quarantine.PrefixUnderscore, quarantine.DefaultTargets)
			var listed []string
			for _, e := range expanded {
				if _, err := os.Lstat(filepath.Join(repo, e)); err == nil && strings.HasPrefix(filepath.ToSlash(e), c.link+"/") {
					listed = append(listed, e)
				}
			}

			got := byPath(scan(t, Options{Root: root}))
			link := filepath.Join(repo, c.link)
			f, ok := got[link]
			if !ok {
				t.Fatalf("no finding for the symlinked %s (quarantine listed %v, err %v)", c.link, listed, expandErr)
			}
			if f.Target != c.target || f.Type != TypeSymlink || !strings.Contains(strings.Join(f.Anomalies, ","), AnomalySymlink) {
				t.Errorf("finding = %+v, want target %q, type symlink, anomaly symlink", f, c.target)
			}
			for p := range got {
				if strings.HasPrefix(p, link+string(filepath.Separator)) {
					t.Errorf("reported %s behind the link: the link was followed", p)
				}
			}
			// Every carrier quarantine lists or refuses behind the link is
			// covered by the link finding.
			if len(listed) == 0 && expandErr == nil && c.file != "other.txt" {
				t.Errorf("quarantine neither listed nor refused a carrier behind %s; the fixture is not exercising the case", c.link)
			}
		})
	}
}

// TestScanInjection_RootUnderSymlinkedParent reproduces the darwin CI
// failure on any platform: the projects root sits under a symlinked parent
// (on macOS every t.TempDir does, since /var -> /private/var). The report
// must use one spelling of the root throughout, the one the caller gave, so
// Root, every finding's Path and Repo, and any Rel a consumer takes against
// Root agree.
func TestScanInjection_RootUnderSymlinkedParent(t *testing.T) {
	base := t.TempDir()
	realParent := filepath.Join(base, "real")
	if err := os.MkdirAll(realParent, 0o750); err != nil {
		t.Fatal(err)
	}
	linkParent := filepath.Join(base, "link")
	if err := os.Symlink(realParent, linkParent); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(linkParent, "projects")
	repo := mkrepo(t, root, "r")
	mkfile(t, repo, "CLAUDE.md")
	mkfile(t, repo, "sub/AGENTS.md")

	r := scan(t, Options{Root: root})
	if r.Root != root {
		t.Errorf("Root = %q, want the caller's spelling %q", r.Root, root)
	}
	got := byPath(r)
	for _, want := range []string{filepath.Join(repo, "CLAUDE.md"), filepath.Join(repo, "sub", "AGENTS.md")} {
		f, ok := got[want]
		if !ok {
			t.Errorf("missing %q; got %v", want, r.Findings)
			continue
		}
		if f.Repo != repo {
			t.Errorf("%q: Repo = %q, want %q", want, f.Repo, repo)
		}
		if rel, err := filepath.Rel(r.Root, f.Path); err != nil || strings.HasPrefix(rel, "..") {
			t.Errorf("%q is not under Root %q (rel %q)", f.Path, r.Root, rel)
		}
	}
}

// TestScanInjection_SymlinkPrefixNeedsADirAtTheRoot pins the MatchPrefix
// guards: a dot-named symlink is reported as a possible carrier directory only
// when it sits at a repo root and root.Stat does not show a file or a missing
// target. `.env` and `.eslintrc` (links to files), a dangling `.gemini`, and
// `pkg/.hidden` (below the root) are not carriers; `.cursorx` to a directory
// at the root is.
func TestScanInjection_SymlinkPrefixNeedsADirAtTheRoot(t *testing.T) {
	root := t.TempDir()
	repo := mkrepo(t, root, "r")
	mkfile(t, repo, "real.env")
	mkfile(t, repo, "cfgdir/keep")
	// Relative targets, as an in-repo link is written: os.Root refuses to
	// resolve an absolute link target, so such a link stays reported as
	// unverifiable, like an escaping one.
	links := map[string]string{
		".env":        "real.env",
		".eslintrc":   "real.env",
		".gemini":     "missing",
		"pkg/.hidden": "../cfgdir",
		".cursorx":    "cfgdir",
	}
	for name, target := range links {
		p := filepath.Join(repo, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, p); err != nil {
			t.Fatal(err)
		}
	}
	got := byPath(scan(t, Options{Root: root}))
	for _, name := range []string{".env", ".eslintrc", ".gemini", "pkg/.hidden"} {
		if f, ok := got[filepath.Join(repo, filepath.FromSlash(name))]; ok {
			t.Errorf("%s reported as %+v; it cannot hold a carrier", name, f)
		}
	}
	if f, ok := got[filepath.Join(repo, ".cursorx")]; !ok || f.Target != ".*/mcp.json" {
		t.Errorf(".cursorx -> directory at the repo root: got %+v, want a .*/mcp.json symlink finding", f)
	}
}

// TestScanInjection_LstatFailureCounted: an entry whose lstat fails is
// counted in Unreadable, not silently dropped.
func TestScanInjection_LstatFailureCounted(t *testing.T) {
	root := t.TempDir()
	repo := mkrepo(t, root, "r")
	mkfile(t, repo, "gone/AGENTS.md")
	mkfile(t, repo, "here/AGENTS.md")
	r, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	ops := rootOps(r)
	lstat := ops.lstat
	ops.lstat = func(name string) (fs.FileInfo, error) {
		if name == "r/gone" {
			return nil, fs.ErrNotExist
		}
		return lstat(name)
	}
	rep, err := scanWith(root, ops, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Unreadable != 1 || len(rep.Findings) != 1 {
		t.Errorf("unreadable=%d findings=%d, want 1/1", rep.Unreadable, len(rep.Findings))
	}
}
