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
		wt, err := AddWorktree(ctx, run, top, "w", "feat")
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
		wt, err := AddWorktree(ctx, run, top, "w", "feat")
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
		wt, err := AddWorktree(ctx, run, top, "w", "feat")
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
	wt, err := AddWorktree(ctx, run, top, "w", "feat")
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
	if _, err := AddWorktree(ctx, run, top, "w", "feat"); err != nil {
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
	wt, err := AddWorktree(ctx, run, top, "w", "feat")
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
		wt, err := AddWorktree(ctx, run, top, "w", "feat")
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
		wt, err := AddWorktree(ctx, run, top, "w", "feat")
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(wt.Path, ".git"), "gitdir: "+top+"/.git/worktrees/..\n")
		if _, err := InspectWorktree(ctx, run, top, "w", ""); !errors.Is(err, ErrUnsafeWorktreeRoot) {
			t.Fatalf("not refused: %v", err)
		}
	})
}
