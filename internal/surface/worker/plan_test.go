package worker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	fexec "github.com/cameronsjo/forgectl/internal/exec"
)

// repoState is everything a plan must leave alone: the worktree list, the
// branch list, and whether .claude exists.
func repoState(t *testing.T, top string) string {
	t.Helper()
	_, claudeErr := os.Lstat(filepath.Join(top, ".claude"))
	return mustGit(t, top, "worktree", "list", "--porcelain") + "|" +
		mustGit(t, top, "branch", "--list", "--all") + "|claude-absent=" + map[bool]string{true: "y", false: "n"}[errors.Is(claudeErr, os.ErrNotExist)]
}

// PlanWorktree says what AddWorktree would do and writes nothing: no worktree
// root, no worktree, no branch (forgectl#1088).
func TestPlanWorktreeWritesNothing(t *testing.T) {
	top := gitRepo(t)
	before := repoState(t, top)

	plan, err := PlanWorktree(context.Background(), fexec.OSRunner{}, top, "w1", "feat/w1")
	if err != nil {
		t.Fatalf("PlanWorktree: %v", err)
	}
	if plan.Path != WorktreePath(top, "w1") || plan.Branch != "feat/w1" || plan.BranchFrom != BranchNew {
		t.Errorf("plan = %+v, want a new branch at %s", plan, WorktreePath(top, "w1"))
	}
	if after := repoState(t, top); after != before {
		t.Errorf("PlanWorktree changed the repo:\nbefore %s\nafter  %s", before, after)
	}
}

// The branch source is the one AddWorktree picks: local first, then origin,
// then new.
func TestPlanWorktreeNamesWhereTheBranchComesFrom(t *testing.T) {
	top := gitRepo(t)
	mustGit(t, top, "branch", "local-one")
	mustGit(t, top, "update-ref", "refs/remotes/origin/remote-one", "HEAD")
	mustGit(t, top, "branch", "both")
	mustGit(t, top, "update-ref", "refs/remotes/origin/both", "HEAD")

	for branch, want := range map[string]string{
		"local-one":  BranchLocal,
		"remote-one": BranchOrigin,
		"both":       BranchLocal,
		"fresh":      BranchNew,
	} {
		plan, err := PlanWorktree(context.Background(), fexec.OSRunner{}, top, "w", branch)
		if err != nil {
			t.Fatalf("PlanWorktree(%s): %v", branch, err)
		}
		if plan.BranchFrom != want {
			t.Errorf("branch %s: BranchFrom = %q, want %q", branch, plan.BranchFrom, want)
		}
	}
}

// A preview refuses what the launch refuses, with the same error: bad names
// and branches, an existing path, a symlinked worktree root.
func TestPlanWorktreeRefusesWhatAddWorktreeRefuses(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		desc, name, branch string
		setup              func(t *testing.T, top string)
		want               error
	}{
		{"bad name", "../escape", "feat/x", nil, ErrInvalidName},
		{"uppercase name", "Upper", "feat/x", nil, ErrInvalidName},
		{"dash branch", "ok", "-b", nil, ErrInvalidBranch},
		{"dotdot branch", "ok", "feat/../x", nil, ErrInvalidBranch},
		{"control branch", "ok", "feat\x1b[31m", nil, ErrInvalidBranch},
		{"empty branch", "ok", "", nil, ErrInvalidBranch},
		{"symlinked .claude", "ok", "feat/x", func(t *testing.T, top string) {
			if err := os.Symlink(t.TempDir(), filepath.Join(top, ".claude")); err != nil {
				t.Fatal(err)
			}
		}, ErrUnsafeWorktreeRoot},
		{"path exists", "taken", "feat/x", func(t *testing.T, top string) {
			if err := os.MkdirAll(WorktreePath(top, "taken"), 0o750); err != nil {
				t.Fatal(err)
			}
		}, nil},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			top := gitRepo(t)
			if tc.setup != nil {
				tc.setup(t, top)
			}
			_, planErr := PlanWorktree(ctx, fexec.OSRunner{}, top, tc.name, tc.branch)
			_, addErr := AddWorktree(ctx, fexec.OSRunner{}, top, tc.name, tc.branch, baseOf(t, top))
			if planErr == nil || addErr == nil {
				t.Fatalf("plan err = %v, add err = %v; both must refuse", planErr, addErr)
			}
			if tc.want != nil && (!errors.Is(planErr, tc.want) || !errors.Is(addErr, tc.want)) {
				t.Errorf("plan err = %v, add err = %v, want both %v", planErr, addErr, tc.want)
			}
			if !strings.Contains(planErr.Error(), strings.SplitN(addErr.Error(), ":", 2)[0]) {
				t.Errorf("plan err %q and add err %q differ in kind", planErr, addErr)
			}
		})
	}
}

// A plan that passes is a launch that passes: the real call then succeeds and
// lands where the plan said.
func TestPlanWorktreeThenAddAgree(t *testing.T) {
	top := gitRepo(t)
	plan, err := PlanWorktree(context.Background(), fexec.OSRunner{}, top, "w1", "feat/w1")
	if err != nil {
		t.Fatal(err)
	}
	wt, err := AddWorktree(context.Background(), fexec.OSRunner{}, top, "w1", "feat/w1", baseOf(t, top))
	if err != nil {
		t.Fatalf("AddWorktree after a clean plan: %v", err)
	}
	if wt.Path != plan.Path || wt.Branch != plan.Branch {
		t.Errorf("AddWorktree = %+v, plan said %+v", wt, plan)
	}
}
