//go:build unix

package projects

import (
	"context"
	osexec "os/exec"
	"path/filepath"
	"testing"
	"time"

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

// The projects status probe re-hashes stat-dirty files through the filter
// drivers the repository names, a clean filter and a dotted-name process
// filter here; neither runs (#977).
// Mutation: run gitStatus's status through gitenv.RunBin under Local instead
// of gitenv.RunUnfiltered: the canary runs.
func TestGitStatusRunsNoFilterDriver(t *testing.T) {
	gitenvtest.NewFilterCanary(t).AssertLive(t)
	canary := gitenvtest.NewFilterCanary(t)
	bin, err := osexec.LookPath(gitenv.Bin)
	if err != nil {
		t.Fatal(err)
	}

	got := gitStatus(context.Background(), exec.OSRunner{}, bin, canary.Dir)
	if canary.Ran(t) {
		t.Fatal("the projects status probe ran a filter driver the repository defines")
	}
	if got.State != StatusOK || got.Modified != 0 {
		t.Errorf("gitStatus = %+v, want a clean %q: the files match their blobs", got, StatusOK)
	}
}

// #1005: a repository whose HEAD is a FIFO, on which git itself blocks,
// reads as unknown, and promptly: the probe never starts git there.
// Mutation: drop gitenv's HEAD mode check: git runs, blocks until the
// 30-second deadline, and the 10-second bound fails the test.
func TestGitStatusOfAFIFOHeadIsUnknownAtOnce(t *testing.T) {
	dir := gitenvtest.FIFOHeadRepo(t)
	bin, err := osexec.LookPath(gitenv.Bin)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan GitStatus, 1)
	go func() { done <- gitStatus(t.Context(), exec.OSRunner{}, bin, dir) }()
	select {
	case got := <-done:
		if got.State != StatusUnknown {
			t.Errorf("gitStatus = %+v, want %q", got, StatusUnknown)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the projects status probe did not return: git is blocked on the FIFO HEAD")
	}
}

// Review of #1008: the inventory leaves a repository whose HEAD is a FIFO
// unattributed at once, rather than waiting out the 30-second deadline on
// its `remote get-url`, and originMatches refuses one without asking git.
// Mutation: drop the gitenv.Blocks check in localRepos or in originMatches:
// a get-url runs.
func TestInventorySkipsTheOriginLookupOfARepoGitBlocksOn(t *testing.T) {
	dir := gitenvtest.FIFOHeadRepo(t)
	root := filepath.Dir(dir)
	r := &deadlineRunner{FakeRunner: &exec.FakeRunner{RunFunc: gitenvtest.NoFilters(func(string, []string) (string, error) { return "", nil })}}
	c := newWithRoot(r, func() (string, error) { return root, nil })
	repos, err := c.localRepos(context.Background())
	if err != nil {
		t.Fatalf("localRepos: %v", err)
	}
	if len(repos) != 1 || repos[0].Status.State != StatusUnknown {
		t.Errorf("repos = %+v, want the one repository, %q", repos, StatusUnknown)
	}
	if c.originMatches(context.Background(), dir, Repo{Host: "github.com", Owner: "o", Name: "n"}) {
		t.Error("originMatches matched a repository git blocks on")
	}
	r.assertBounded(t, 0)
}
