//go:build unix

package branch

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/gitenv"
	"github.com/cameronsjo/forgectl/internal/gitenv/gitenvtest"
)

// TestMergedLocallyNeverLazyFetches is TestGitStatusNeverLazyFetches
// (internal/projects) for the local merge check, which runs git in the
// working directory and walks history from the default branch
// (cameronsjo/forgectl#944). The for-each-ref listing reads refs only, and
// does not fire the canary even unhardened (measured on git 2.43).
//
// Mutation: run `git branch --merged` in mergedLocally through c.run.Run:
// the canary runs.
func TestMergedLocallyNeverLazyFetches(t *testing.T) {
	gitenvtest.WithoutLazyFetchPin(t)
	gitenvtest.NewCanary(t).AssertLive(t)
	canary := gitenvtest.NewCanary(t)
	t.Chdir(canary.Dir)

	_, err := New(exec.OSRunner{}).mergedLocally(context.Background(), "main")
	if canary.Ran(t) {
		t.Fatal("the local merge check ran the repository's transport command through a lazy fetch")
	}
	if err == nil {
		t.Error("mergedLocally succeeded with a default branch naming a missing commit")
	}
}

// worktreeFilterCanary is a repository at dir with a worktree at wt on
// branch "w", whose own config.worktree defines a clean filter that creates
// canary, on a stat-dirty x.txt that .gitattributes routes through it. The
// main worktree's config defines no driver.
func worktreeFilterCanary(t *testing.T) (dir, wt, canary string) {
	t.Helper()
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("the canary's driver is an sh command")
	}
	root := t.TempDir()
	dir, wt, canary = filepath.Join(root, "repo"), filepath.Join(root, "wt"), filepath.Join(root, "canary")
	if err := os.Mkdir(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"x.txt": "payload\n", ".gitattributes": "x.txt filter=f\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	gitenvtest.Git(t, dir, "init", "-q", "-b", "main")
	gitenvtest.Git(t, dir, "add", ".")
	gitenvtest.Git(t, dir, "commit", "-q", "-m", "control")
	gitenvtest.Git(t, dir, "worktree", "add", "-q", "-b", "w", "--", wt)
	gitenvtest.Git(t, dir, "config", "extensions.worktreeConfig", "true")
	gitenvtest.Git(t, wt, "config", "--worktree", "filter.f.clean", "touch '"+canary+"'; cat")
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(filepath.Join(wt, "x.txt"), later, later); err != nil {
		t.Fatal(err)
	}
	return dir, wt, canary
}

func fired(t *testing.T, canary string) bool {
	t.Helper()
	_, err := os.Lstat(canary)
	if errors.Is(err, fs.ErrNotExist) {
		return false
	}
	if err != nil {
		t.Fatal(err)
	}
	return true
}

// TestDeleteLocalRemovesAWorktreeRunningNoFilterDriver: prune's worktree
// remove runs status in the worktree it removes, which re-hashes a
// stat-dirty file through the filter driver the worktree's own
// config.worktree defines. deleteLocal blanks it, and the remove and the
// branch delete both still succeed (#977).
// Mutation: run the remove through gitenv.Run under Local, or through
// RunUnfiltered without the worktree in also (the main worktree's listing
// cannot see config.worktree): the canary runs.
func TestDeleteLocalRemovesAWorktreeRunningNoFilterDriver(t *testing.T) {
	liveDir, liveWt, liveCanary := worktreeFilterCanary(t)
	cmd := gitenv.Command(t.Context(), gitenv.Local, "-C", liveDir, "worktree", "remove", "--", liveWt)
	_ = cmd.Run()
	if !fired(t, liveCanary) {
		t.Fatal("a Local worktree remove ran no filter driver; the fixture cannot fire, so a passing test would prove nothing")
	}

	dir, wt, canary := worktreeFilterCanary(t)
	t.Chdir(dir)
	err := New(exec.OSRunner{}).deleteLocal(context.Background(), Info{Name: "w", LocalExists: true, MergedOnServer: true, WorktreePath: wt})
	if fired(t, canary) {
		t.Fatal("prune's worktree remove ran a filter driver the worktree's config defines")
	}
	if err != nil {
		t.Fatalf("deleteLocal: %v", err)
	}
	if _, err := os.Lstat(wt); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the worktree %s is still there: %v", wt, err)
	}
}

// #1005: prune refuses, cleanly and at once, to remove a worktree whose
// repository's HEAD is a FIFO, on which git's dirty check would block. The
// branch stays: neither the remove nor the branch delete runs.
// Mutation: drop gitenv's HEAD mode check: the remove and the branch delete
// both run.
func TestDeleteLocalRefusesAWorktreeWithAFIFOHead(t *testing.T) {
	gitDir := filepath.Join(gitenvtest.FIFOHeadRepo(t), ".git")
	wt := t.TempDir()
	if err := os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: "+gitDir+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fake := &exec.FakeRunner{RunFunc: gitenvtest.NoFilters(func(string, []string) (string, error) { return "", nil })}
	err := New(fake).deleteLocal(context.Background(), Info{Name: "w", LocalExists: true, MergedOnServer: true, WorktreePath: wt})
	if err == nil {
		t.Error("deleteLocal removed a worktree whose HEAD is a FIFO")
	}
	if len(fake.Calls) != 0 {
		t.Errorf("git ran %d time(s): %+v", len(fake.Calls), fake.Calls)
	}
}
