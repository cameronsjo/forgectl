package worker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// git refuses a second worktree for a branch another worktree has checked out.
// The preview sees that case (it reads the worktree list) and refuses before
// the launch would, and AddWorktree refuses it too, so the preview is not
// optimistic about it.
func TestPlanWorktreeRefusesABranchCheckedOutElsewhere(t *testing.T) {
	ctx := context.Background()
	for name, setup := range map[string]func(t *testing.T, top string) string{
		"the main checkout's own branch": func(*testing.T, string) string { return "main" },
		"another worktree's branch": func(t *testing.T, top string) string {
			mustGit(t, top, "branch", "busy")
			mustGit(t, top, "worktree", "add", "-q", filepath.Join(t.TempDir(), "other"), "busy")
			return "busy"
		},
	} {
		t.Run(name, func(t *testing.T) {
			top := gitRepo(t)
			branch := setup(t, top)
			before := repoState(t, top)

			_, planErr := PlanWorktree(ctx, fexec.OSRunner{}, top, "w1", branch)
			if !errors.Is(planErr, ErrBranchCheckedOut) {
				t.Fatalf("PlanWorktree err = %v, want ErrBranchCheckedOut", planErr)
			}
			if after := repoState(t, top); after != before {
				t.Errorf("the refused preview changed the repo")
			}
			if _, addErr := AddWorktree(ctx, fexec.OSRunner{}, top, "w1", branch, baseOf(t, top)); addErr == nil {
				t.Error("AddWorktree accepted a branch git refuses; the preview's refusal has no real counterpart")
			}
		})
	}
}

// A branch that exists but is checked out nowhere is fine.
func TestPlanWorktreeAcceptsAFreeLocalBranch(t *testing.T) {
	top := gitRepo(t)
	mustGit(t, top, "branch", "free")
	plan, err := PlanWorktree(context.Background(), fexec.OSRunner{}, top, "w1", "free")
	if err != nil || plan.BranchFrom != BranchLocal {
		t.Fatalf("plan = %+v err = %v, want a local branch", plan, err)
	}
}

// Inspecting a worktree must not rewrite its index: `surface close --dry-run`
// rests on it, and a preview changes nothing. A plain `git status` refreshes a
// stat-dirty entry and writes the index back; that is the control, so the
// assertion below can see the difference. InspectWorktree runs status with
// --no-optional-locks and leaves the index bytes alone.
func TestInspectWorktreeDoesNotRewriteTheIndex(t *testing.T) {
	ctx := context.Background()
	run := fexec.OSRunner{}
	top := gitRepo(t)
	wt, err := AddWorktree(ctx, run, top, "w", "feat", baseOf(t, top))
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(wt.Path, "a.txt")
	if err := os.WriteFile(file, []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mustGit(t, wt.Path, "add", "a.txt")
	mustGit(t, wt.Path, "commit", "-q", "-m", "a")
	indexPath := strings.TrimSpace(mustGit(t, wt.Path, "rev-parse", "--path-format=absolute", "--git-path", "index"))
	// Each call moves the file's mtime somewhere new: an index already
	// refreshed to the same mtime has nothing left to rewrite, and the test
	// would see no difference whether or not status takes locks.
	hours := 0
	staleStat := func() {
		hours++
		later := time.Now().Add(time.Duration(hours) * time.Hour)
		if err := os.Chtimes(file, later, later); err != nil {
			t.Fatal(err)
		}
	}
	readIndex := func() string {
		b, err := os.ReadFile(indexPath) //nolint:gosec // G304: the index path git reported for a test worktree
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}

	staleStat()
	before := readIndex()
	mustGit(t, wt.Path, "status", "--porcelain")
	if readIndex() == before {
		t.Skip("this git does not rewrite a stat-dirty index on status; the control cannot see the difference")
	}

	staleStat()
	before = readIndex()
	if _, err := InspectWorktree(ctx, run, top, "w", wt.Base); err != nil {
		t.Fatalf("InspectWorktree: %v", err)
	}
	if readIndex() != before {
		t.Error("InspectWorktree rewrote the worktree's index")
	}
}
