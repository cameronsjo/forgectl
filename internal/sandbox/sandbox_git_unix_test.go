//go:build unix

package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/gitenv"
	"github.com/cameronsjo/forgectl/internal/gitenv/gitenvtest"
)

// operatorAllowsExt points git at a global config that admits the ext::
// transport, as an operator's own protocol.ext.allow=always would, and clears
// the variables that would refuse or replace it before the clone sees it.
func operatorAllowsExt(t *testing.T) {
	t.Helper()
	gitenvtest.RequireGit(t)
	for _, key := range []string{"GIT_ALLOW_PROTOCOL", "GIT_CONFIG_PARAMETERS", "GIT_CONFIG_COUNT"} {
		t.Setenv(key, "")
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
	}
	global := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(global, []byte("[protocol \"ext\"]\n\tallow = always\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("TMPDIR", t.TempDir())
}

// #978 item 1: a workflow-supplied ext:: URL cannot run its command through
// the sandbox clone, even where the operator's own config admits ext::.
// The control clones the same URL with the options the sandbox clone carried
// before, and its command runs.
// Mutation: drop "-c protocol.ext.allow=never" from the clone's argv: the
// canary runs.
func TestSandbox_CloneRefusesExtEvenWhenTheOperatorAllowsIt(t *testing.T) {
	operatorAllowsExt(t)
	control := filepath.Join(t.TempDir(), "control")
	ctl := gitenv.Command(t.Context(), gitenv.Transport, "clone", "--", "ext::sh -c touch% "+control, filepath.Join(t.TempDir(), "c"))
	_ = ctl.Run()
	if _, err := os.Stat(control); err != nil {
		t.Fatalf("the control clone ran no ext:: command (%v); the fixture cannot fire", err)
	}

	canary := filepath.Join(t.TempDir(), "canary")
	if _, err := Sandbox(context.Background(), exec.OSRunner{}, "ext::sh -c touch% "+canary, "", true); err == nil {
		t.Error("the sandbox clone of an ext:: URL succeeded")
	}
	if _, err := os.Stat(canary); err == nil {
		t.Fatal("the sandbox clone ran the ext:: URL's command")
	}
}

// The refusal leaves a local path cloneable: alwaysClone of a local
// repository still clones it.
func TestSandbox_AlwaysCloneOfALocalPathStillWorks(t *testing.T) {
	operatorAllowsExt(t)
	src := filepath.Join(t.TempDir(), "src")
	if err := os.Mkdir(src, 0o750); err != nil {
		t.Fatal(err)
	}
	gitenvtest.Git(t, src, "init", "-q", "-b", "main")
	gitenvtest.Git(t, src, "commit", "-q", "--allow-empty", "-m", "c")

	dir, err := Sandbox(context.Background(), exec.OSRunner{}, src, "main", true)
	if err != nil {
		t.Fatalf("alwaysClone of a local path: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if got := gitenvtest.Git(t, dir, "log", "-1", "--format=%s"); got != "c" {
		t.Errorf("cloned HEAD subject = %q, want c", got)
	}
}
