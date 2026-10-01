//go:build unix

package projects

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/gitenv"
	"github.com/cameronsjo/forgectl/internal/gitenv/gitenvtest"
)

// #987: an inherited GIT_ALLOW_PROTOCOL naming ext, which git honours ahead
// of every protocol.allow setting, -c included, cannot admit a served ext::
// URL through the Gitea/SSH clone or bare clone. The control runs the clone
// these sites ran before, -c options and all, and its command runs. A local
// path still clones both ways, since the operator's list keeps file.
// Mutation: clone through gitenv.Run with the two -c options in either
// function, as before: its canary runs.
func TestGiteaClonesRefuseExtThatGitAllowProtocolAdmits(t *testing.T) {
	gitenvtest.OperatorAllowsExt(t)
	t.Setenv("GIT_ALLOW_PROTOCOL", "ext:file")
	control := filepath.Join(t.TempDir(), "control")
	ctl := gitenv.Command(t.Context(), gitenv.Transport, "-c", "protocol.ext.allow=never", "-c", "protocol.fd.allow=never", "clone", "--", "ext::sh -c touch% "+control, filepath.Join(t.TempDir(), "c"))
	_ = ctl.Run()
	if _, err := os.Stat(control); err != nil {
		t.Fatalf("the control clone ran no ext:: command (%v); the fixture cannot fire", err)
	}

	src := filepath.Join(t.TempDir(), "src")
	if err := os.Mkdir(src, 0o750); err != nil {
		t.Fatal(err)
	}
	gitenvtest.Git(t, src, "init", "-q", "-b", "main")
	gitenvtest.Git(t, src, "commit", "-q", "--allow-empty", "-m", "c")

	for name, clone := range map[string]func(context.Context, gitenv.Runner, string, string) error{
		"cloneFromGitea":   cloneFromGitea,
		"cloneBareFromURL": cloneBareFromURL,
	} {
		t.Run(name, func(t *testing.T) {
			canary := filepath.Join(t.TempDir(), "canary")
			if err := clone(context.Background(), exec.OSRunner{}, "ext::sh -c touch% "+canary, filepath.Join(t.TempDir(), "d")); err == nil {
				t.Error("the clone of an ext:: URL succeeded")
			}
			if _, err := os.Stat(canary); err == nil {
				t.Fatal("the clone ran the ext:: URL's command")
			}
			if err := clone(context.Background(), exec.OSRunner{}, src, filepath.Join(t.TempDir(), "local")); err != nil {
				t.Errorf("a local path under GIT_ALLOW_PROTOCOL=ext:file: %v", err)
			}
		})
	}
}

// #987's sweep: a checkout whose own config names an ext:: origin and allows
// it (protocol.ext.allow=always in .git/config) runs that origin's command
// under the `git pull --rebase` PullAll ran before, with no operator setting
// at all. PullAll's pull refuses it and reports the pull failed.
// Mutation: pull through gitenv.RunBin under Transport, as before: the
// canary runs.
func TestPullAllRefusesTheCheckoutsOwnExtRemote(t *testing.T) {
	gitenvtest.RequireGit(t)
	for _, key := range []string{"GIT_ALLOW_PROTOCOL", "GIT_CONFIG_PARAMETERS", "GIT_CONFIG_COUNT"} {
		t.Setenv(key, "")
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
	}
	global := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(global, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")

	root := t.TempDir()
	repo := filepath.Join(root, "hostile")
	if err := os.Mkdir(repo, 0o750); err != nil {
		t.Fatal(err)
	}
	canary := filepath.Join(t.TempDir(), "canary")
	gitenvtest.Git(t, repo, "init", "-q", "-b", "main")
	gitenvtest.Git(t, repo, "-c", "user.name=t", "-c", "user.email=t@example.test", "commit", "-q", "--allow-empty", "-m", "c")
	gitenvtest.Git(t, repo, "config", "protocol.ext.allow", "always")
	gitenvtest.Git(t, repo, "remote", "add", "origin", "ext::sh -c touch% "+canary)
	gitenvtest.Git(t, repo, "config", "branch.main.remote", "origin")
	gitenvtest.Git(t, repo, "config", "branch.main.merge", "refs/heads/main")

	ctl := gitenv.Command(t.Context(), gitenv.Transport, "-C", repo, "pull", "--rebase")
	_ = ctl.Run()
	if _, err := os.Stat(canary); err != nil {
		t.Fatalf("the control pull ran no ext:: command (%v); the fixture cannot fire", err)
	}
	if err := os.Remove(canary); err != nil {
		t.Fatal(err)
	}

	c := newWithRoot(exec.OSRunner{}, func() (string, error) { return root, nil })
	results, err := c.PullAll(context.Background(), root)
	if err != nil {
		t.Fatalf("PullAll: %v", err)
	}
	if _, err := os.Stat(canary); err == nil {
		t.Fatal("PullAll's pull ran the checkout's ext:: remote command")
	}
	if len(results) != 1 || results[0].Status != PullFailed {
		t.Errorf("results = %+v, want one %v", results, PullFailed)
	}
}
