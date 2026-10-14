//go:build unix

package projects

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/gitenv/gitenvtest"
)

// localRemoteRunner runs Worktree's git calls on real git, except the two
// that reach the remote: the bare clone copies src instead of the URL, and
// then selects hooks for the new bare repository through its own config, as
// a fetched branch would through a relative core.hooksPath; the fetch is a
// no-op, since the clone already holds every branch.
type localRemoteRunner struct {
	exec.OSRunner
	t     *testing.T
	src   string
	hooks string
}

func (r localRemoteRunner) RunWithEnvFiltered(ctx context.Context, env map[string]string, unset []string, name string, args ...string) (string, error) {
	plain := gitenvtest.Strip(args)
	switch {
	case slices.Contains(plain, "clone"):
		dest := plain[len(plain)-1]
		gitenvtest.Git(r.t, "", "clone", "-q", "--bare", r.src, dest)
		gitenvtest.Git(r.t, dest, "config", "core.hooksPath", r.hooks)
		return "", nil
	case slices.Contains(plain, "fetch"):
		return "", nil
	}
	return r.OSRunner.RunWithEnvFiltered(ctx, env, unset, name, args...)
}

// TestWorktreeRunsNoHook: `projects worktree` checks out a branch it has
// just fetched, and the post-checkout hook its configured hooks path names
// does not run (#1049). Global and system git config are cut off, so an
// operator's own core.hooksPath cannot mask the control.
// Mutation: drop the core.hooksPath pin from Worktree's adds: the hook runs.
func TestWorktreeRunsNoHook(t *testing.T) {
	gitenvtest.RequireGit(t)
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	tmp := t.TempDir()
	src := filepath.Join(tmp, "src")
	hooks := filepath.Join(tmp, "hooks")
	marker := filepath.Join(tmp, "hook-ran")
	for _, d := range []string{src, hooks} {
		if err := os.Mkdir(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	gitenvtest.Git(t, src, "init", "-q", "-b", "main")
	gitenvtest.Git(t, src, "commit", "-q", "--allow-empty", "-m", "control")
	//nolint:gosec // G306: a hook must be executable to be a live control
	if err := os.WriteFile(filepath.Join(hooks, "post-checkout"), []byte("#!/bin/sh\ntouch '"+marker+"'\n"), 0o750); err != nil {
		t.Fatal(err)
	}

	// The control: the same add, unpinned, runs the hook.
	ctl := filepath.Join(tmp, "control.git")
	gitenvtest.Git(t, "", "clone", "-q", "--bare", src, ctl)
	gitenvtest.Git(t, ctl, "config", "core.hooksPath", hooks)
	gitenvtest.Git(t, ctl, "worktree", "add", "-q", filepath.Join(tmp, "control"), "main")
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("an unpinned worktree add ran no hook; the fixture cannot fire: %v", err)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}

	run := localRemoteRunner{t: t, src: src, hooks: hooks}
	c := &Client{Dir: filepath.Join(tmp, "projects"), run: run, gitBin: "git"}
	dir, err := c.Worktree(t.Context(), Repo{Host: "git.example.test", Owner: "o", Name: "r", SSHURL: "ssh://git.example.test/o/r.git"}, "main")
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		t.Fatalf("Worktree made no checkout at %s: %v", dir, err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a post-checkout hook ran during projects worktree's add")
	}
}
