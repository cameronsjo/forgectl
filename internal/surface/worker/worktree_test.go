package worker

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	fexec "github.com/cameronsjo/forgectl/internal/exec"
)

// gitRepo makes a repo with one commit and returns its real top. Global and
// system git config are cut off: a developer's global core.hooksPath would
// otherwise suppress the hook the control below needs to see run.
func gitRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@example.com")
	dir := t.TempDir()
	mustGit(t, dir, "init", "-q", "-b", "main")
	mustGit(t, dir, "commit", "-q", "--allow-empty", "-m", "init")
	top, err := RepoTop(context.Background(), fexec.OSRunner{}, dir)
	if err != nil {
		t.Fatalf("RepoTop: %v", err)
	}
	return top
}

func mustGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := fexec.OSRunner{}.Run(context.Background(), "git", append([]string{"-C", dir}, args...)...)
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return out
}

// installHook writes a post-checkout hook that leaves a marker when it runs.
func installHook(t *testing.T, top string) string {
	t.Helper()
	marker := filepath.Join(t.TempDir(), "hook-ran")
	hook := filepath.Join(top, ".git", "hooks", "post-checkout")
	if err := os.MkdirAll(filepath.Dir(hook), 0o750); err != nil {
		t.Fatal(err)
	}
	// #nosec G306 -- a hook must be executable to be a valid control.
	if err := os.WriteFile(hook, []byte("#!/bin/sh\ntouch '"+marker+"'\n"), 0o750); err != nil {
		t.Fatal(err)
	}
	return marker
}

// TestAddWorktreeRunsNoRepoHooks is the hooksPath control, staged both ways:
// the same hook runs on a plain `git worktree add` and does not run through
// AddWorktree. Without the first half, a machine-wide hooksPath would make the
// second half pass for the wrong reason.
func TestAddWorktreeRunsNoRepoHooks(t *testing.T) {
	top := gitRepo(t)
	marker := installHook(t, top)

	mustGit(t, top, "worktree", "add", "-q", "-b", "control", filepath.Join(t.TempDir(), "control"))
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("control: the hook did not run on a plain worktree add, so this test cannot see it: %v", err)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}

	wt, err := AddWorktree(context.Background(), fexec.OSRunner{}, top, "w1", "feat/w1")
	if err != nil {
		t.Fatalf("AddWorktree: %v", err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the repo's post-checkout hook ran during AddWorktree")
	}
	if want := WorktreePath(top, "w1"); wt.Path != want {
		t.Errorf("Path = %q, want %q", wt.Path, want)
	}
	head := strings.TrimSpace(mustGit(t, top, "rev-parse", "HEAD"))
	if wt.Base != head {
		t.Errorf("Base = %q, want the checkout's HEAD %q", wt.Base, head)
	}
	if got := strings.TrimSpace(mustGit(t, wt.Path, "branch", "--show-current")); got != "feat/w1" {
		t.Errorf("worktree branch = %q, want feat/w1", got)
	}
}

// TestAddWorktreeUsesAnExistingLocalBranch checks the branch is reused, not
// recreated, and its own commit is the base.
func TestAddWorktreeUsesAnExistingLocalBranch(t *testing.T) {
	top := gitRepo(t)
	mustGit(t, top, "branch", "existing")
	mustGit(t, top, "commit", "-q", "--allow-empty", "-m", "on main")

	wt, err := AddWorktree(context.Background(), fexec.OSRunner{}, top, "w2", "existing")
	if err != nil {
		t.Fatalf("AddWorktree: %v", err)
	}
	want := strings.TrimSpace(mustGit(t, top, "rev-parse", "existing"))
	if wt.Base != want {
		t.Errorf("Base = %q, want the existing branch tip %q", wt.Base, want)
	}
}

// TestAddWorktreeRefusesASymlinkedClaudeDir: a repo can commit .claude as a
// symlink. Following it would put the worktree, and the worker with its
// looser fallback posture, outside the repo.
func TestAddWorktreeRefusesASymlinkedClaudeDir(t *testing.T) {
	for _, linked := range []string{".claude", ".claude/worktrees"} {
		t.Run(linked, func(t *testing.T) {
			top := gitRepo(t)
			outside := t.TempDir()
			link := filepath.Join(top, linked)
			if err := os.MkdirAll(filepath.Dir(link), 0o750); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, link); err != nil {
				t.Fatal(err)
			}

			_, err := AddWorktree(context.Background(), fexec.OSRunner{}, top, "w3", "feat/w3")
			if !errors.Is(err, ErrUnsafeWorktreeRoot) {
				t.Fatalf("err = %v, want ErrUnsafeWorktreeRoot", err)
			}
			entries, _ := os.ReadDir(outside)
			if len(entries) != 0 {
				t.Errorf("something was created through the symlink: %v", entries)
			}
		})
	}
}

// TestAddWorktreeRefusesAnExistingPath keeps a second launch from reusing a
// directory a previous worker may still hold work in.
func TestAddWorktreeRefusesAnExistingPath(t *testing.T) {
	top := gitRepo(t)
	if err := os.MkdirAll(WorktreePath(top, "w4"), 0o750); err != nil {
		t.Fatal(err)
	}
	if _, err := AddWorktree(context.Background(), fexec.OSRunner{}, top, "w4", "feat/w4"); err == nil {
		t.Fatal("AddWorktree reused an existing directory")
	}
}

func TestAddWorktreeRefusesBadNamesAndBranches(t *testing.T) {
	top := gitRepo(t)
	for _, tc := range []struct {
		name, branch string
		want         error
	}{
		{"../escape", "feat/x", ErrInvalidName},
		{"a/b", "feat/x", ErrInvalidName},
		{"-flag", "feat/x", ErrInvalidName},
		{"Upper", "feat/x", ErrInvalidName},
		{"", "feat/x", ErrInvalidName},
		{"ok", "-b", ErrInvalidBranch},
		{"ok", "feat/../x", ErrInvalidBranch},
		{"ok", "feat\x1b[31m", ErrInvalidBranch},
		{"ok", "", ErrInvalidBranch},
	} {
		_, err := AddWorktree(context.Background(), fexec.OSRunner{}, top, tc.name, tc.branch)
		if !errors.Is(err, tc.want) {
			t.Errorf("AddWorktree(%q, %q) err = %v, want %v", tc.name, tc.branch, err, tc.want)
		}
	}
	if _, err := os.Lstat(filepath.Join(top, ".claude")); !errors.Is(err, os.ErrNotExist) {
		t.Error("a refused call still created .claude")
	}
}
