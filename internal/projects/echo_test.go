package projects

// Test plan for the #658 echo convention in internal/projects: a failed gh,
// git, or tea call is reported categorically. The subprocess's stderr (text
// the server or transport chooses), a server-supplied clone URL, and a
// remote-derived branch name never reach the error text, and the
// CommandError stays on the unwrap chain for errors.Is/As dispositions.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/githubauth"
)

// subprocessFailure is a failed call as the real runner reports it. Its
// Error() renders argv and stderr, so both carry markers.
func subprocessFailure(name string, args []string) error {
	return &exec.CommandError{Name: name, Args: args, Stderr: "remote: STDERRMARKER\x1b[2J‮", ExitCode: 128, Err: errors.New("exit status 128")}
}

func failingRunner() *exec.FakeRunner {
	return &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		return "", subprocessFailure(name, args)
	}}
}

// assertCategorical: err exists, shows none of the markers, and keeps the
// CommandError reachable.
func assertCategorical(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatal("want an error")
	}
	msg := err.Error()
	for _, s := range []string{"STDERRMARKER", "SECRETTOK", "BRANCHMARKER", "\x1b", "‮"} {
		if strings.Contains(msg, s) {
			t.Fatalf("error %q echoes %q", msg, s)
		}
	}
	if !strings.Contains(msg, want) {
		t.Fatalf("error %q, want it to say %q", msg, want)
	}
	var cmdErr *exec.CommandError
	if !errors.As(err, &cmdErr) {
		t.Fatalf("error %v lost the CommandError from its chain", err)
	}
}

func TestEcho_GitHubSubprocessFailuresAreCategorical(t *testing.T) {
	ctx := context.Background()
	_, err := githubListOrg(ctx, failingRunner(), "cameronsjo", githubauth.DefaultHost)
	assertCategorical(t, err, "gh repo list failed")

	err = cloneRepo(ctx, failingRunner(), "cameronsjo/forgectl", t.TempDir(), githubauth.DefaultHost)
	assertCategorical(t, err, "gh repo clone failed")

	err = cloneBareRepo(ctx, failingRunner(), "cameronsjo/forgectl", t.TempDir(), githubauth.DefaultHost)
	assertCategorical(t, err, "gh repo clone --bare failed")
}

// TestEcho_ServerSuppliedCloneURLIsNotEchoed: the clone URL comes from the
// repo-list output, and an https form can carry a token.
func TestEcho_ServerSuppliedCloneURLIsNotEchoed(t *testing.T) {
	ctx := context.Background()
	url := "https://SECRETTOK@git.example.test/o/r.git"
	assertCategorical(t, cloneFromGitea(ctx, failingRunner(), url, t.TempDir()), "git clone failed")
	assertCategorical(t, cloneBareFromURL(ctx, failingRunner(), url, t.TempDir()), "git clone --bare failed")
}

// worktreeFailAt is worktreeRunFunc with one step failing as a real
// subprocess would, and a remote-reported default branch.
func worktreeFailAt(step, headBranch string) func(string, []string) (string, error) {
	ok := worktreeRunFunc(headBranch, false)
	return func(name string, args []string) (string, error) {
		if strings.Contains(strings.Join(args, " "), step) {
			return "", subprocessFailure(name, args)
		}
		return ok(name, args)
	}
}

func TestEcho_WorktreeSubprocessFailuresAreCategorical(t *testing.T) {
	r := Repo{Host: "github.com", Owner: "cameronsjo", Name: "forgectl"}

	c := &Client{Dir: t.TempDir(), run: &exec.FakeRunner{RunFunc: worktreeFailAt("fetch origin", "main")}, gitBin: "git"}
	_, err := c.Worktree(context.Background(), r, "")
	assertCategorical(t, err, "git fetch origin failed")
	// The bare dir is forgectl-composed, so it is named, and it is the one
	// locating detail left once git's text is withheld.
	if !strings.Contains(err.Error(), ".bare") {
		t.Errorf("error %q, want it to name the bare dir", err)
	}

	// The branch is what the REMOTE reported as HEAD, so it is not echoed
	// when either worktree add fails.
	c = &Client{Dir: t.TempDir(), run: &exec.FakeRunner{RunFunc: worktreeFailAt("worktree add", "BRANCHMARKER\x1b[2J")}, gitBin: "git"}
	_, err = c.Worktree(context.Background(), r, "")
	assertCategorical(t, err, "git worktree add failed")
	// The bare dir is forgectl-composed, so it is named, and it is the one
	// locating detail left once git's text is withheld.
	if !strings.Contains(err.Error(), ".bare") {
		t.Errorf("error %q, want it to name the bare dir", err)
	}
}

func TestEcho_UnsafeRemoteDefaultBranchIsNotEchoed(t *testing.T) {
	c := &Client{Dir: t.TempDir(), run: &exec.FakeRunner{RunFunc: worktreeRunFunc("-BRANCHMARKER\x1b[2J", false)}, gitBin: "git"}
	_, err := c.Worktree(context.Background(), Repo{Host: "github.com", Owner: "cameronsjo", Name: "forgectl"}, "")
	if err == nil {
		t.Fatal("want a refusal for a flag-leading remote branch")
	}
	if strings.Contains(err.Error(), "BRANCHMARKER") || !strings.Contains(err.Error(), "unsafe branch name") {
		t.Fatalf("error %q, want the categorical unsafe-branch refusal", err)
	}
}
