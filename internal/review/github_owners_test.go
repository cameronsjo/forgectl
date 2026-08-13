package review

// Test plan for the #191 owner/host contract on the GitHub source.
//
// GitHub.Items (Classification: concurrent enumeration / low-trust config)
//   [x] Invariant: every gh call — discovery, issues, prs — is pinned to
//       github.com under a hostile ambient GH_HOST
//   [x] Happy: configured owners skip discovery; an absent list discovers once
//   [x] Boundary: at most maxGitHubQueryConcurrency queries in flight
//   [x] Invariant: notes fold in owner order, issues before prs, whatever the
//       completion order
//   [x] Unhappy: a failed query's note is categorical and carries no gh stderr
//   [x] Unhappy: every query failing satisfies errors.Is on the safe sentinel
//       and preserves context identity without the raw cause

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/githubauth"
)

// hookRunner is an exec.Runner double that can act on the caller's context —
// exec.FakeRunner ignores ctx entirely, so it cannot express "the subprocess
// failed because the context was cancelled".
type hookRunner struct {
	mu       sync.Mutex
	calls    []exec.Call
	inFlight int
	peak     int

	// respond produces stdout/error per call; hook runs before returning.
	respond func(args []string) (string, error)
	hook    func(context.Context)
}

func (r *hookRunner) record(env map[string]string, name string, args []string) {
	r.mu.Lock()
	r.calls = append(r.calls, exec.Call{Name: name, Args: args, Env: env})
	r.inFlight++
	r.peak = max(r.peak, r.inFlight)
	r.mu.Unlock()
}

func (r *hookRunner) done() {
	r.mu.Lock()
	r.inFlight--
	r.mu.Unlock()
}

func (r *hookRunner) call(ctx context.Context, env map[string]string, name string, args []string) (string, error) {
	r.record(env, name, args)
	defer r.done()
	if r.hook != nil {
		r.hook(ctx)
	}
	if r.respond != nil {
		return r.respond(args)
	}
	return "[]", nil
}

func (r *hookRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	return r.call(ctx, nil, name, args)
}

func (r *hookRunner) RunWithEnv(ctx context.Context, env map[string]string, name string, args ...string) (string, error) {
	return r.call(ctx, env, name, args)
}

func (r *hookRunner) RunWithInput(ctx context.Context, _ string, name string, args ...string) (string, error) {
	return r.call(ctx, nil, name, args)
}

func (r *hookRunner) RunInteractive(context.Context, string, ...string) error { return nil }

func (r *hookRunner) snapshot() ([]exec.Call, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]exec.Call(nil), r.calls...), r.peak
}

// isDiscovery reports whether a recorded call is githubauth's login question.
func isDiscovery(c exec.Call) bool {
	return c.Name == "gh" && strings.Join(c.Args, " ") == "api user --jq .login"
}

func TestGitHubItems_PinsEveryLegToGitHubCom(t *testing.T) {
	t.Setenv("GH_HOST", "ghe.example.test")
	run := &hookRunner{respond: func(args []string) (string, error) {
		if strings.Join(args, " ") == "api user --jq .login" {
			return "octocat", nil
		}
		return "[]", nil
	}}

	if _, _, err := NewGitHub(run, nil).Items(context.Background()); err != nil {
		t.Fatalf("Items: %v", err)
	}

	calls, _ := run.snapshot()
	var discoveries, issues, prs int
	for _, c := range calls {
		if c.Name != "gh" {
			continue
		}
		if got := c.Env["GH_HOST"]; got != githubauth.Host {
			t.Errorf("call %v ran with GH_HOST=%q, want %q", c.Args, got, githubauth.Host)
		}
		switch {
		case isDiscovery(c):
			discoveries++
		case searchLeg(c.Args) == "issues":
			issues++
		case searchLeg(c.Args) == "prs":
			prs++
		}
	}
	if discoveries != 1 || issues != 1 || prs != 1 {
		t.Errorf("discoveries=%d issues=%d prs=%d, want 1 each", discoveries, issues, prs)
	}
}

func TestGitHubItems_ConfiguredOwnersSkipDiscovery(t *testing.T) {
	run := &hookRunner{}

	if _, _, err := NewGitHub(run, []string{"alpha", "Alpha", "beta"}).Items(context.Background()); err != nil {
		t.Fatalf("Items: %v", err)
	}

	calls, _ := run.snapshot()
	legs := map[string]int{}
	for _, c := range calls {
		if isDiscovery(c) {
			t.Error("configured owners must make no discovery call")
		}
		if leg := searchLeg(c.Args); leg != "" {
			legs[leg]++
		}
	}
	// Two unique owners after case-insensitive dedup → two legs each.
	if legs["issues"] != 2 || legs["prs"] != 2 {
		t.Errorf("legs = %v, want 2 issues and 2 prs for two unique owners", legs)
	}
}

func TestGitHubItems_BoundsConcurrentQueries(t *testing.T) {
	owners := make([]string, githubauth.MaxOwners)
	for i := range owners {
		owners[i] = "owner" + strings.Repeat("z", i)
	}
	run := &hookRunner{
		hook:    func(context.Context) { time.Sleep(time.Millisecond) },
		respond: func([]string) (string, error) { return "[]", nil },
	}

	if _, _, err := NewGitHub(run, owners).Items(context.Background()); err != nil {
		t.Fatalf("Items: %v", err)
	}

	calls, peak := run.snapshot()
	if len(calls) != 2*githubauth.MaxOwners {
		t.Errorf("queries = %d, want two per owner", len(calls))
	}
	// The literal is deliberate. Asserting against maxGitHubQueryConcurrency
	// alone would make this test agree with any value the constant took,
	// including one that reintroduces the unbounded fan-out — the eight-query
	// ceiling is the contract, not an implementation detail.
	const contractCeiling = 8
	if maxGitHubQueryConcurrency > contractCeiling {
		t.Errorf("maxGitHubQueryConcurrency = %d, want at most %d", maxGitHubQueryConcurrency, contractCeiling)
	}
	if peak > contractCeiling {
		t.Errorf("peak concurrent queries = %d, want at most %d", peak, contractCeiling)
	}
	// Guard the guard: a peak of 1 would satisfy the bound while proving the
	// counter — or the concurrency — is broken rather than the cap working.
	if peak < 2 {
		t.Errorf("peak concurrent queries = %d, want the queries to actually overlap", peak)
	}
}

func TestGitHubItems_FoldsInOwnerOrderRegardlessOfCompletion(t *testing.T) {
	// The first owner's issues leg finishes last; the fold must not notice.
	run := &hookRunner{respond: func(args []string) (string, error) {
		if searchLeg(args) == "issues" && strings.Contains(strings.Join(args, " "), "alpha") {
			time.Sleep(30 * time.Millisecond)
		}
		return "", errors.New("gh: \x1b[31mghp_deadbeef\x1b[0m")
	}}

	_, notes, err := NewGitHub(run, []string{"alpha", "beta"}).Items(context.Background())

	if err == nil {
		t.Fatal("every query failing must be an error")
	}
	want := []string{
		"issues(alpha): query failed",
		"prs(alpha): query failed",
		"issues(beta): query failed",
		"prs(beta): query failed",
	}
	if len(notes) != len(want) {
		t.Fatalf("notes = %v, want %v", notes, want)
	}
	for i, w := range want {
		if notes[i] != w {
			t.Fatalf("notes = %v, want %v (owner order, issues before prs)", notes, want)
		}
	}
	if strings.Contains(strings.Join(notes, " "), "ghp_deadbeef") {
		t.Errorf("notes leaked raw gh stderr: %v", notes)
	}
	if !errors.Is(err, ErrGitHubQueriesUnavailable) {
		t.Errorf("err = %v, want errors.Is ErrGitHubQueriesUnavailable", err)
	}
	if strings.Contains(err.Error(), "ghp_deadbeef") || strings.Contains(err.Error(), "\x1b") {
		t.Errorf("aggregate error leaked raw gh stderr: %v", err)
	}
}

func TestGitHubItems_PartialFailureKeepsHealthyRows(t *testing.T) {
	run := &hookRunner{respond: func(args []string) (string, error) {
		switch searchLeg(args) {
		case "issues":
			return "", errors.New("gh: \x1b[31mrate limited\x1b[0m")
		case "prs":
			return "[" + prRow("cameronsjo/forgectl", 1) + "]", nil
		}
		return "[]", nil
	}}

	items, notes, err := NewGitHub(run, []string{"cameronsjo"}).Items(context.Background())

	if err != nil {
		t.Fatalf("a partial failure must not fail the source: %v", err)
	}
	if len(items) != 1 {
		t.Errorf("healthy leg's rows must survive; got %d", len(items))
	}
	if len(notes) != 1 || notes[0] != "issues(cameronsjo): query failed" {
		t.Fatalf("notes = %v, want the categorical issues note", notes)
	}
}

func TestGitHubItems_PreservesContextIdentityWithoutRawCause(t *testing.T) {
	for _, tc := range []struct {
		name string
		want error
	}{
		{"canceled", context.Canceled},
		{"deadline", context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			deadlineCtx, cancelDeadline := context.WithTimeout(context.Background(), time.Millisecond)
			defer cancelDeadline()
			if tc.want == context.DeadlineExceeded {
				ctx = deadlineCtx
			}
			run := &hookRunner{
				hook: func(c context.Context) {
					if tc.want == context.Canceled {
						cancel()
						return
					}
					<-c.Done()
				},
				respond: func([]string) (string, error) {
					return "", errors.New("gh: \x1b[31msignal: killed\x1b[0m")
				},
			}

			_, _, err := NewGitHub(run, []string{"cameronsjo"}).Items(ctx)

			if !errors.Is(err, ErrGitHubQueriesUnavailable) {
				t.Fatalf("err = %v, want errors.Is ErrGitHubQueriesUnavailable", err)
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want errors.Is %v", err, tc.want)
			}
			if strings.Contains(err.Error(), "signal: killed") {
				t.Errorf("err leaked the raw runner cause: %v", err)
			}
		})
	}
}

func TestAggregate_NeverReturnsOrRendersARawSourceError(t *testing.T) {
	hostile := errors.New("tea: \x1b[31mtoken abc123 rejected\x1b[0m")
	src := fakeSource{name: "gitea", notes: []string{"gitea(cameron): query failed"}, err: hostile}

	_, notes, err := Aggregate(context.Background(), src)

	if err == nil {
		t.Fatal("every source failing must be an error")
	}
	if errors.Is(err, hostile) {
		t.Error("the aggregate error must not wrap the source's raw error")
	}
	joined := strings.Join(notes, " ") + " " + err.Error()
	if strings.Contains(joined, "abc123") || strings.Contains(joined, "\x1b") {
		t.Errorf("aggregate leaked raw subprocess output: notes=%v err=%v", notes, err)
	}
	if len(notes) != 2 || notes[1] != "gitea: source failed" {
		t.Fatalf("notes = %v, want the source's own note plus a categorical one", notes)
	}
}

func TestAggregate_PropagatesContextIdentityInFixedOrder(t *testing.T) {
	deadline := fakeSource{name: "github", err: context.DeadlineExceeded}
	canceled := fakeSource{name: "gitea", err: context.Canceled}

	_, _, err := Aggregate(context.Background(), deadline, canceled)

	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want both context sentinels preserved", err)
	}
	if !strings.Contains(err.Error(), "review: every source failed") {
		t.Errorf("err = %v, want the aggregate message retained", err)
	}
	if i, j := strings.Index(err.Error(), "deadline"), strings.Index(err.Error(), "canceled"); i > j {
		t.Errorf("err = %q, want deadline before canceled for a stable rendering", err)
	}
}

func TestAggregate_HealthySourceSurvivesAFailingOne(t *testing.T) {
	healthy := fakeSource{name: "github", items: []Item{{Kind: KindIssue, Host: GitHubHost, Owner: "o", Repo: "r", Number: 1}}}
	broken := fakeSource{name: "gitea", err: errors.New("tea: boom")}

	items, _, err := Aggregate(context.Background(), healthy, broken)

	if err != nil {
		t.Fatalf("one healthy source must keep the aggregate nil-error: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("items = %+v, want the healthy source's row", items)
	}
}
