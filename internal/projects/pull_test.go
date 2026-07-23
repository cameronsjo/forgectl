package projects

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
)

// pullFixture wires a *Client whose git calls are keyed by the full repo dir:
// porcelain controls `git status --porcelain` (empty = clean), pullOut/pullErr
// control `git pull --rebase`'s outcome. Any dir absent from a map gets the
// zero value (clean / "" / nil), and `rev-list --count` always answers "0"
// unless overridden via ahead.
func pullFixture(porcelain, pullOut map[string]string, pullErr map[string]error, ahead map[string]string) *exec.FakeRunner {
	return &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		if name != "git" || len(args) < 3 || args[0] != "-C" {
			return "", nil
		}
		dir := args[1]
		switch args[2] {
		case "status":
			return porcelain[dir], nil
		case "rev-list":
			if a, ok := ahead[dir]; ok {
				return a, nil
			}
			return "0", nil
		case "pull":
			return pullOut[dir], pullErr[dir]
		}
		return "", nil
	}}
}

func TestPullAll_ClassifiesEachRepo(t *testing.T) {
	tmp := t.TempDir()
	mkGitDir(t, tmp, "uptodate")
	mkGitDir(t, tmp, "updated")
	mkGitDir(t, tmp, "dirtymod")
	mkGitDir(t, tmp, "dirtyuntracked")
	mkGitDir(t, tmp, "failpull")
	mkGitDir(t, tmp, "aheadonly")

	uptodateDir := filepath.Join(tmp, "uptodate")
	updatedDir := filepath.Join(tmp, "updated")
	dirtyModDir := filepath.Join(tmp, "dirtymod")
	dirtyUntrackedDir := filepath.Join(tmp, "dirtyuntracked")
	failDir := filepath.Join(tmp, "failpull")
	aheadDir := filepath.Join(tmp, "aheadonly")

	porcelain := map[string]string{
		dirtyModDir:       " M file.go",
		dirtyUntrackedDir: "?? newfile.go",
	}
	pullOut := map[string]string{
		uptodateDir: "Already up to date.",
		updatedDir:  "Fast-forward\n file.go | 1 +",
		aheadDir:    "Already up to date.",
	}
	pullErr := map[string]error{
		failDir: errors.New("conflict"),
	}
	ahead := map[string]string{
		aheadDir: "2",
	}

	fake := pullFixture(porcelain, pullOut, pullErr, ahead)
	c := &Client{Dir: tmp, run: fake}

	results, err := c.PullAll(context.Background(), "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	byName := make(map[string]PullResult, len(results))
	for _, r := range results {
		byName[r.Name] = r
	}

	if got := byName["uptodate"].Status; got != PullUpToDate {
		t.Errorf("uptodate status = %v, want PullUpToDate", got)
	}
	if got := byName["updated"].Status; got != PullUpdated {
		t.Errorf("updated status = %v, want PullUpdated", got)
	}
	if got := byName["dirtymod"].Status; got != PullSkippedDirty {
		t.Errorf("dirtymod status = %v, want PullSkippedDirty", got)
	}
	if got := byName["dirtyuntracked"].Status; got != PullSkippedDirty {
		t.Errorf("dirtyuntracked status = %v, want PullSkippedDirty", got)
	}
	if got := byName["failpull"].Status; got != PullFailed {
		t.Errorf("failpull status = %v, want PullFailed", got)
	}
	if byName["failpull"].Err == nil {
		t.Error("failpull result should carry the pull error")
	}
	// Ahead-only is NOT dirty — a rebase pull handles unpushed local commits,
	// so it must run (and here reports up to date on the remote side).
	if got := byName["aheadonly"].Status; got != PullUpToDate {
		t.Errorf("aheadonly status = %v, want PullUpToDate (ahead alone must not skip)", got)
	}

	// No pull call was recorded for either dirty repo.
	for _, call := range fake.Calls {
		if call.Name != "git" || len(call.Args) < 3 || call.Args[2] != "pull" {
			continue
		}
		dir := call.Args[1]
		if dir == dirtyModDir || dir == dirtyUntrackedDir {
			t.Errorf("pull ran for a dirty repo: %v", call.Args)
		}
	}
}

// TestPullAll_SkipsNonGitDir guards the discoverDir non-git-inclusion trap:
// discoverDir returns plain non-git directories (list/pick display them) with
// the same zero GitStatus as a clean repo, so PullAll must isGitRepo-gate them
// rather than shelling `git pull` into a non-repo. The fake ERRORS on any pull
// into the scratch dir, so a regression that pulls it surfaces as a real
// PullFailed here — not the fixture's rosy default-success.
func TestPullAll_SkipsNonGitDir(t *testing.T) {
	tmp := t.TempDir()
	mkGitDir(t, tmp, "realrepo")
	scratch := filepath.Join(tmp, "scratch")
	if err := os.MkdirAll(scratch, 0o755); err != nil {
		t.Fatal(err)
	}

	fake := pullFixture(nil, nil, map[string]error{scratch: errors.New("fatal: not a git repository")}, nil)
	c := &Client{Dir: tmp, run: fake}

	results, err := c.PullAll(context.Background(), "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// The non-git dir must be skipped entirely — not pulled, not in results.
	if len(results) != 1 || results[0].Name != "realrepo" {
		t.Fatalf("PullAll = %+v, want only 'realrepo' (the non-git scratch dir must be skipped)", results)
	}
	for _, call := range fake.Calls {
		if call.Name == "git" && len(call.Args) >= 3 && call.Args[2] == "pull" && call.Args[1] == scratch {
			t.Errorf("pull ran for a non-git dir: %v", call.Args)
		}
	}
}

func TestPullAll_DirOverride_WalksOnlyThatSubtree(t *testing.T) {
	tmp := t.TempDir()
	mkGitDir(t, tmp, "outside")
	sub := filepath.Join(tmp, "sub")
	mkGitDir(t, sub, "inside")

	fake := pullFixture(nil, nil, nil, nil)
	c := &Client{Dir: tmp, run: fake}

	results, err := c.PullAll(context.Background(), sub)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 1 || results[0].Name != "inside" {
		t.Fatalf("PullAll(dir=%q) = %+v, want only 'inside'", sub, results)
	}
}

func TestPullAll_MissingDir_PropagatesDiscoverError(t *testing.T) {
	c := &Client{Dir: "/nonexistent/does/not/exist", run: &exec.FakeRunner{}}
	if _, err := c.PullAll(context.Background(), ""); err == nil {
		t.Fatal("expected an error for a missing projects dir, got nil")
	}
}

func TestClassifyPull_Table(t *testing.T) {
	cases := []struct {
		name string
		out  string
		err  error
		want PullStatus
	}{
		{"error wins regardless of output", "Already up to date.", errors.New("boom"), PullFailed},
		{"empty output means up to date", "", nil, PullUpToDate},
		{"explicit up-to-date message", "Already up to date.", nil, PullUpToDate},
		{"case-insensitive match", "ALREADY UP TO DATE.", nil, PullUpToDate},
		{"anything else means updated", "Fast-forward\n file | 1 +", nil, PullUpdated},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyPull(tc.out, tc.err); got != tc.want {
				t.Errorf("classifyPull(%q, %v) = %v, want %v", tc.out, tc.err, got, tc.want)
			}
		})
	}
}
