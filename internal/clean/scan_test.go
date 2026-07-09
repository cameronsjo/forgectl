package clean

// See scan.go's own header comment for the full test plan this file
// implements — kept there (rather than duplicated here) since the task
// explicitly asked for it to open scan.go.

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// --- kindForName / ParseKind / matchesTypeFilter (pure) --------------------

func TestKindForName_KnownNames(t *testing.T) {
	cases := map[string]Kind{
		"node_modules": KindNode,
		".next":        KindNode,
		".svelte-kit":  KindNode,
		".venv":        KindPython,
		"venv":         KindPython,
		"__pycache__":  KindPython,
		"vendor":       KindGo,
		"target":       KindGo,
		"dist":         KindBuild,
		"build":        KindBuild,
	}
	for name, want := range cases {
		got, ok := kindForName(name)
		if !ok {
			t.Errorf("kindForName(%q): expected a match", name)
			continue
		}
		if got != want {
			t.Errorf("kindForName(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestKindForName_UnknownName(t *testing.T) {
	if _, ok := kindForName("src"); ok {
		t.Error("kindForName(\"src\") should not match any Kind")
	}
	if _, ok := kindForName(".git"); ok {
		t.Error("kindForName(\".git\") should never be a target — .git is handled separately by Scan, never via this map")
	}
}

func TestParseKind_RoundTrips(t *testing.T) {
	for _, k := range []Kind{KindNode, KindPython, KindGo, KindBuild} {
		got, err := ParseKind(string(k))
		if err != nil {
			t.Errorf("ParseKind(%q): unexpected error: %v", k, err)
		}
		if got != k {
			t.Errorf("ParseKind(%q) = %q, want %q", k, got, k)
		}
	}
}

func TestParseKind_UnknownIsError(t *testing.T) {
	if _, err := ParseKind("rust"); err == nil {
		t.Error("ParseKind(\"rust\"): expected an error, got nil")
	}
	if _, err := ParseKind(""); err == nil {
		t.Error(`ParseKind(""): expected an error — callers must treat "" as "no filter" before calling`)
	}
}

func TestMatchesTypeFilter_EmptyMatchesAll(t *testing.T) {
	for _, k := range []Kind{KindNode, KindPython, KindGo, KindBuild} {
		if !matchesTypeFilter(k, nil) {
			t.Errorf("matchesTypeFilter(%q, nil) = false, want true (empty filter matches everything)", k)
		}
	}
}

func TestMatchesTypeFilter_NonEmptyFiltersToListedKinds(t *testing.T) {
	types := []Kind{KindNode, KindGo}
	if !matchesTypeFilter(KindNode, types) {
		t.Error("KindNode should match a filter that includes it")
	}
	if matchesTypeFilter(KindPython, types) {
		t.Error("KindPython should not match a filter that omits it")
	}
}

// --- dirSize -----------------------------------------------------------

func TestDirSize_SumsFilesRecursively(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "a.txt"), 100)
	mustWriteFile(t, filepath.Join(root, "sub", "b.txt"), 250)
	mustWriteFile(t, filepath.Join(root, "sub", "deeper", "c.txt"), 7)

	got := dirSize(root)
	want := int64(100 + 250 + 7)
	if got != want {
		t.Errorf("dirSize = %d, want %d", got, want)
	}
}

func TestDirSize_BrokenSymlinkDoesNotAbort(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "a.txt"), 42)
	link := filepath.Join(root, "broken")
	if err := os.Symlink(filepath.Join(root, "does-not-exist"), link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	got := dirSize(root)
	if got != 42 {
		t.Errorf("dirSize with a broken symlink present = %d, want 42 (symlink itself contributes nothing, and must not abort the walk)", got)
	}
}

// --- Scan ----------------------------------------------------------------

func TestScan_FindsMatchedTargetSize(t *testing.T) {
	root := t.TempDir()
	nm := filepath.Join(root, "proj", "node_modules")
	mustWriteFile(t, filepath.Join(nm, "pkg", "index.js"), 300)
	mustWriteFile(t, filepath.Join(nm, "pkg", "readme.md"), 20)

	report, err := Scan(ScanOptions{Root: root})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(report.Targets) != 1 {
		t.Fatalf("expected 1 target, got %d: %+v", len(report.Targets), report.Targets)
	}
	got := report.Targets[0]
	if got.Path != nm {
		t.Errorf("Path = %q, want %q", got.Path, nm)
	}
	if got.Kind != KindNode {
		t.Errorf("Kind = %q, want %q", got.Kind, KindNode)
	}
	if got.Size != 320 {
		t.Errorf("Size = %d, want 320", got.Size)
	}
	if report.TotalSize != 320 {
		t.Errorf("TotalSize = %d, want 320", report.TotalSize)
	}
}

func TestScan_NeverDescendsIntoMatchedDir(t *testing.T) {
	root := t.TempDir()
	// A node_modules dir that (pathologically) contains its own nested
	// node_modules — Scan must report the OUTER one only; the nested one is
	// never separately identified as a second target.
	outer := filepath.Join(root, "proj", "node_modules")
	inner := filepath.Join(outer, "some-pkg", "node_modules")
	mustWriteFile(t, filepath.Join(inner, "leaf.js"), 10)
	mustWriteFile(t, filepath.Join(outer, "top.js"), 5)

	report, err := Scan(ScanOptions{Root: root})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(report.Targets) != 1 {
		t.Fatalf("expected exactly 1 target (nested match must not be reported separately), got %d: %+v", len(report.Targets), report.Targets)
	}
	if report.Targets[0].Path != outer {
		t.Errorf("Path = %q, want the OUTER node_modules %q", report.Targets[0].Path, outer)
	}
}

func TestScan_GitNeverMatchedOrDescended(t *testing.T) {
	root := t.TempDir()
	// A directory literally named "dist" that is ALSO its own git working
	// tree (a repo that happens to share a target name). Guarantee #1's
	// strict form: this must never be reported as a target at all.
	dist := filepath.Join(root, "proj", "dist")
	mustWriteFile(t, filepath.Join(dist, ".git", "HEAD"), 5)
	mustWriteFile(t, filepath.Join(dist, "real-file.txt"), 999)

	report, err := Scan(ScanOptions{Root: root})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(report.Targets) != 0 {
		t.Fatalf("expected 0 targets — a dir that is itself a git repo must never be a reclaim target, got %+v", report.Targets)
	}
}

func TestScan_NestedGitRepoInsideMatchedDir_NeverReported(t *testing.T) {
	// Regression: a matched dir ("build") that is NOT itself a git repo but
	// CONTAINS one nested a level or more deeper — e.g. someone's real
	// checkout placed inside a directory that happens to match a reclaim
	// name. hasOwnGit's old immediate-child-only check missed this
	// entirely: Scan pruned descent into "build" on the name match, so
	// nothing downstream (findProjectRoot walks upward only; the delete
	// path's containsGitComponent inspects only the target's OWN path) ever
	// saw the nested .git either. This must never be reported as a target.
	root := t.TempDir()
	build := filepath.Join(root, "proj", "build")
	mustWriteFile(t, filepath.Join(build, "my-experiment", ".git", "HEAD"), 5)
	mustWriteFile(t, filepath.Join(build, "my-experiment", "real-file.txt"), 999)

	report, err := Scan(ScanOptions{Root: root})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(report.Targets) != 0 {
		t.Fatalf("expected 0 targets — a matched dir containing a nested git repo must never be a reclaim target, got %+v", report.Targets)
	}
}

func TestScan_GitDirNeverTraversed_EvenAsAncestorMatch(t *testing.T) {
	root := t.TempDir()
	// A bare .git directory sitting directly under a scanned project; it must
	// never appear as a target and Scan must never look inside it, even
	// though .git's own internals (objects/, hooks/) resemble regular dirs.
	mustWriteFile(t, filepath.Join(root, "proj", ".git", "hooks", "pre-commit"), 3)
	mustWriteFile(t, filepath.Join(root, "proj", "node_modules", "leaf.js"), 11)

	report, err := Scan(ScanOptions{Root: root})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(report.Targets) != 1 {
		t.Fatalf("expected exactly 1 target (node_modules only), got %d: %+v", len(report.Targets), report.Targets)
	}
	if report.Targets[0].Kind != KindNode {
		t.Errorf("Kind = %q, want %q", report.Targets[0].Kind, KindNode)
	}
}

func TestScan_SymlinkedDirectoryNeverFollowed(t *testing.T) {
	root := t.TempDir()
	external := t.TempDir()
	mustWriteFile(t, filepath.Join(external, "node_modules", "leaf.js"), 500)

	link := filepath.Join(root, "proj", "node_modules")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.Symlink(filepath.Join(external, "node_modules"), link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	report, err := Scan(ScanOptions{Root: root})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(report.Targets) != 0 {
		t.Fatalf("expected 0 targets — a symlinked node_modules must never be followed/matched, got %+v", report.Targets)
	}
}

func TestScan_OlderThanExcludesRecentTargetButStillPrunes(t *testing.T) {
	root := t.TempDir()
	nm := filepath.Join(root, "proj", "node_modules")
	mustWriteFile(t, filepath.Join(nm, "leaf.js"), 10)

	report, err := Scan(ScanOptions{Root: root, OlderThan: 24 * time.Hour})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(report.Targets) != 0 {
		t.Fatalf("a freshly-created target should be excluded by a 24h --older-than filter, got %+v", report.Targets)
	}
}

func TestScan_TypeFilterStillPrunesDescentIntoExcludedMatch(t *testing.T) {
	root := t.TempDir()
	// A node_modules dir (excluded by the python-only filter below) that
	// nests a __pycache__ inside it — if Scan didn't prune descent into the
	// excluded node_modules match, it would incorrectly surface the nested
	// __pycache__ as a second target.
	nested := filepath.Join(root, "proj", "node_modules", "sub", "__pycache__")
	mustWriteFile(t, filepath.Join(nested, "x.pyc"), 4)

	report, err := Scan(ScanOptions{Root: root, Types: []Kind{KindPython}})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(report.Targets) != 0 {
		t.Fatalf("expected 0 targets — node_modules is excluded by the type filter AND descent into it must still be pruned, got %+v", report.Targets)
	}
}

func TestFindProjectRoot(t *testing.T) {
	root := t.TempDir()
	proj := filepath.Join(root, "proj")
	if err := os.MkdirAll(filepath.Join(proj, ".git"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	target := filepath.Join(proj, "node_modules")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	if got := findProjectRoot(root, target); got != proj {
		t.Errorf("findProjectRoot = %q, want %q", got, proj)
	}

	// No .git anywhere between root and an unversioned scratch target -> "".
	scratch := filepath.Join(root, "scratch", "node_modules")
	if err := os.MkdirAll(scratch, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if got := findProjectRoot(root, scratch); got != "" {
		t.Errorf("findProjectRoot for an unversioned target = %q, want \"\"", got)
	}
}

// mustWriteFile creates path (and its parent dirs) containing size bytes of
// filler content, failing the test on any error.
func mustWriteFile(t *testing.T, path string, size int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", filepath.Dir(path), err)
	}
	content := make([]byte, size)
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}
}
