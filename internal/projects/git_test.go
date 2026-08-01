package projects

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
)

// TestGitStatus_NonRepoDir_StateIsNotRepo pins the first of the three
// collapsed states: a directory with no .git must report StatusNotRepo, not
// the zero-value-shaped "clean". A sibling dir that DOES have .git, wired to
// the same FakeRunner, is the control — the only variable across the two
// subtests is .git's presence, so a pass here isolates the .git check itself.
func TestGitStatus_NonRepoDir_StateIsNotRepo(t *testing.T) {
	tmp := t.TempDir()
	nonRepo := filepath.Join(tmp, "notarepo")
	if err := os.MkdirAll(nonRepo, 0o755); err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(tmp, "realrepo")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	fake := &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		return "", nil // clean status, 0 ahead
	}}

	if got := gitStatus(context.Background(), fake, nonRepo); got.State != StatusNotRepo {
		t.Errorf("non-repo dir: State = %q, want %q", got.State, StatusNotRepo)
	}
	// Control: same runner, a dir that IS a repo must report StatusOK.
	if got := gitStatus(context.Background(), fake, repo); got.State != StatusOK {
		t.Errorf("control repo dir: State = %q, want %q", got.State, StatusOK)
	}
}

// TestGitStatus_CommandError_StateIsUnknown pins the third collapsed state —
// the one no existing test covered — a `git status` failure (corrupt repo,
// permissions, git missing) must report StatusUnknown, explicitly not
// StatusOK, so a failed check is never mistaken for a clean tree.
func TestGitStatus_CommandError_StateIsUnknown(t *testing.T) {
	tmp := t.TempDir()
	repo := filepath.Join(tmp, "brokenrepo")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	failing := &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		return "", errors.New("fatal: unable to read current working directory")
	}}
	if got := gitStatus(context.Background(), failing, repo); got.State != StatusUnknown {
		t.Errorf("failing git status: State = %q, want %q", got.State, StatusUnknown)
	}

	// Control: identical fixture, RunFunc succeeds → StatusOK.
	succeeding := &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		return "", nil
	}}
	if got := gitStatus(context.Background(), succeeding, repo); got.State != StatusOK {
		t.Errorf("control succeeding git status: State = %q, want %q", got.State, StatusOK)
	}
}
