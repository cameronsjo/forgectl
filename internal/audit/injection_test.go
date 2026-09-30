package audit

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
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

// TestScanInjection_SymlinkedRootIsResolved: a projects root reached
// through a symlink is walked, and findings carry the resolved prefix.
func TestScanInjection_SymlinkedRootIsResolved(t *testing.T) {
	base := t.TempDir()
	realDir := filepath.Join(base, "real")
	repo := mkrepo(t, realDir, "r")
	mkfile(t, repo, "AGENTS.md")
	link := filepath.Join(base, "link")
	if err := os.Symlink(realDir, link); err != nil {
		t.Fatal(err)
	}
	r := scan(t, Options{Root: link})
	resolved, err := filepath.EvalSymlinks(realDir)
	if err != nil {
		t.Fatal(err)
	}
	if r.Root != resolved || len(r.Findings) != 1 {
		t.Fatalf("root=%q findings=%d, want root %q and 1 finding", r.Root, len(r.Findings), resolved)
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

// TestAuditSource_NoUnconfinedFilesystemCalls is the static half of the
// confinement pin: the package's shipped source makes no filesystem call
// that could resolve outside the os.Root. Only os.OpenRoot (on the resolved
// root), the root's own methods, and a root-opened file's Readdirnames/Close
// are allowed; any DirEntry.Info/Type, any os/fs stat or listing, and any
// filepath walk is refused.
func TestAuditSource_NoUnconfinedFilesystemCalls(t *testing.T) {
	fset := token.NewFileSet()
	dirents, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]*ast.File{}
	for _, d := range dirents {
		if !strings.HasSuffix(d.Name(), ".go") || strings.HasSuffix(d.Name(), "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Clean(d.Name()), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		files[d.Name()] = file
	}
	banned := map[string]map[string]bool{
		"os":       {"Lstat": true, "Stat": true, "Open": true, "OpenFile": true, "ReadDir": true, "ReadFile": true, "Readlink": true, "DirFS": true},
		"fs":       {"ReadDir": true, "Stat": true, "ReadFile": true, "WalkDir": true, "Glob": true, "Sub": true, "Lstat": true},
		"filepath": {"Walk": true, "WalkDir": true, "Glob": true},
	}
	bannedMethods := map[string]bool{"Info": true, "Type": true, "ReadDir": true, "Readdir": true, "Stat": true}
	checked := 0
	for name, file := range files {
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			checked++
			if id, ok := sel.X.(*ast.Ident); ok {
				if set, ok := banned[id.Name]; ok {
					if set[sel.Sel.Name] {
						t.Errorf("%s: %s.%s escapes the os.Root", fset.Position(call.Pos()), id.Name, sel.Sel.Name)
					}
					return true
				}
			}
			if bannedMethods[sel.Sel.Name] {
				t.Errorf("%s: .%s() reads metadata outside the os.Root seam (%s)", fset.Position(call.Pos()), sel.Sel.Name, name)
			}
			return true
		})
	}
	if checked == 0 {
		t.Fatal("no calls inspected; the check is vacuous")
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
