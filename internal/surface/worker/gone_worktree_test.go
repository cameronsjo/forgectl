package worker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	fexec "github.com/cameronsjo/forgectl/internal/exec"
)

func TestTopOfWorktreePath(t *testing.T) {
	for path, want := range map[string]string{
		"/r/.claude/worktrees/w":                      "/r",
		"/Users/a/The Compendium/.claude/worktrees/w": "/Users/a/The Compendium",
		"/r/.claude/worktrees/w/":                     "/r",
		"/r/.claude/worktrees/w/sub":                  "",
		"/r/.claude/other/w":                          "",
		"/r/worktrees/w":                              "",
		"r/.claude/worktrees/w":                       "",
		"/r/.claude/worktrees/..":                     "",
		"/r/.claude/worktrees/bad name":               "",
		"/r/.claude/worktrees/../worktrees/w":         "/r",
		"/.claude/worktrees/w":                        "/",
		"/r/x/../.claude/worktrees/w":                 "/r",
	} {
		got, ok := TopOfWorktreePath(path)
		if ok != (want != "") || got != want {
			t.Errorf("TopOfWorktreePath(%q) = %q, %v; want %q", path, got, ok, want)
		}
	}
}

// TestInspectWorktreeInARepoWithASpace is forgectl#1456's close: the repo
// path held a space, and close kept a worktree git "does not list". A space
// changes nothing: git lists the worktree, and close's lookup finds it. What
// git did not list was a leftover directory with no .git, the shape a
// harness writing .claude files into its old cwd after removal leaves, and
// once that is gone the worktree reads as absent.
func TestInspectWorktreeInARepoWithASpace(t *testing.T) {
	ctx := context.Background()
	run := fexec.OSRunner{}
	gitRepo(t) // the git environment; the repo itself is made below
	dir := filepath.Join(t.TempDir(), "The Compendium")
	if err := os.Mkdir(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	mustGit(t, dir, "init", "-q", "-b", "main")
	mustGit(t, dir, "commit", "-q", "--allow-empty", "-m", "init")
	top, err := RepoTop(ctx, run, dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(top, " ") {
		t.Fatalf("top %q lost its space", top)
	}
	wt, err := AddWorktree(ctx, run, top, "w", "feat", baseOf(t, top))
	if err != nil {
		t.Fatalf("AddWorktree: %v", err)
	}
	f, err := InspectWorktree(ctx, run, top, "w", wt.Base)
	if err != nil {
		t.Fatal(err)
	}
	if !f.Listed || !f.OnDisk || len(RemovalBlockers(f)) != 0 {
		t.Fatalf("a fresh worktree under a path with a space: facts %+v, blockers %q", f, RemovalBlockers(f))
	}
	if got, ok := TopOfWorktreePath(wt.Path); !ok || got != top {
		t.Fatalf("TopOfWorktreePath(%q) = %q, %v; want %q", wt.Path, got, ok, top)
	}

	if err := RemoveWorktree(ctx, run, top, f.Path); err != nil {
		t.Fatalf("RemoveWorktree: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(wt.Path, ".claude"), 0o750); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(wt.Path, ".claude", "settings.local.json"), `{"disabledMcpjsonServers":["x"]}`)
	f, err = InspectWorktree(ctx, run, top, "w", wt.Base)
	if err != nil {
		t.Fatal(err)
	}
	if b := RemovalBlockers(f); f.Listed || !f.OnDisk || len(b) != 1 || !strings.Contains(b[0], "does not list it") {
		t.Fatalf("a leftover directory with no .git: facts %+v, blockers %q", f, b)
	}

	if err := os.RemoveAll(wt.Path); err != nil {
		t.Fatal(err)
	}
	if f, err = InspectWorktree(ctx, run, top, "w", wt.Base); err != nil || f.Present() {
		t.Fatalf("a removed worktree: facts %+v, err %v; want not present", f, err)
	}
}
