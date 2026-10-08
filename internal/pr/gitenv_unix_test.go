//go:build unix

package pr

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/gitenv/gitenvtest"
)

// TestResolveLocalHeadIgnoresAnInheritedGitDir runs `pr local`'s HEAD probe
// through the production Runner with GIT_DIR and GIT_WORK_TREE exported at
// another repository, as a git hook or a caller that exported them would. The
// probe must still read the path it was given (cameronsjo/forgectl#944).
//
// pr's git calls outside the review window are rev-parse and remote get-url,
// which read refs and config but no object, so no lazy fetch can reach them
// (measured on git 2.43: neither runs a promisor canary even unhardened).
// The inherited repository redirection is what gitenv changes for them.
//
// Mutation: run rev-parse in ResolveLocalHead through c.run.Run: it reports
// the other repository's HEAD.
func TestResolveLocalHeadIgnoresAnInheritedGitDir(t *testing.T) {
	gitenvtest.RequireGit(t)
	target, other := t.TempDir(), t.TempDir()
	for i, dir := range []string{target, other} {
		gitenvtest.Git(t, dir, "init", "-q", "-b", "main")
		if err := os.WriteFile(filepath.Join(dir, "f"), []byte{byte('a' + i)}, 0o600); err != nil {
			t.Fatal(err)
		}
		gitenvtest.Git(t, dir, "add", "f")
		gitenvtest.Git(t, dir, "commit", "-q", "-m", "c")
	}
	want := gitenvtest.Git(t, target, "rev-parse", "HEAD")
	if want == gitenvtest.Git(t, other, "rev-parse", "HEAD") {
		t.Fatal("the two repositories share a HEAD; the test could not tell them apart")
	}
	t.Setenv("GIT_DIR", filepath.Join(other, ".git"))
	t.Setenv("GIT_WORK_TREE", other)

	c := New(exec.OSRunner{}, WithSessionsDir(t.TempDir()), WithFindingsDir(t.TempDir()))
	_, got, err := c.ResolveLocalHead(context.Background(), target)
	if err != nil {
		t.Fatalf("ResolveLocalHead: %v", err)
	}
	if got != want {
		t.Fatalf("ResolveLocalHead = %s, want the target's own HEAD %s: an inherited GIT_DIR redirected the probe", got, want)
	}
}
