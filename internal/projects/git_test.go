package projects

// Test plan for git.go
//
// gitStatus (Classification: business logic / subprocess boundary)
//   [x] Happy: non-repo dir (no .git) → TreeNotRepo, empty Label()
//   [x] Unhappy: .git present but `git status --porcelain` errors → TreeUnknown,
//       empty Label() — the headline regression this issue exists to pin: a
//       tree that was never successfully inspected must never read as clean
//   [x] Happy: .git present, clean status → TreeOK, Label()=="[clean]"

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
)

func TestGitStatus_NonRepoDir_IsTreeNotRepo(t *testing.T) {
	tmp := t.TempDir() // no .git

	gs := gitStatus(context.Background(), &exec.FakeRunner{}, tmp)

	if gs.State != TreeNotRepo {
		t.Errorf("State = %q, want %q", gs.State, TreeNotRepo)
	}
	if gs.Label() != "" {
		t.Errorf("Label() = %q, want empty for a non-repo dir", gs.Label())
	}
}

// TestGitStatus_StatusCallErrors_IsTreeUnknown pins the headline regression:
// a directory whose git status was never successfully read must surface as
// TreeUnknown (empty Label()), not fall through to reading as clean.
func TestGitStatus_StatusCallErrors_IsTreeUnknown(t *testing.T) {
	tmp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmp, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	fake := &exec.FakeRunner{
		RunFunc: func(name string, args []string) (string, error) {
			return "", errors.New("git: command failed")
		},
	}

	gs := gitStatus(context.Background(), fake, tmp)

	if gs.State != TreeUnknown {
		t.Errorf("State = %q, want %q", gs.State, TreeUnknown)
	}
	if gs.Label() != "" {
		t.Errorf("Label() = %q, want empty — an uninspected tree must never read as clean", gs.Label())
	}
}

func TestGitStatus_CleanRepo_IsTreeOK(t *testing.T) {
	tmp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmp, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	fake := &exec.FakeRunner{
		RunFunc: func(name string, args []string) (string, error) {
			// status --porcelain / rev-list → clean, 0 ahead.
			return "", nil
		},
	}

	gs := gitStatus(context.Background(), fake, tmp)

	if gs.State != TreeOK {
		t.Errorf("State = %q, want %q", gs.State, TreeOK)
	}
	if got, want := gs.Label(), "[clean]"; got != want {
		t.Errorf("Label() = %q, want %q", got, want)
	}
}

// TestGitStatus_DirtyTree_CountsModifiedAndUntracked exercises the porcelain
// line-classification loop directly: "??" lines are untracked, every other
// non-empty line (regardless of its actual XY code) counts as modified.
func TestGitStatus_DirtyTree_CountsModifiedAndUntracked(t *testing.T) {
	tmp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmp, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	porcelain := " M modified1.go\nA  staged-new.go\n?? untracked1.txt\n?? untracked2.txt\n"
	fake := &exec.FakeRunner{
		RunFunc: func(name string, args []string) (string, error) {
			return porcelain, nil
		},
	}

	gs := gitStatus(context.Background(), fake, tmp)

	if gs.State != TreeOK {
		t.Errorf("State = %q, want %q", gs.State, TreeOK)
	}
	if gs.Modified != 2 {
		t.Errorf("Modified = %d, want 2", gs.Modified)
	}
	if gs.Untracked != 2 {
		t.Errorf("Untracked = %d, want 2", gs.Untracked)
	}
	if gs.Ahead != 0 {
		t.Errorf("Ahead = %d, want 0 (rev-list is only consulted for a clean tree)", gs.Ahead)
	}
}

// TestGitStatus_DirtyTree_SkipsShortLines guards the `len(line) < 2` skip: a
// blank line from the trailing split (or any stray single-char line) must not
// be miscounted as either modified or untracked.
func TestGitStatus_DirtyTree_SkipsShortLines(t *testing.T) {
	tmp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmp, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Trailing newline produces a trailing "" element from strings.Split.
	porcelain := "?? only.txt\n"
	fake := &exec.FakeRunner{
		RunFunc: func(name string, args []string) (string, error) {
			return porcelain, nil
		},
	}

	gs := gitStatus(context.Background(), fake, tmp)

	if gs.Untracked != 1 {
		t.Errorf("Untracked = %d, want 1 (trailing blank line must not be counted)", gs.Untracked)
	}
	if gs.Modified != 0 {
		t.Errorf("Modified = %d, want 0", gs.Modified)
	}
}

// TestGitStatus_CleanTreeWithUnpushedCommits_SetsAhead pins the clean-tree
// branch's rev-list call: porcelain empty → gs.Ahead comes from parsing the
// rev-list output, not left at zero.
func TestGitStatus_CleanTreeWithUnpushedCommits_SetsAhead(t *testing.T) {
	tmp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmp, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	fake := &exec.FakeRunner{
		RunFunc: func(name string, args []string) (string, error) {
			for _, a := range args {
				if a == "rev-list" {
					return "4\n", nil
				}
			}
			return "", nil // status --porcelain → clean
		},
	}

	gs := gitStatus(context.Background(), fake, tmp)

	if gs.State != TreeOK {
		t.Errorf("State = %q, want %q", gs.State, TreeOK)
	}
	if gs.Ahead != 4 {
		t.Errorf("Ahead = %d, want 4", gs.Ahead)
	}
	if got, want := gs.Label(), "[4 ahead]"; got != want {
		t.Errorf("Label() = %q, want %q", got, want)
	}
}
