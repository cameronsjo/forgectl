//go:build unix

package cli

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/surface/drain"
	"github.com/cameronsjo/forgectl/internal/surface/worker"
)

const (
	identityFound  = `{"data":{"repository":{"databaseId":1252924951,"nameWithOwner":"cameronsjo/forgectl","ref":null}}}`
	identityHasRef = `{"data":{"repository":{"databaseId":1252924951,"nameWithOwner":"cameronsjo/forgectl","ref":{"name":"worker/w1"}}}}`
)

// identityFake answers workerRepoIdentity's git and gh calls. localRef is
// the one ref rev-parse finds ("" for none).
func identityFake(origin, ghOut string, ghErr error, localRef string) *exec.FakeRunner {
	return &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		joined := strings.Join(args, " ")
		switch {
		case name == "gh":
			return ghOut, ghErr
		case strings.Contains(joined, "get-url origin"):
			if origin == "" {
				return "", errors.New("no such remote")
			}
			return origin + "\n", nil
		case strings.Contains(joined, "rev-parse --verify --quiet"):
			if localRef != "" && strings.Contains(joined, localRef+"^{commit}") {
				return testHeadSHA + "\n", nil
			}
			return "", errors.New("unknown revision")
		}
		return "", nil
	}}
}

func ghCalls(run *exec.FakeRunner) int {
	n := 0
	for _, c := range run.Calls {
		if c.Name == "gh" {
			n++
		}
	}
	return n
}

func TestWorkerRepoIdentity(t *testing.T) {
	ctx := context.Background()
	t.Run("github origin records GitHub's name and id", func(t *testing.T) {
		run := identityFake("git@github.com:CameronSjo/ForgeCtl.git", identityFound, nil, "")
		id, err := workerRepoIdentity(ctx, run, io.Discard, "/top", "worker/w1", true)
		if err != nil || id != (repoIdentity{NameWithOwner: "cameronsjo/forgectl", DatabaseID: 1252924951}) {
			t.Fatalf("identity %+v, %v", id, err)
		}
		var args []string
		for _, c := range run.Calls {
			if c.Name == "gh" {
				args = c.Args
			}
		}
		joined := strings.Join(args, " ")
		if !strings.HasPrefix(joined, "api graphql --hostname github.com") || !strings.Contains(joined, "owner=CameronSjo") || !strings.Contains(joined, "ref=refs/heads/worker/w1") {
			t.Fatalf("gh args %q", args)
		}
	})
	t.Run("a non-GitHub origin records nothing and asks GitHub nothing", func(t *testing.T) {
		for _, origin := range []string{"git@gitlab.com:o/r.git", ""} {
			run := identityFake(origin, identityFound, nil, "")
			if id, err := workerRepoIdentity(ctx, run, io.Discard, "/top", "worker/w1", false); err != nil || id != (repoIdentity{}) || ghCalls(run) != 0 {
				t.Fatalf("origin %q: %+v, %v, %d gh calls", origin, id, err, ghCalls(run))
			}
		}
	})
	failing := func() map[string]*exec.FakeRunner {
		return map[string]*exec.FakeRunner{
			"gh fails":      identityFake("git@github.com:o/r.git", "", errors.New("HTTP 502"), ""),
			"gh refused":    identityFake("git@github.com:o/r.git", "", errors.New("HTTP 404: Not Found"), ""),
			"bad response":  identityFake("git@github.com:o/r.git", `{"data":{"repository":null}}`, nil, ""),
			"graphql error": identityFake("git@github.com:o/r.git", `{"errors":[{"message":"nope"}]}`, nil, ""),
		}
	}
	t.Run("a transient GitHub failure pauses a drain launch; an answer counts an attempt", func(t *testing.T) {
		for name, run := range failing() {
			_, err := workerRepoIdentity(ctx, run, io.Discard, "/top", "worker/w1", true)
			if err == nil || !strings.Contains(err.Error(), "which a worker launch records") {
				t.Fatalf("%s: %v, want the identity read named", name, err)
			}
			want, wantErr := drain.ErrOther, errIdentityRefused
			if name == "gh fails" {
				want, wantErr = drain.ErrGitHubRead, errIdentityRead
			}
			if !errors.Is(err, wantErr) {
				t.Errorf("%s: %v, want %v", name, err, wantErr)
			}
			if got := classifyLaunchError(err); got != want {
				t.Errorf("%s: class %v, want %v", name, got, want)
			}
		}
	})
	t.Run("a GitHub read failure warns once and goes on for a launch by hand", func(t *testing.T) {
		for name, run := range failing() {
			var warn strings.Builder
			id, err := workerRepoIdentity(ctx, run, &warn, "/top", "worker/w1", false)
			if err != nil || id != (repoIdentity{}) {
				t.Errorf("%s: %+v, %v; want no identity and no error", name, id, err)
			}
			if w := warn.String(); strings.Count(w, "\n") != 1 || !strings.HasPrefix(w, "forgectl: warning: ") || !strings.Contains(w, "the merge policy never passes it") {
				t.Errorf("%s: warning %q; want one line", name, w)
			}
		}
	})
	t.Run("a drain branch that exists is refused", func(t *testing.T) {
		local := identityFake("git@github.com:o/r.git", identityFound, nil, "refs/heads/worker/w1")
		if _, err := workerRepoIdentity(ctx, local, io.Discard, "/top", "worker/w1", true); !errors.Is(err, worker.ErrBranchExists) || ghCalls(local) != 0 {
			t.Fatalf("local branch: %v, %d gh calls; want ErrBranchExists before any GitHub call", err, ghCalls(local))
		}
		tracking := identityFake("git@github.com:o/r.git", identityFound, nil, "refs/remotes/origin/worker/w1")
		if _, err := workerRepoIdentity(ctx, tracking, io.Discard, "/top", "worker/w1", true); !errors.Is(err, worker.ErrBranchExists) {
			t.Fatalf("origin/<branch>: %v, want ErrBranchExists", err)
		}
		remote := identityFake("git@github.com:o/r.git", identityHasRef, nil, "")
		_, err := workerRepoIdentity(ctx, remote, io.Discard, "/top", "worker/w1", true)
		if !errors.Is(err, worker.ErrBranchExists) || !strings.Contains(err.Error(), "exists on GitHub") {
			t.Fatalf("GitHub branch: %v, want ErrBranchExists naming GitHub", err)
		}
		for _, want := range []string{"git -C /top branch -D worker/w1", "git -C /top push origin --delete worker/w1", "enqueue under a new name"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("GitHub branch: %q does not name the way out %q", err, want)
			}
		}
	})
	t.Run("a CLI launch keeps an existing branch", func(t *testing.T) {
		run := identityFake("git@github.com:o/r.git", identityHasRef, nil, "refs/heads/worker/w1")
		if id, err := workerRepoIdentity(ctx, run, io.Discard, "/top", "worker/w1", false); err != nil || id.DatabaseID == 0 {
			t.Fatalf("CLI launch: %+v, %v", id, err)
		}
	})
}

// TestWorkerLaunchRecordsIdentityAndBranchFrom checks the row carries the
// identity and BranchFrom, and that a failed identity read, or a drain
// branch that exists, writes no row at all.
func TestWorkerLaunchRecordsIdentityAndBranchFrom(t *testing.T) {
	led := testWorkerLedger(t)
	if _, err := runWorkerSteps(context.Background(), led, "w1", "feat/w1", goodSteps(t)); err != nil {
		t.Fatal(err)
	}
	row := onlyRow(t, led)
	if row.GitHubRepo != "cameronsjo/forgectl" || row.GitHubRepoID != 1252924951 || row.BranchFrom != worker.BranchNew {
		t.Fatalf("row %+v; want the GitHub identity and branch_from new", row)
	}

	for name, idErr := range map[string]error{
		"identity read fails": errors.New("forgectl: read o/r's repository name and id from GitHub, which a worker launch records: HTTP 502"),
		"drain branch exists": worker.ErrBranchExists,
	} {
		t.Run(name, func(t *testing.T) {
			led := testWorkerLedger(t)
			steps := goodSteps(t)
			made := false
			steps.identify = func(context.Context) (repoIdentity, error) { return repoIdentity{}, idErr }
			steps.addWorktree = func(context.Context) (worker.Worktree, error) {
				made = true
				return worker.Worktree{}, nil
			}
			attempt, err := attemptWorker(context.Background(), led, "w1", "feat/w1", steps)
			if !errors.Is(err, idErr) || made || !attempt.createdNothing() {
				t.Fatalf("err %v, worktree made %v, created nothing %v", err, made, attempt.createdNothing())
			}
			if rows, err := led.Rows(); err != nil || len(rows) != 0 {
				t.Fatalf("rows %+v, %v; want none", rows, err)
			}
		})
	}

	steps := goodSteps(t)
	steps.identify = nil
	if _, err := runWorkerSteps(context.Background(), testWorkerLedger(t), "w1", "feat/w1", steps); !errors.Is(err, errNoIdentityStep) {
		t.Fatalf("no identity step: %v", err)
	}
}

func TestDrainSpecRefusesExistingBranch(t *testing.T) {
	if spec := drainSpec(worker.QueueRow{Name: "w1", Repo: "/r"}); !spec.drain {
		t.Fatal("the drain's spec does not mark a drain launch")
	}
	err := errors.Join(errors.New("context"), worker.ErrBranchExists)
	if got := classifyLaunchError(err); got != drain.ErrRowInvalid {
		t.Fatalf("class %v, want ErrRowInvalid (failed, no retry)", got)
	}
}
