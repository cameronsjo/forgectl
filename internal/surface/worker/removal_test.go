package worker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	fexec "github.com/cameronsjo/forgectl/internal/exec"
)

func TestRemovalBlockers(t *testing.T) {
	clean := WorktreeFacts{Path: "/r/.claude/worktrees/w", Listed: true, OnDisk: true, Branch: "feat", Ahead: 0}
	if b := RemovalBlockers(clean); len(b) != 0 {
		t.Fatalf("a clean worktree is blocked: %q", b)
	}
	cases := map[string]struct {
		edit func(*WorktreeFacts)
		want string
	}{
		"dirty":                   {func(f *WorktreeFacts) { f.Status = "!! .env\n" }, "git status"},
		"detached":                {func(f *WorktreeFacts) { f.Branch = "" }, "detached"},
		"ahead of upstream":       {func(f *WorktreeFacts) { f.Upstream, f.Ahead = "origin/feat", 2 }, "2 commit(s) not on origin/feat"},
		"ahead of base":           {func(f *WorktreeFacts) { f.Ahead = 1 }, "beyond the worktree's base"},
		"base unknown":            {func(f *WorktreeFacts) { f.Ahead = -1 }, "base commit is unknown"},
		"stash":                   {func(f *WorktreeFacts) { f.Stashes = 1 }, "stash"},
		"submodules":              {func(f *WorktreeFacts) { f.Submodules = []string{"sm"} }, "holds submodules ("},
		"on disk, not a worktree": {func(f *WorktreeFacts) { f.Listed = false }, "does not list it"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f := clean
			c.edit(&f)
			b := RemovalBlockers(f)
			if len(b) != 1 || !strings.Contains(b[0], c.want) {
				t.Fatalf("blockers %q, want one naming %q", b, c.want)
			}
		})
	}
}

func TestStashesOn(t *testing.T) {
	list := "WIP on feat: 1a2b3c4 init\nOn feat: saved\nWIP on feat-2: 1a2b3c4 init\nOn main: other\n"
	if n := stashesOn(list, "feat"); n != 2 {
		t.Fatalf("stashesOn = %d, want 2 (a branch whose name extends feat must not count)", n)
	}
}

// TestInspectWorktree drives each removal check against real git: a worktree
// fresh from AddWorktree is removable, and each of the plan's negative
// controls keeps it. The branch survives every removal.
func TestInspectWorktree(t *testing.T) {
	ctx := context.Background()
	run := fexec.OSRunner{}

	setup := func(t *testing.T) (top string, wt Worktree) {
		t.Helper()
		top = gitRepo(t)
		wt, err := AddWorktree(ctx, run, top, "w", "feat", baseOf(t, top))
		if err != nil {
			t.Fatalf("AddWorktree: %v", err)
		}
		return top, wt
	}
	inspect := func(t *testing.T, top, base string) WorktreeFacts {
		t.Helper()
		f, err := InspectWorktree(ctx, run, top, "w", base)
		if err != nil {
			t.Fatalf("InspectWorktree: %v", err)
		}
		return f
	}

	t.Run("a fresh worktree is removable, and the branch survives", func(t *testing.T) {
		top, wt := setup(t)
		f := inspect(t, top, wt.Base)
		if b := RemovalBlockers(f); len(b) != 0 || f.Path != wt.Path {
			t.Fatalf("facts %+v, blockers %q", f, b)
		}
		if err := RemoveWorktree(ctx, run, top, f.Path); err != nil {
			t.Fatalf("RemoveWorktree: %v", err)
		}
		if _, err := os.Stat(wt.Path); !os.IsNotExist(err) {
			t.Fatalf("worktree still there: %v", err)
		}
		mustGit(t, top, "rev-parse", "--verify", "refs/heads/feat")
		if f := inspect(t, top, wt.Base); f.Present() {
			t.Fatalf("a removed worktree reads as present: %+v", f)
		}
	})

	controls := map[string]struct {
		want  string
		stage func(t *testing.T, top string, wt Worktree)
	}{
		"an ignored file": {"git status", func(t *testing.T, _ string, wt Worktree) {
			writeFile(t, filepath.Join(wt.Path, ".gitignore"), ".env\n")
			mustGit(t, wt.Path, "add", ".gitignore")
			mustGit(t, wt.Path, "commit", "-q", "-m", "ignore")
			mustGit(t, wt.Path, "push", "-q", "origin", "HEAD:feat-up")
			mustGit(t, wt.Path, "branch", "-q", "--set-upstream-to=origin/feat-up")
			writeFile(t, filepath.Join(wt.Path, ".env"), "SECRET=1\n")
		}},
		"a detached HEAD with a commit": {"detached", func(t *testing.T, _ string, wt Worktree) {
			mustGit(t, wt.Path, "checkout", "-q", "--detach")
			mustGit(t, wt.Path, "commit", "-q", "--allow-empty", "-m", "detached work")
		}},
		"an unpushed commit": {"beyond the worktree's base", func(t *testing.T, _ string, wt Worktree) {
			mustGit(t, wt.Path, "commit", "-q", "--allow-empty", "-m", "work")
		}},
		"a stash entry": {"stash", func(t *testing.T, _ string, wt Worktree) {
			writeFile(t, filepath.Join(wt.Path, "f"), "x\n")
			mustGit(t, wt.Path, "stash", "push", "-q", "-u", "-m", "parked")
		}},
		"a populated submodule": {"holds submodules (", func(t *testing.T, _ string, wt Worktree) {
			addSubmodule(t, wt.Path, gitRepo(t))
			mustGit(t, wt.Path, "commit", "-q", "-m", "sm")
		}},
	}
	for name, c := range controls {
		t.Run("keeps it: "+name, func(t *testing.T) {
			top, wt := setup(t)
			// The ignored-file control needs an upstream: a self-remote.
			mustGit(t, top, "remote", "add", "origin", top)
			c.stage(t, top, wt)
			b := RemovalBlockers(inspect(t, top, wt.Base))
			if !slices.ContainsFunc(b, func(s string) bool { return strings.Contains(s, c.want) }) {
				t.Fatalf("blockers %q, want one naming %q", b, c.want)
			}
			mustGit(t, top, "rev-parse", "--verify", "refs/heads/feat")
		})
	}
}

func writeFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestInspectWorktreeEdges(t *testing.T) {
	ctx := context.Background()
	run := fexec.OSRunner{}

	t.Run("an unpushed commit on a branch with an upstream", func(t *testing.T) {
		top := gitRepo(t)
		wt, err := AddWorktree(ctx, run, top, "w", "feat", baseOf(t, top))
		if err != nil {
			t.Fatal(err)
		}
		mustGit(t, top, "remote", "add", "origin", top)
		mustGit(t, wt.Path, "push", "-q", "origin", "HEAD:feat-up")
		mustGit(t, wt.Path, "branch", "-q", "--set-upstream-to=origin/feat-up")
		mustGit(t, wt.Path, "commit", "-q", "--allow-empty", "-m", "work")
		f, err := InspectWorktree(ctx, run, top, "w", wt.Base)
		if err != nil {
			t.Fatal(err)
		}
		if f.Upstream != "origin/feat-up" || f.Ahead != 1 {
			t.Fatalf("facts %+v, want 1 commit ahead of origin/feat-up", f)
		}
	})

	t.Run("a base that is not a commit id is unknown, never trusted", func(t *testing.T) {
		top := gitRepo(t)
		wt, err := AddWorktree(ctx, run, top, "w", "feat", baseOf(t, top))
		if err != nil {
			t.Fatal(err)
		}
		for _, base := range []string{"", "HEAD~1", "--output=/tmp/x", strings.ToUpper(wt.Base), wt.Base + "0"} {
			f, err := InspectWorktree(ctx, run, top, "w", base)
			if err != nil {
				t.Fatal(err)
			}
			if f.Ahead != -1 {
				t.Fatalf("base %q gave Ahead %d, want -1", base, f.Ahead)
			}
		}
	})

	t.Run("RemoveWorktree refuses a path not directly under the worktree root", func(t *testing.T) {
		top := gitRepo(t)
		for _, p := range []string{
			filepath.Join(top, ".claude", "worktrees", "w", "sub"),
			filepath.Join(top, ".claude", "worktrees-x", "w"),
			top,
		} {
			if err := RemoveWorktree(ctx, run, top, p); err == nil {
				t.Fatalf("RemoveWorktree(%q) was not refused", p)
			}
		}
	})
}

// TestInspectWorktreeRunsNoWorkerFilter is the canary for the worker-written
// filter driver: a clean filter on a stat-dirty tracked file must not run
// when close inspects or removes the worktree.
func TestInspectWorktreeRunsNoWorkerFilter(t *testing.T) {
	ctx := context.Background()
	run := fexec.OSRunner{}
	top := gitRepo(t)
	wt, err := AddWorktree(ctx, run, top, "w", "feat", baseOf(t, top))
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "filter-ran")
	writeFile(t, filepath.Join(wt.Path, ".gitattributes"), "*.txt filter=canary\n")
	writeFile(t, filepath.Join(wt.Path, "a.txt"), "x\n")
	mustGit(t, wt.Path, "add", ".gitattributes", "a.txt")
	mustGit(t, wt.Path, "commit", "-q", "-m", "files")
	mustGit(t, top, "config", "filter.canary.clean", "touch '"+marker+"'; cat")
	// A new mtime makes the entry stat-dirty, so status re-hashes it.
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(filepath.Join(wt.Path, "a.txt"), later, later); err != nil {
		t.Fatal(err)
	}
	if _, err := InspectWorktree(ctx, run, top, "w", wt.Base); err != nil {
		t.Fatalf("InspectWorktree: %v", err)
	}
	_ = RemoveWorktree(ctx, run, top, wt.Path)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a filter driver ran during close's git calls")
	}
}

func TestInspectWorktreeRefusesARedirectedGitFile(t *testing.T) {
	ctx := context.Background()
	run := fexec.OSRunner{}
	top := gitRepo(t)
	if _, err := AddWorktree(ctx, run, top, "w", "feat", baseOf(t, top)); err != nil {
		t.Fatal(err)
	}
	other := gitRepo(t)
	writeFile(t, filepath.Join(WorktreePath(top, "w"), ".git"), "gitdir: "+filepath.Join(other, ".git")+"\n")
	if _, err := InspectWorktree(ctx, run, top, "w", ""); !errors.Is(err, ErrUnsafeWorktreeRoot) {
		t.Fatalf("a redirected .git was not refused: %v", err)
	}
}

// TestInspectWorktreeRunsNoHook is the canary for a worker-selected hook:
// status refreshing a stat-dirty entry rewrites the index, which would fire
// post-index-change.
func TestInspectWorktreeRunsNoHook(t *testing.T) {
	ctx := context.Background()
	run := fexec.OSRunner{}
	top := gitRepo(t)
	wt, err := AddWorktree(ctx, run, top, "w", "feat", baseOf(t, top))
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(wt.Path, "a.txt"), "x\n")
	mustGit(t, wt.Path, "add", "a.txt")
	mustGit(t, wt.Path, "commit", "-q", "-m", "a")
	marker := filepath.Join(t.TempDir(), "hook-ran")
	hooks := t.TempDir()
	// #nosec G306 -- a hook must be executable to be a valid control.
	if err := os.WriteFile(filepath.Join(hooks, "post-index-change"), []byte("#!/bin/sh\ntouch '"+marker+"'\n"), 0o750); err != nil {
		t.Fatal(err)
	}
	mustGit(t, top, "config", "core.hooksPath", hooks)
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(filepath.Join(wt.Path, "a.txt"), later, later); err != nil {
		t.Fatal(err)
	}
	if _, err := InspectWorktree(ctx, run, top, "w", wt.Base); err != nil {
		t.Fatalf("InspectWorktree: %v", err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a hook ran during close's git status")
	}
}

func TestCheckGitFile(t *testing.T) {
	ctx := context.Background()
	run := fexec.OSRunner{}

	t.Run("a relative gitfile git wrote is accepted", func(t *testing.T) {
		top := gitRepo(t)
		mustGit(t, top, "config", "worktree.useRelativePaths", "true")
		wt, err := AddWorktree(ctx, run, top, "w", "feat", baseOf(t, top))
		if err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(filepath.Join(wt.Path, ".git"))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "gitdir: /") {
			t.Skip("this git writes absolute gitfiles (worktree.useRelativePaths needs git 2.48)")
		}
		if _, err := InspectWorktree(ctx, run, top, "w", wt.Base); err != nil {
			t.Fatalf("a relative gitfile was refused: %v", err)
		}
	})

	t.Run("a gitfile naming the worktrees dir's parent is refused", func(t *testing.T) {
		top := gitRepo(t)
		wt, err := AddWorktree(ctx, run, top, "w", "feat", baseOf(t, top))
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(wt.Path, ".git"), "gitdir: "+top+"/.git/worktrees/..\n")
		if _, err := InspectWorktree(ctx, run, top, "w", ""); !errors.Is(err, ErrUnsafeWorktreeRoot) {
			t.Fatalf("not refused: %v", err)
		}
	})
}

// swapAfterCheck is a Runner that rewrites the worktree's gitfile to name
// another repository just before the first git call after InspectWorktree
// reads the common git dir, so after checkGitFile has passed: a process that
// outlived the workspace swapping it between the check and the calls that
// follow.
type swapAfterCheck struct {
	fexec.OSRunner
	swap           func() error
	armed, swapped bool
}

func (s *swapAfterCheck) RunWithEnvFiltered(ctx context.Context, env map[string]string, unset []string, name string, args ...string) (string, error) {
	if s.armed && !s.swapped {
		s.swapped = true
		if err := s.swap(); err != nil {
			return "", err
		}
	}
	if slices.Contains(args, "--git-common-dir") {
		s.armed = true
	}
	return s.OSRunner.RunWithEnvFiltered(ctx, env, unset, name, args...)
}

// Every git call after the gitfile check runs against the git dir the check
// found, so a gitfile swapped in after it is never read (#1304): the facts
// are the worker's own branch, not the swapped-in repository's. Mutation:
// run the calls with -C path alone (Branch comes back "evil"), or only the
// status (it reports the tracked file as untracked).
func TestInspectWorktreePinsTheCheckedGitDir(t *testing.T) {
	ctx := context.Background()
	top := gitRepo(t)
	// A tracked file, which the swapped-in repository's empty index would
	// report as untracked.
	writeFile(t, filepath.Join(top, "tracked"), "x")
	mustGit(t, top, "add", "tracked")
	mustGit(t, top, "commit", "-q", "-m", "tracked")
	wt, err := AddWorktree(ctx, fexec.OSRunner{}, top, "w", "feat", baseOf(t, top))
	if err != nil {
		t.Fatal(err)
	}
	evil := filepath.Join(t.TempDir(), "evil")
	if err := os.Mkdir(evil, 0o750); err != nil {
		t.Fatal(err)
	}
	mustGit(t, evil, "init", "-q", "-b", "evil")
	mustGit(t, evil, "commit", "-q", "--allow-empty", "-m", "evil")

	gitfile := filepath.Join(wt.Path, ".git")
	run := &swapAfterCheck{swap: func() error {
		return os.WriteFile(gitfile, []byte("gitdir: "+filepath.Join(evil, ".git")+"\n"), 0o600)
	}}
	f, err := InspectWorktree(ctx, run, top, "w", wt.Base)
	if err != nil {
		t.Fatal(err)
	}
	if !run.swapped {
		t.Fatal("the gitfile was never swapped; the test no longer models the window")
	}
	if f.Branch != "feat" || f.Status != "" || f.Ahead != 0 {
		t.Fatalf("facts = branch %q, status %q, ahead %d; want feat, clean, 0 from the checked git dir", f.Branch, f.Status, f.Ahead)
	}
}

// addSubmodule adds the repository sub to the working tree dir as sm.
func addSubmodule(t *testing.T, dir, sub string) {
	t.Helper()
	mustGit(t, dir, "-c", "protocol.file.allow=always", "submodule", "add", "-q", sub, "sm")
}

// swapBeforeStatus is a Runner that runs swap just before InspectWorktree's
// git status, so after RunUnfiltered has listed every repository's drivers.
type swapBeforeStatus struct {
	fexec.OSRunner
	swap    func() error
	swapped bool
}

func (s *swapBeforeStatus) RunWithEnvFiltered(ctx context.Context, env map[string]string, unset []string, name string, args ...string) (string, error) {
	if !s.swapped && slices.Contains(args, "status") {
		s.swapped = true
		if err := s.swap(); err != nil {
			return "", err
		}
	}
	return s.OSRunner.RunWithEnvFiltered(ctx, env, unset, name, args...)
}

// A submodule populated after the driver listing, by a .git naming a
// repository whose config defines a driver nobody listed, never runs that
// driver: status does not enter the submodule (#1338). Mutation: drop
// --ignore-submodules=dirty from the status, and the child status in sm runs
// the driver.
func TestInspectWorktreeRunsNoLatePopulatedSubmoduleDriver(t *testing.T) {
	ctx := context.Background()
	top := gitRepo(t)
	wt, err := AddWorktree(ctx, fexec.OSRunner{}, top, "w", "feat", baseOf(t, top))
	if err != nil {
		t.Fatal(err)
	}
	// evil's index lists a.txt with stat data that does not match sm's
	// copy, so status in sm would re-hash it through evil's driver.
	evil := gitRepo(t)
	writeFile(t, filepath.Join(evil, ".gitattributes"), "*.txt filter=evil\n")
	writeFile(t, filepath.Join(evil, "a.txt"), "x\n")
	mustGit(t, evil, "add", ".gitattributes", "a.txt")
	mustGit(t, evil, "commit", "-q", "-m", "files")
	marker := filepath.Join(t.TempDir(), "filter-ran")
	mustGit(t, evil, "config", "filter.evil.clean", "touch '"+marker+"'; cat")

	// An unpopulated gitlink sm, with evil's files laid out but no .git, so
	// neither submodulesIn nor the listing finds a submodule there.
	sm := filepath.Join(wt.Path, "sm")
	if err := os.Mkdir(sm, 0o750); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(sm, ".gitattributes"), "*.txt filter=evil\n")
	writeFile(t, filepath.Join(sm, "a.txt"), "x\n")
	mustGit(t, wt.Path, "update-index", "--add", "--cacheinfo", "160000,"+headOf(t, evil)+",sm")
	mustGit(t, wt.Path, "commit", "-q", "-m", "sm")

	run := &swapBeforeStatus{swap: func() error {
		return os.WriteFile(filepath.Join(sm, ".git"), []byte("gitdir: "+filepath.Join(evil, ".git")+"\n"), 0o600)
	}}
	if _, err := InspectWorktree(ctx, run, top, "w", wt.Base); err != nil {
		t.Fatalf("InspectWorktree: %v", err)
	}
	if !run.swapped {
		t.Fatal("sm was never populated before status; the test no longer models the window")
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a driver from the late-populated submodule ran during close's git status")
	}
}

// A staged gitlink change is still reported: --ignore-submodules=all would
// hide it, and removing the worktree would lose it.
func TestInspectWorktreeReportsAStagedGitlink(t *testing.T) {
	ctx := context.Background()
	run := fexec.OSRunner{}
	top := gitRepo(t)
	wt, err := AddWorktree(ctx, run, top, "w", "feat", baseOf(t, top))
	if err != nil {
		t.Fatal(err)
	}
	// Staged without populating: only the index holds it.
	mustGit(t, wt.Path, "update-index", "--add", "--cacheinfo", "160000,"+headOf(t, top)+",sm")
	f, err := InspectWorktree(ctx, run, top, "w", wt.Base)
	if err != nil {
		t.Fatalf("InspectWorktree: %v", err)
	}
	if len(f.Submodules) != 0 || !strings.Contains(f.Status, "sm") {
		t.Fatalf("Submodules = %q, Status = %q; want none, and the staged gitlink sm", f.Submodules, f.Status)
	}
}

// The blocker names at most maxNamedSubmodules of the worker's paths, each
// quoted, and counts the rest.
func TestSubmoduleList(t *testing.T) {
	got := submoduleList([]string{"a", "b\x1b[2J", "c", "d", "e"})
	if strings.ContainsRune(got, 0x1b) || !strings.HasSuffix(got, "and 2 more") {
		t.Fatalf("submoduleList = %q", got)
	}
}

// A gitfile naming another worktree's git dir is refused: its gitdir file
// names that worktree, not this one, and the pinned calls would report the
// other worktree's branch (#1338). Mutation: drop checkBackLink.
func TestInspectWorktreeRefusesAnotherWorktreesGitDir(t *testing.T) {
	ctx := context.Background()
	run := fexec.OSRunner{}
	top := gitRepo(t)
	wt, err := AddWorktree(ctx, run, top, "w", "feat", baseOf(t, top))
	if err != nil {
		t.Fatal(err)
	}
	other, err := AddWorktree(ctx, run, top, "v", "other", baseOf(t, top))
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(other.Path, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(wt.Path, ".git"), string(data))
	if _, err := InspectWorktree(ctx, run, top, "w", ""); !errors.Is(err, ErrUnsafeWorktreeRoot) {
		t.Fatalf("another worktree's git dir was not refused: %v", err)
	}
	if _, err := InspectWorktree(ctx, run, top, "v", ""); err != nil {
		t.Fatalf("the other worktree's own gitfile was refused: %v", err)
	}
}

// submodulesIn names each gitlink once, and counts a gitlink whose .git it
// cannot rule out as a submodule rather than failing close.
func TestSubmodulesIn(t *testing.T) {
	dir := t.TempDir()
	gitDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "populated"), 0o750); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "populated", ".git"), "")
	if err := os.Mkdir(filepath.Join(dir, "empty"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("loop", filepath.Join(dir, "loop")); err != nil {
		t.Fatal(err)
	}
	const oid = "0123456789012345678901234567890123456789"
	index := "160000 " + oid + " 1\tpopulated\x00" +
		"160000 " + oid + " 2\tpopulated\x00" +
		"160000 " + oid + " 0\tempty\x00" +
		"160000 " + oid + " 0\tabsent\x00" +
		"160000 " + oid + " 0\tloop\x00" +
		"100644 " + oid + " 0\tfile\x00"
	got, err := submodulesIn(dir, gitDir, func(...string) (string, error) { return index, nil })
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"populated", "loop"}; !slices.Equal(got, want) {
		t.Fatalf("submodulesIn = %q, want %q", got, want)
	}
}

// A gitlink whose .git RunUnfiltered would refuse (here an empty file)
// keeps the worktree with the submodule reason, not a failed close: status
// is skipped once a submodule is found. Mutation: run status anyway.
func TestInspectWorktreeNamesAnUnreadableSubmodule(t *testing.T) {
	ctx := context.Background()
	run := fexec.OSRunner{}
	top := gitRepo(t)
	wt, err := AddWorktree(ctx, run, top, "w", "feat", baseOf(t, top))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(wt.Path, "sm"), 0o750); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(wt.Path, "sm", ".git"), "")
	mustGit(t, wt.Path, "update-index", "--add", "--cacheinfo", "160000,"+headOf(t, top)+",sm")
	f, err := InspectWorktree(ctx, run, top, "w", wt.Base)
	if err != nil {
		t.Fatalf("InspectWorktree: %v", err)
	}
	if !slices.Contains(f.Submodules, "sm") {
		t.Fatalf("Submodules = %q, want sm", f.Submodules)
	}
}
