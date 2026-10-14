//go:build unix

package worker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	fexec "github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/gitenv/gitenvtest"
)

// TestInspectWorktreeRefusesAnUnreadableGitFile plants each .git close must
// refuse in a real worktree: a symbolic link to the real gitfile, and a
// gitfile past the size bound. Both are refused before git runs there.
func TestInspectWorktreeRefusesAnUnreadableGitFile(t *testing.T) {
	ctx := context.Background()
	run := fexec.OSRunner{}
	cases := map[string]func(t *testing.T, gitfile string){
		"a symlinked gitfile": func(t *testing.T, gitfile string) {
			moved := filepath.Join(t.TempDir(), "gitfile")
			data, err := os.ReadFile(gitfile) //nolint:gosec // G304: the test worktree's own gitfile
			if err != nil {
				t.Fatal(err)
			}
			writeFile(t, moved, string(data))
			if err := os.Remove(gitfile); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(moved, gitfile); err != nil {
				t.Fatal(err)
			}
		},
		"an oversized gitfile": func(t *testing.T, gitfile string) {
			data, err := os.ReadFile(gitfile) //nolint:gosec // G304: the test worktree's own gitfile
			if err != nil {
				t.Fatal(err)
			}
			// Still a valid gitfile once trimmed, so only the bound refuses it.
			writeFile(t, gitfile, string(data)+strings.Repeat("\n", maxGitFileBytes))
		},
	}
	for name, plant := range cases {
		t.Run(name, func(t *testing.T) {
			top := gitRepo(t)
			wt, err := AddWorktree(ctx, run, top, "w", "feat", baseOf(t, top))
			if err != nil {
				t.Fatal(err)
			}
			plant(t, filepath.Join(wt.Path, ".git"))
			if _, err := InspectWorktree(ctx, run, top, "w", wt.Base); !errors.Is(err, ErrUnsafeWorktreeRoot) {
				t.Fatalf("InspectWorktree with %s = %v, want ErrUnsafeWorktreeRoot", name, err)
			}
		})
	}
}

// readGitFile is the read behind checkGitFile's Lstat, and it refuses on its
// own what could be swapped in after that Lstat: a symbolic link, a FIFO
// (promptly, never blocking on it), a directory, and a file past the bound.
// Mutations: open with os.Open (the FIFO blocks, the symlink is followed);
// drop the LimitReader bound check (the oversized file is read).
func TestReadGitFileRefusesWhatASwapCouldPlant(t *testing.T) {
	valid := "gitdir: /r/.git/worktrees/w\n"
	cases := map[string]func(t *testing.T, path string){
		"symlink": func(t *testing.T, path string) {
			moved := filepath.Join(t.TempDir(), "gitfile")
			writeFile(t, moved, valid)
			if err := os.Symlink(moved, path); err != nil {
				t.Fatal(err)
			}
		},
		"fifo": func(t *testing.T, path string) {
			if err := unix.Mkfifo(path, 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"directory": func(t *testing.T, path string) {
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
		},
		"oversized": func(t *testing.T, path string) {
			writeFile(t, path, valid+strings.Repeat("x", maxGitFileBytes))
		},
	}
	for name, plant := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), ".git")
			plant(t, path)
			done := make(chan error, 1)
			go func() {
				_, err := readGitFile(path)
				done <- err
			}()
			select {
			case err := <-done:
				if !errors.Is(err, ErrUnsafeWorktreeRoot) {
					t.Fatalf("readGitFile over a %s = %v, want ErrUnsafeWorktreeRoot", name, err)
				}
			case <-time.After(10 * time.Second):
				t.Fatalf("readGitFile blocked on a %s", name)
			}
		})
	}

	t.Run("a gitfile removed after the check is an open error, not a hostile one", func(t *testing.T) {
		_, err := readGitFile(filepath.Join(t.TempDir(), ".git"))
		if err == nil || !errors.Is(err, os.ErrNotExist) || errors.Is(err, ErrUnsafeWorktreeRoot) {
			t.Fatalf("readGitFile on a missing file = %v, want a not-exist error that is not ErrUnsafeWorktreeRoot", err)
		}
	})

	t.Run("a gitfile within the bound is read", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), ".git")
		writeFile(t, path, valid)
		data, err := readGitFile(path)
		if err != nil || string(data) != valid {
			t.Fatalf("readGitFile = %q, %v; want %q", data, err, valid)
		}
	})
}

// TestWorkerGitRunsNoConfiguredHook covers the hooks git 2.54 lets
// configuration define, which core.hooksPath=/dev/null does not reach: a
// post-checkout hook in the repository's config does not run during
// AddWorktree, and a post-index-change hook the worker writes into the
// common config does not run during close's status (#1079). InspectWorktree
// runs status with --no-optional-locks, which writes no refreshed index, so
// the mutation that drops the per-name switch is caught by the gitenv and
// AddWorktree halves, not this one; this half guards the call shape. Each half has a
// control proving the hook fires under core.hooksPath=/dev/null alone.
func TestWorkerGitRunsNoConfiguredHook(t *testing.T) {
	gitenvtest.RequireGitVersion(t, 2, 54)
	ctx := context.Background()
	run := fexec.OSRunner{}
	top := gitRepo(t)
	marker := filepath.Join(t.TempDir(), "hook-ran")
	fired := func() bool {
		_, err := os.Stat(marker)
		if err == nil {
			if rmErr := os.Remove(marker); rmErr != nil {
				t.Fatal(rmErr)
			}
		}
		return err == nil
	}
	mustGit(t, top, "config", "hook.canary.command", "touch '"+marker+"'")
	mustGit(t, top, "config", "--add", "hook.canary.event", "post-checkout")

	mustGit(t, top, "-c", "core.hooksPath=/dev/null", "worktree", "add", "-q", "-b", "control", filepath.Join(t.TempDir(), "control"))
	if !fired() {
		t.Fatal("control: a configured post-checkout hook did not run under core.hooksPath=/dev/null")
	}
	wt, err := AddWorktree(ctx, run, top, "w", "feat", baseOf(t, top))
	if err != nil {
		t.Fatalf("AddWorktree: %v", err)
	}
	if fired() {
		t.Fatal("a configured post-checkout hook ran during AddWorktree")
	}

	writeFile(t, filepath.Join(wt.Path, "a.txt"), "x\n")
	mustGit(t, wt.Path, "-c", "core.hooksPath=/dev/null", "-c", "hook.canary.enabled=false", "add", "a.txt")
	mustGit(t, wt.Path, "-c", "core.hooksPath=/dev/null", "-c", "hook.canary.enabled=false", "commit", "-q", "-m", "a")
	mustGit(t, top, "config", "--replace-all", "hook.canary.event", "post-index-change")
	// A new mtime makes a.txt stat-dirty, so a status refreshes the index.
	stale := func(d time.Duration) {
		later := time.Now().Add(d)
		if err := os.Chtimes(filepath.Join(wt.Path, "a.txt"), later, later); err != nil {
			t.Fatal(err)
		}
	}
	stale(time.Hour)
	mustGit(t, wt.Path, "-c", "core.hooksPath=/dev/null", "status", "--porcelain")
	if !fired() {
		t.Fatal("control: a configured post-index-change hook did not run under core.hooksPath=/dev/null")
	}
	stale(2 * time.Hour)
	if _, err := InspectWorktree(ctx, run, top, "w", wt.Base); err != nil {
		t.Fatalf("InspectWorktree: %v", err)
	}
	if fired() {
		t.Fatal("a configured hook ran during close's git status")
	}
}
