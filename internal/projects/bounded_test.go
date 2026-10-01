package projects

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/gitenv/gitenvtest"
)

// deadlineRunner is a FakeRunner that records, for each `remote get-url`
// call, whether its context carried a deadline and how far off it was.
type deadlineRunner struct {
	*exec.FakeRunner
	mu    sync.Mutex
	lefts []time.Duration // -1 for a call with no deadline
}

func (d *deadlineRunner) RunWithEnvFiltered(ctx context.Context, env map[string]string, unset []string, name string, args ...string) (string, error) {
	if slices.Contains(args, "get-url") {
		left := time.Duration(-1)
		if dl, ok := ctx.Deadline(); ok {
			left = time.Until(dl)
		}
		d.mu.Lock()
		d.lefts = append(d.lefts, left)
		d.mu.Unlock()
	}
	return d.FakeRunner.RunWithEnvFiltered(ctx, env, unset, name, args...)
}

// assertBounded fails t unless every get-url ran under a deadline of at
// most 30 seconds.
func (d *deadlineRunner) assertBounded(t *testing.T, want int) {
	t.Helper()
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.lefts) != want {
		t.Fatalf("get-url ran %d time(s), want %d", len(d.lefts), want)
	}
	for _, left := range d.lefts {
		if left < 0 || left > 30*time.Second {
			t.Errorf("get-url ran with %v left before its deadline (-1: none), want at most 30s", left)
		}
	}
}

// #1005: the inventory's `remote get-url` in each local clone runs under
// gitenv.Bounded, since a FIFO HEAD blocks it as it blocks status (measured
// on git 2.43: `forgectl projects list` hung in a root holding one).
// Mutation: run localRepos' get-url under ctx rather than gitenv.Bounded:
// the call carries no deadline.
func TestLocalReposBoundsTheOriginLookup(t *testing.T) {
	root := t.TempDir()
	mkGitDir(t, root, "a")
	r := &deadlineRunner{FakeRunner: &exec.FakeRunner{RunFunc: gitenvtest.NoFilters(func(string, []string) (string, error) { return "", nil })}}
	c := newWithRoot(r, func() (string, error) { return root, nil })
	if _, err := c.localRepos(context.Background()); err != nil {
		t.Fatalf("localRepos: %v", err)
	}
	r.assertBounded(t, 1)
}

// #1005: originMatches runs its get-url under gitenv.Bounded too: dir is
// whatever checkout already sits at a placement.
// Mutation: drop gitenv.Bounded from originMatches: no deadline.
func TestOriginMatchesBoundsTheOriginLookup(t *testing.T) {
	r := &deadlineRunner{FakeRunner: &exec.FakeRunner{}}
	c := newWithRoot(r, func() (string, error) { return t.TempDir(), nil })
	_ = c.originMatches(context.Background(), t.TempDir(), Repo{Host: "github.com", Owner: "o", Name: "n"})
	r.assertBounded(t, 1)
}
