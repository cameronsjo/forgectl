//go:build unix

package cli

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	fexec "github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/surface/backend"
	"github.com/cameronsjo/forgectl/internal/surface/worker"
)

// spacedRepo makes a git repo at <tmp>/The Compendium, the path shape
// forgectl#1456 hit, and returns its top as RepoTop resolves it.
func spacedRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	dir := filepath.Join(t.TempDir(), "The Compendium")
	if err := os.Mkdir(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "--allow-empty", "-m", "init"}} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	top, err := worker.RepoTop(context.Background(), fexec.OSRunner{}, dir)
	if err != nil {
		t.Fatal(err)
	}
	return top
}

// TestOpenRepoLedgerFollowsAGoneWorktree is forgectl#1456: the chief of
// staff names a worker's repo by the worker's worktree path, so once close
// removed the worktree every later close or forget failed to resolve --repo.
// A missing worker worktree path now opens its repository's ledger, with a
// note; any other missing path keeps the resolver's error.
func TestOpenRepoLedgerFollowsAGoneWorktree(t *testing.T) {
	top := spacedRepo(t)
	deps := module.Deps{Runner: fexec.OSRunner{}}
	ctx := context.Background()

	var warn bytes.Buffer
	gone := worker.WorktreePath(top, "cos-vault-subreddits")
	_, got, err := openRepoLedger(ctx, &warn, deps, gone, "hc:default")
	if err != nil || got != top {
		t.Fatalf("openRepoLedger(%q) = %q, %v; want %q", gone, got, err, top)
	}
	if !strings.Contains(warn.String(), "no longer exists") {
		t.Fatalf("no note naming the fallback: %q", warn.String())
	}

	warn.Reset()
	if _, got, err := openRepoLedger(ctx, &warn, deps, top, "hc:default"); err != nil || got != top || warn.Len() != 0 {
		t.Fatalf("the repo itself: top %q, err %v, warn %q; want %q and no note", got, err, warn.String(), top)
	}

	for name, repo := range map[string]string{
		"a missing path of another shape":              filepath.Join(top, "nope", "w"),
		"a gone worktree under a subfolder, not a top": worker.WorktreePath(filepath.Join(top, "sub"), "w"),
	} {
		t.Run(name, func(t *testing.T) {
			if name == "a gone worktree under a subfolder, not a top" {
				if err := os.Mkdir(filepath.Join(top, "sub"), 0o750); err != nil {
					t.Fatal(err)
				}
			}
			var warn bytes.Buffer
			if _, got, err := openRepoLedger(ctx, &warn, deps, repo, "hc:default"); err == nil || ExitCode(err) != exitUsage {
				t.Fatalf("openRepoLedger(%q) = %q, %v; want a usage error", repo, got, err)
			}
		})
	}
}

// TestForgetClearsAClosedRowOnceItsWorktreeIsGone drives forget's closed-row
// path through the real git inspection, in a repo whose path holds a space:
// a worktree still listed keeps the row, and a gone one lets it go.
func TestForgetClearsAClosedRowOnceItsWorktreeIsGone(t *testing.T) {
	top := spacedRepo(t)
	ctx := context.Background()
	run := fexec.OSRunner{}
	row := launchedHcRow(t)
	row.Name, row.Stage = "w", worker.StageClosed

	wt, err := worker.AddWorktree(ctx, run, top, row.Name, "feat", func() (string, error) {
		out, err := run.Run(ctx, "git", "-C", top, "rev-parse", "HEAD")
		return strings.TrimSpace(out), err
	})
	if err != nil {
		t.Fatalf("AddWorktree: %v", err)
	}
	removed := false
	steps := forgetSteps{
		abandoned: func(context.Context, backend.Ref, string, bool) (string, error) {
			t.Error("hc was asked about a closed row")
			return "", nil
		},
		worktreeGone: func(ctx context.Context) (bool, error) {
			f, err := worker.InspectWorktree(ctx, run, top, row.Name, wt.Base)
			return !f.Present(), err
		},
		remove: func() error { removed = true; return nil },
	}

	if res := forgetWorker(ctx, row, row.StartedAt, false, steps); res.Forgotten || removed {
		t.Fatalf("a closed row whose worktree is still there was forgotten: %+v", res)
	}
	if err := worker.RemoveWorktree(ctx, run, top, wt.Path); err != nil {
		t.Fatal(err)
	}
	if res := forgetWorker(ctx, row, row.StartedAt, false, steps); !res.Forgotten || !removed {
		t.Fatalf("a closed row whose worktree is gone was kept: %+v", res)
	}
}
