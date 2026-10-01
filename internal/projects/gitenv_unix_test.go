//go:build unix

package projects

import (
	"context"
	osexec "os/exec"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/gitenv"
	"github.com/cameronsjo/forgectl/internal/gitenv/gitenvtest"
)

// TestGitStatusNeverLazyFetches runs the projects status probe, through the
// production Runner and the absolute git New resolves, in a partial clone
// whose HEAD commit is missing and whose promisor remote is an ext:: URL the
// repository allows. A modelled pre-2.44 git ignores GIT_NO_LAZY_FETCH, so
// gitenv's empty GIT_ALLOW_PROTOCOL is all that stands between the
// repository and its canary (cameronsjo/forgectl#944).
//
// Mutation: run the probe as run.Run(ctx, gitBin, ...) in gitStatus, or drop
// GIT_ALLOW_PROTOCOL from gitenv's Local pins: the canary runs.
func TestGitStatusNeverLazyFetches(t *testing.T) {
	gitenvtest.WithoutLazyFetchPin(t)
	gitenvtest.NewCanary(t).AssertLive(t)
	canary := gitenvtest.NewCanary(t)
	bin, err := osexec.LookPath(gitenv.Bin)
	if err != nil {
		t.Fatal(err)
	}

	got := gitStatus(context.Background(), exec.OSRunner{}, bin, canary.Dir)
	if canary.Ran(t) {
		t.Fatal("the projects status probe ran the repository's transport command through a lazy fetch")
	}
	if got.State != StatusUnknown {
		t.Errorf("gitStatus = %+v, want %q: the refused fetch leaves HEAD unreadable", got, StatusUnknown)
	}
}
