package audit

import (
	"os"
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
	if r := scan(t, Options{Root: root, MaxFindings: 1}); !r.Truncated || len(r.Findings) != 1 {
		t.Errorf("MaxFindings=1: truncated=%v findings=%d, want true/1", r.Truncated, len(r.Findings))
	}
	if r := scan(t, Options{Root: root, MaxEntries: 3}); !r.Truncated || r.Entries > 4 {
		t.Errorf("MaxEntries=3: truncated=%v entries=%d, want true and at most one over", r.Truncated, r.Entries)
	}
	r := scan(t, Options{Root: root, MaxDepth: 4})
	if !r.Truncated {
		t.Error("MaxDepth=4: want truncated")
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
