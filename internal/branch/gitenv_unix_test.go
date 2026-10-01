//go:build unix

package branch

import (
	"context"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/gitenv/gitenvtest"
)

// TestMergedLocallyNeverLazyFetches is TestGitStatusNeverLazyFetches
// (internal/projects) for the local merge check, which runs git in the
// working directory and walks history from the default branch
// (cameronsjo/forgectl#944). The for-each-ref listing reads refs only, and
// does not fire the canary even unhardened (measured on git 2.43).
//
// Mutation: run `git branch --merged` in mergedLocally through c.run.Run:
// the canary runs.
func TestMergedLocallyNeverLazyFetches(t *testing.T) {
	gitenvtest.WithoutLazyFetchPin(t)
	gitenvtest.NewCanary(t).AssertLive(t)
	canary := gitenvtest.NewCanary(t)
	t.Chdir(canary.Dir)

	_, err := New(exec.OSRunner{}).mergedLocally(context.Background(), "main")
	if canary.Ran(t) {
		t.Fatal("the local merge check ran the repository's transport command through a lazy fetch")
	}
	if err == nil {
		t.Error("mergedLocally succeeded with a default branch naming a missing commit")
	}
}
