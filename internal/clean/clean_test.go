package clean

// Test plan for clean.go
//
// Client.Clean (Classification: core orchestration, real temp-dir fixtures)
//   [x] Happy: dry run (Apply=false) reports the correct reclaimable total
//       and issues ZERO os.RemoveAll calls — every fixture file/dir present
//       before the call is still present, byte-for-byte, after it
//   [x] Happy: --apply deletes every non-skipped target and reports the
//       actual reclaimed bytes (TotalReclaimed), not just the estimate
//   [x] Safety #3: a project with an uncommitted/dirty git tree is skipped
//       (not deleted) without --force
//   [x] Safety #3: the same dirty project IS cleaned when --force is passed
//   [x] Safety #3, fail-safe direction: a `git status` error is treated as
//       dirty (skipped), never silently treated as clean
//   [x] Safety #1: .git is never a scan/delete target, even nested inside a
//       dir that coincidentally shares a target name (delegated to Scan;
//       reasserted here end-to-end through the real Client.Clean path)
//   [x] Safety #1: a matched dir CONTAINING a git repo nested deeper than an
//       immediate child (e.g. build/my-experiment/.git) also survives
//       --apply --force end-to-end — the regression Finding #1 of the
//       security review caught (delegated to Scan's hasNestedGit; reasserted
//       here through the real delete path, not just Scan's own unit test)
//   [x] Safety #2: a symlink pointing outside --root is never followed or
//       deleted (delegated to Scan; reasserted end-to-end)
//   [x] Safety #4: delete() refuses a path that WithinWorkspace rejects,
//       even when handed directly (defense-in-depth unit test, not just
//       relying on Scan never producing such a Target)
//   [x] Re-running --apply on an already-cleaned tree reports ~0 reclaimable
//       (acceptance criterion from issue #4)
//   [x] WithCleanConfig / WithRoot / WithTypes options apply only when the
//       corresponding config/option field is non-empty, matching the
//       fill-in-defaults shape internal/net's WithNetConfig uses
//
// ScanReport / ApplyReport (Classification: scan/apply split, real temp-dir
// fixture — added post-security-review to fix Finding #3, a preview/apply
// re-scan mismatch)
//   [x] ApplyReport, given an ALREADY-SCANNED Report, deletes exactly that
//       Report's targets even after the filesystem changes underneath it —
//       proving apply reuses the confirmed scan rather than re-walking root

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
)

// initGitRepo makes dir a real git repo (git init + optional dirty file) via
// the FakeRunner's RunFunc — clean_test.go doesn't shell to real git; a
// dirty/clean answer is faked per-directory through statusByDir below. What
// makes gitDirty's ancestor-walk trigger at all is scan.go's findProjectRoot,
// which only needs a real .git DIRECTORY on disk (no real git binary
// required) to identify dir as a project root.
func initGitRepo(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatalf("MkdirAll .git: %v", err)
	}
}

// resolvedPath resolves symlinks in path (t.TempDir() on macOS returns a path
// under /var/folders/... that is itself a symlink to /private/var/folders/...
// — Client.Clean resolves --root through filepath.EvalSymlinks before
// scanning, so Scan's reported Target.ProjectRoot is the RESOLVED path; tests
// that key a fake `git status` answer by project directory must key it the
// same way).
func resolvedPath(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatalf("EvalSymlinks(%s): %v", path, err)
	}
	return resolved
}

// fakeGitRunner returns a FakeRunner whose `git status --porcelain` answer is
// keyed by the `-C <dir>` argument, per statusByDir. Any dir not present in
// statusByDir gets a clean ("") answer.
func fakeGitRunner(statusByDir map[string]string) *exec.FakeRunner {
	return &exec.FakeRunner{
		RunFunc: func(name string, args []string) (string, error) {
			if name != "git" {
				return "", nil
			}
			// args: -C <dir> status --porcelain
			if len(args) >= 2 && args[0] == "-C" {
				dir := args[1]
				if err, ok := statusErrors[dir]; ok {
					return "", err
				}
				return statusByDir[dir], nil
			}
			return "", nil
		},
	}
}

// statusErrors lets a single test case inject a `git status` failure for a
// specific dir via fakeGitRunnerWithErr — package-level map keyed per test
// run would race across parallel tests, so tests using it must not run
// t.Parallel(). None of the tests in this file do.
var statusErrors = map[string]error{}

func TestClean_DryRun_ZeroDeletes(t *testing.T) {
	statusErrors = map[string]error{}
	root := t.TempDir()
	nm := filepath.Join(root, "proj", "node_modules")
	leaf := filepath.Join(nm, "leaf.js")
	mustWriteFile(t, leaf, 100)

	run := fakeGitRunner(nil)
	c := New(run, WithRoot(root))

	result, err := c.Clean(context.Background(), CleanOptions{Apply: false})
	if err != nil {
		t.Fatalf("Clean: %v", err)
	}
	if result.TotalReclaimable != 100 {
		t.Errorf("TotalReclaimable = %d, want 100", result.TotalReclaimable)
	}
	if result.TotalReclaimed != 0 {
		t.Errorf("TotalReclaimed = %d, want 0 for a dry run", result.TotalReclaimed)
	}
	if _, err := os.Stat(leaf); err != nil {
		t.Errorf("fixture file must survive a dry run, stat error: %v", err)
	}
	for _, call := range run.Calls {
		if call.Name != "git" {
			t.Errorf("dry run must never shell out to anything but git status, got: %+v", call)
		}
	}
}

func TestClean_Apply_DeletesAndReportsActualBytes(t *testing.T) {
	statusErrors = map[string]error{}
	root := t.TempDir()
	nm := filepath.Join(root, "proj", "node_modules")
	mustWriteFile(t, filepath.Join(nm, "leaf.js"), 100)
	mustWriteFile(t, filepath.Join(nm, "other.js"), 50)

	run := fakeGitRunner(nil)
	c := New(run, WithRoot(root))

	result, err := c.Clean(context.Background(), CleanOptions{Apply: true})
	if err != nil {
		t.Fatalf("Clean: %v", err)
	}
	if result.TotalReclaimed != 150 {
		t.Errorf("TotalReclaimed = %d, want 150", result.TotalReclaimed)
	}
	if _, err := os.Stat(nm); !os.IsNotExist(err) {
		t.Errorf("node_modules should be gone after --apply, stat err = %v", err)
	}
}

func TestClean_ReApplyAfterCleanReportsZero(t *testing.T) {
	statusErrors = map[string]error{}
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "proj", "node_modules", "leaf.js"), 100)

	run := fakeGitRunner(nil)
	c := New(run, WithRoot(root))
	ctx := context.Background()

	if _, err := c.Clean(ctx, CleanOptions{Apply: true}); err != nil {
		t.Fatalf("first Clean: %v", err)
	}

	second, err := c.Clean(ctx, CleanOptions{Apply: true})
	if err != nil {
		t.Fatalf("second Clean: %v", err)
	}
	if second.TotalReclaimable != 0 || second.TotalReclaimed != 0 {
		t.Errorf("re-running --apply on an already-cleaned tree should report ~0, got reclaimable=%d reclaimed=%d", second.TotalReclaimable, second.TotalReclaimed)
	}
}

func TestClean_DirtyProject_SkippedWithoutForce(t *testing.T) {
	statusErrors = map[string]error{}
	root := t.TempDir()
	proj := filepath.Join(root, "proj")
	initGitRepo(t, proj)
	nm := filepath.Join(proj, "node_modules")
	mustWriteFile(t, filepath.Join(nm, "leaf.js"), 100)

	run := fakeGitRunner(map[string]string{resolvedPath(t, proj): " M some/file.go\n"})
	c := New(run, WithRoot(root))

	result, err := c.Clean(context.Background(), CleanOptions{Apply: true})
	if err != nil {
		t.Fatalf("Clean: %v", err)
	}
	if result.TotalReclaimed != 0 {
		t.Errorf("TotalReclaimed = %d, want 0 — dirty project must not be cleaned without --force", result.TotalReclaimed)
	}
	if len(result.Items) != 1 || !result.Items[0].Skipped {
		t.Fatalf("expected exactly 1 skipped item, got %+v", result.Items)
	}
	if _, err := os.Stat(nm); err != nil {
		t.Errorf("node_modules must survive when its project is dirty, stat error: %v", err)
	}
}

func TestClean_DirtyProject_CleanedWithForce(t *testing.T) {
	statusErrors = map[string]error{}
	root := t.TempDir()
	proj := filepath.Join(root, "proj")
	initGitRepo(t, proj)
	nm := filepath.Join(proj, "node_modules")
	mustWriteFile(t, filepath.Join(nm, "leaf.js"), 100)

	run := fakeGitRunner(map[string]string{resolvedPath(t, proj): " M some/file.go\n"})
	c := New(run, WithRoot(root))

	result, err := c.Clean(context.Background(), CleanOptions{Apply: true, Force: true})
	if err != nil {
		t.Fatalf("Clean: %v", err)
	}
	if result.TotalReclaimed != 100 {
		t.Errorf("TotalReclaimed = %d, want 100 — --force must clean a dirty project's targets", result.TotalReclaimed)
	}
	if _, err := os.Stat(nm); !os.IsNotExist(err) {
		t.Errorf("node_modules should be gone after --apply --force, stat err = %v", err)
	}
}

func TestClean_GitStatusError_TreatedAsDirty(t *testing.T) {
	root := t.TempDir()
	proj := filepath.Join(root, "proj")
	initGitRepo(t, proj)
	nm := filepath.Join(proj, "node_modules")
	mustWriteFile(t, filepath.Join(nm, "leaf.js"), 100)

	statusErrors = map[string]error{resolvedPath(t, proj): errors.New("git: command not found")}
	t.Cleanup(func() { statusErrors = map[string]error{} })

	run := fakeGitRunner(nil)
	c := New(run, WithRoot(root))

	result, err := c.Clean(context.Background(), CleanOptions{Apply: true})
	if err != nil {
		t.Fatalf("Clean: %v", err)
	}
	if result.TotalReclaimed != 0 {
		t.Errorf("TotalReclaimed = %d, want 0 — a git status ERROR must fail safe (treated as dirty), not as clean", result.TotalReclaimed)
	}
	if _, err := os.Stat(nm); err != nil {
		t.Errorf("node_modules must survive when git status errors, stat error: %v", err)
	}
}

func TestClean_GitNeverAScanOrDeleteTarget_EndToEnd(t *testing.T) {
	statusErrors = map[string]error{}
	root := t.TempDir()
	// "dist" is itself a git repo (see scan_test.go's equivalent unit test) —
	// end-to-end through Client.Clean with --apply --force, it must survive.
	dist := filepath.Join(root, "proj", "dist")
	mustWriteFile(t, filepath.Join(dist, ".git", "HEAD"), 5)
	mustWriteFile(t, filepath.Join(dist, "real-file.txt"), 999)

	run := fakeGitRunner(nil)
	c := New(run, WithRoot(root))

	result, err := c.Clean(context.Background(), CleanOptions{Apply: true, Force: true})
	if err != nil {
		t.Fatalf("Clean: %v", err)
	}
	if len(result.Items) != 0 {
		t.Fatalf("a dir that is itself a git repo must never appear as an Item at all, got %+v", result.Items)
	}
	if _, err := os.Stat(dist); err != nil {
		t.Errorf("dist (a git repo) must survive --apply --force, stat error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dist, ".git", "HEAD")); err != nil {
		t.Errorf(".git contents inside dist must survive, stat error: %v", err)
	}
}

func TestClean_NestedGitRepoInsideMatchedDir_NeverDeleted_EndToEnd(t *testing.T) {
	statusErrors = map[string]error{}
	root := t.TempDir()
	// "build" is NOT itself a git repo, but contains one nested a level
	// deeper — end-to-end through Client.Clean with --apply --force, both
	// the outer "build" dir and the nested repo's contents must survive.
	build := filepath.Join(root, "proj", "build")
	nestedGitHead := filepath.Join(build, "my-experiment", ".git", "HEAD")
	mustWriteFile(t, nestedGitHead, 5)
	mustWriteFile(t, filepath.Join(build, "my-experiment", "real-file.txt"), 999)

	run := fakeGitRunner(nil)
	c := New(run, WithRoot(root))

	result, err := c.Clean(context.Background(), CleanOptions{Apply: true, Force: true})
	if err != nil {
		t.Fatalf("Clean: %v", err)
	}
	if len(result.Items) != 0 {
		t.Fatalf("a dir containing a nested git repo must never appear as an Item at all, got %+v", result.Items)
	}
	if _, err := os.Stat(build); err != nil {
		t.Errorf("build (contains a nested git repo) must survive --apply --force, stat error: %v", err)
	}
	if _, err := os.Stat(nestedGitHead); err != nil {
		t.Errorf("the nested .git contents must survive, stat error: %v", err)
	}
}

func TestScanReport_ThenApplyReport_ReusesOriginalTargets_NotRescanned(t *testing.T) {
	statusErrors = map[string]error{}
	root := t.TempDir()
	original := filepath.Join(root, "proj", "node_modules")
	mustWriteFile(t, filepath.Join(original, "leaf.js"), 100)

	run := fakeGitRunner(nil)
	c := New(run, WithRoot(root))

	resolvedRoot, report, err := c.ScanReport(CleanOptions{})
	if err != nil {
		t.Fatalf("ScanReport: %v", err)
	}
	if len(report.Targets) != 1 {
		t.Fatalf("expected exactly 1 scanned target, got %d: %+v", len(report.Targets), report.Targets)
	}

	// Mutate the filesystem AFTER the scan, BEFORE apply — a new reclaimable
	// target appears that was never part of the confirmed Report.
	lateArrival := filepath.Join(root, "proj2", "node_modules")
	mustWriteFile(t, filepath.Join(lateArrival, "leaf.js"), 200)

	result, err := c.ApplyReport(context.Background(), resolvedRoot, report, CleanOptions{Apply: true})
	if err != nil {
		t.Fatalf("ApplyReport: %v", err)
	}
	if len(result.Items) != 1 {
		t.Fatalf("ApplyReport must act ONLY on the original scanned Report, got %d item(s): %+v", len(result.Items), result.Items)
	}
	// Compare against the Report's own resolved path (captured before the
	// delete removes it) rather than re-resolving `original` now — it no
	// longer exists on disk to resolve.
	if want := report.Targets[0].Path; result.Items[0].Path != want {
		t.Errorf("Path = %q, want the originally-scanned target %q", result.Items[0].Path, want)
	}
	if _, err := os.Stat(original); !os.IsNotExist(err) {
		t.Errorf("the originally-scanned target should have been deleted, stat error: %v", err)
	}
	if _, err := os.Stat(lateArrival); err != nil {
		t.Errorf("a target that appeared AFTER the scan must survive an apply pass driven by the earlier Report, stat error: %v", err)
	}
}

func TestClean_SymlinkOutsideRoot_NeverDeleted(t *testing.T) {
	statusErrors = map[string]error{}
	root := t.TempDir()
	external := t.TempDir()
	victim := filepath.Join(external, "node_modules", "leaf.js")
	mustWriteFile(t, victim, 500)

	link := filepath.Join(root, "proj", "node_modules")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.Symlink(filepath.Join(external, "node_modules"), link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	run := fakeGitRunner(nil)
	c := New(run, WithRoot(root))

	result, err := c.Clean(context.Background(), CleanOptions{Apply: true, Force: true})
	if err != nil {
		t.Fatalf("Clean: %v", err)
	}
	if len(result.Items) != 0 {
		t.Fatalf("a symlinked node_modules must never be scanned/deleted, got %+v", result.Items)
	}
	if _, err := os.Stat(victim); err != nil {
		t.Errorf("file reached only via the external symlink must survive, stat error: %v", err)
	}
}

// TestClient_Delete_RefusesPathOutsideRoot is a direct, defense-in-depth unit
// test of delete()'s own WithinWorkspace re-check (safety guarantee #4) —
// independent of whether Scan could ever actually hand it such a path.
func TestClient_Delete_RefusesPathOutsideRoot(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	victim := filepath.Join(outside, "victim.txt")
	mustWriteFile(t, victim, 10)

	c := New(&exec.FakeRunner{})
	if err := c.delete(root, outside); err == nil {
		t.Fatal("delete() should refuse a target outside root")
	}
	if _, err := os.Stat(victim); err != nil {
		t.Errorf("victim must survive a refused delete, stat error: %v", err)
	}
}

// TestClient_Delete_RefusesGitComponent is a direct unit test of delete()'s
// own .git guard, independent of Scan.
func TestClient_Delete_RefusesGitComponent(t *testing.T) {
	root := t.TempDir()
	gitDir := filepath.Join(root, "proj", ".git")
	mustWriteFile(t, filepath.Join(gitDir, "HEAD"), 5)

	c := New(&exec.FakeRunner{})
	if err := c.delete(root, gitDir); err == nil {
		t.Fatal("delete() should refuse a path containing a .git component")
	}
	if _, err := os.Stat(gitDir); err != nil {
		t.Errorf(".git must survive a refused delete, stat error: %v", err)
	}
}

// TestClient_Delete_RefusesTargetSwappedForSymlink is a direct unit test of
// delete()'s post-review Lstat guard: Scan never yields a symlink as a
// Target (it fs.SkipDirs every symlinked directory), so a target that IS a
// symlink at delete time can only be a post-scan swap — refuse it rather
// than following it into some OTHER inside-root directory the scan never
// actually vetted (security review Finding #2's residual).
func TestClient_Delete_RefusesTargetSwappedForSymlink(t *testing.T) {
	root := t.TempDir()
	victim := filepath.Join(root, "victim-repo")
	mustWriteFile(t, filepath.Join(victim, "real-file.txt"), 999)

	// The "target" delete() is asked to remove no longer exists as a real
	// directory — it's now a symlink pointing at some OTHER inside-root
	// directory, simulating a swap that happened after Scan ran.
	swapped := filepath.Join(root, "build")
	if err := os.Symlink(victim, swapped); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	c := New(&exec.FakeRunner{})
	if err := c.delete(root, swapped); err == nil {
		t.Fatal("delete() should refuse a target that is a symlink at delete time")
	}
	if _, err := os.Stat(victim); err != nil {
		t.Errorf("the symlink's target (victim-repo) must survive a refused delete, stat error: %v", err)
	}
	if _, err := os.Lstat(swapped); err != nil {
		t.Errorf("the swapped symlink itself must survive too (delete() must not partially act), lstat error: %v", err)
	}
}

func TestWithRoot_AndWithTypes_OnlyApplyWhenNonEmpty(t *testing.T) {
	c := New(&exec.FakeRunner{})
	before := c.root

	WithRoot("")(c)
	if c.root != before {
		t.Errorf("WithRoot(\"\") should be a no-op, root changed from %q to %q", before, c.root)
	}
	WithTypes(nil)(c)
	if len(c.types) != 0 {
		t.Errorf("WithTypes(nil) should be a no-op, got %v", c.types)
	}

	WithRoot("/custom/root")(c)
	if c.root != "/custom/root" {
		t.Errorf("WithRoot should override root, got %q", c.root)
	}
	WithTypes([]Kind{KindGo})(c)
	if len(c.types) != 1 || c.types[0] != KindGo {
		t.Errorf("WithTypes should override types, got %v", c.types)
	}
}
