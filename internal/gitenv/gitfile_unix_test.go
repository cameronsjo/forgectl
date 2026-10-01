//go:build unix

package gitenv_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/gitenv"
	"github.com/cameronsjo/forgectl/internal/gitenv/gitenvtest"
)

// specialGit makes dir/.git a special file: a FIFO, which a blocking read
// waits on for a writer that never comes, or /dev/zero's character device,
// which a read never finishes. It skips t when the system refuses one.
func specialGit(t *testing.T, dir, kind string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, ".git")
	switch kind {
	case "fifo":
		if err := unix.Mkfifo(p, 0o600); err != nil {
			t.Skipf("mkfifo: %v", err)
		}
	case "chardev":
		if err := mknodZero(p); err != nil {
			t.Skipf("mknod /dev/zero's device: %v", err)
		}
	}
}

// runPromptly runs RunUnfiltered in dir against fake and fails t if it does
// not return within the deadline, rather than hanging the suite.
func runPromptly(t *testing.T, dir string, gitlinks map[string]string) error {
	t.Helper()
	f := listingFake(nil, gitlinks)
	done := make(chan error, 1)
	go func() {
		_, err := gitenv.RunUnfiltered(t.Context(), f, gitenv.Bin, dir, "status")
		done <- err
	}()
	select {
	case err := <-done:
		for _, c := range f.Calls {
			if !gitenvtest.FilterListing(c.Args) {
				t.Errorf("RunUnfiltered ran %v", c.Args)
			}
		}
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("RunUnfiltered did not return: it is reading a .git that never ends")
		return nil
	}
}

// A .git that is a FIFO or a character device, at the starting repository
// or in a submodule, is refused at once: git's own read_gitfile refuses
// anything but a regular file, and reading one could block forever (a FIFO)
// or never end (/dev/zero).
// Mutations: drop both regular-file checks, the Lstat's and the open
// handle's (the error is no longer the not-regular refusal); drop the
// Lstat's check and O_NONBLOCK together (the FIFO open blocks, and the
// deadline fails the test); ignore every gitDirKey error at the starting
// repositories (the root rows run the status).
func TestRunUnfilteredRefusesASpecialGitFile(t *testing.T) {
	for _, kind := range []string{"fifo", "chardev"} {
		t.Run(kind+"/root", func(t *testing.T) {
			root := t.TempDir()
			specialGit(t, root, kind)
			if err := runPromptly(t, root, nil); !errors.Is(err, gitenv.ErrGitfileNotRegular) {
				t.Errorf("err = %v, want the not-a-regular-file refusal", err)
			}
		})
		t.Run(kind+"/submodule", func(t *testing.T) {
			root := t.TempDir()
			specialGit(t, filepath.Join(root, "s"), kind)
			if err := runPromptly(t, root, map[string]string{root: gitlink("s")}); !errors.Is(err, gitenv.ErrGitfileNotRegular) {
				t.Errorf("err = %v, want the not-a-regular-file refusal", err)
			}
		})
	}
}

// A gitfile larger than any path is refused without reading past the bound.
// Mutation: drop the length check (the oversized gitfile's path is used,
// and fails later as no repository; the error is then the same refusal, so
// this pins only that the call is refused).
func TestRunUnfilteredRefusesAnOversizedGitfile(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "s")
	if err := os.MkdirAll(sub, 0o750); err != nil {
		t.Fatal(err)
	}
	body := "gitdir: " + strings.Repeat("a/", 40<<10) + "\n"
	if err := os.WriteFile(filepath.Join(sub, ".git"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runPromptly(t, root, map[string]string{root: gitlink("s")}); err == nil {
		t.Error("RunUnfiltered ran the call through an oversized gitfile")
	}
}

// returnsPromptly runs fn and fails t if it does not return within limit,
// rather than hanging the suite on a git that blocks.
func returnsPromptly(t *testing.T, limit time.Duration, fn func() error) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- fn() }()
	select {
	case err := <-done:
		return err
	case <-time.After(limit):
		t.Fatalf("the call did not return within %s: git is blocked on a file it reads", limit)
		return nil
	}
}

// #1005 item 1, the cheap check: a repository whose HEAD is a FIFO is
// refused before any git runs, at the starting repository and at a working
// tree RunUnfilteredAlso also reaches. Real git would block on it until the
// deadline.
// Mutations: drop the HEAD mode check in gitDirKey, or ignore
// errHeadNotRegular at the starting repositories: git runs and blocks, and
// the 10-second bound (a third of the deadline) fails the test.
func TestRunUnfilteredRefusesAFIFOHead(t *testing.T) {
	t.Run("root", func(t *testing.T) {
		dir := gitenvtest.FIFOHeadRepo(t)
		err := returnsPromptly(t, 10*time.Second, func() error {
			_, err := gitenv.RunUnfiltered(t.Context(), exec.OSRunner{}, gitenv.Bin, dir, "status", "--porcelain")
			return err
		})
		if !errors.Is(err, gitenv.ErrHeadNotRegular) {
			t.Errorf("err = %v, want the HEAD refusal", err)
		}
	})
	t.Run("also", func(t *testing.T) {
		dir := gitenvtest.FIFOHeadRepo(t)
		f := listingFake(nil, nil)
		err := returnsPromptly(t, 10*time.Second, func() error {
			_, err := gitenv.RunUnfilteredAlso(t.Context(), f, gitenv.Bin, "", []string{dir}, "worktree", "remove", "--", dir)
			return err
		})
		if !errors.Is(err, gitenv.ErrHeadNotRegular) {
			t.Errorf("err = %v, want the HEAD refusal", err)
		}
		if len(f.Calls) != 0 {
			t.Errorf("git ran %d time(s) before the refusal: %v", len(f.Calls), f.Calls)
		}
	})
}

// A symbolic-link HEAD, the legacy symref form, is still accepted.
// Mutation: require a regular file (the symlink row is refused).
func TestRunUnfilteredAcceptsASymlinkHead(t *testing.T) {
	gitenvtest.RequireGit(t)
	dir := t.TempDir()
	gitenvtest.Git(t, dir, "init", "-q", "-b", "main")
	gitenvtest.Git(t, dir, "-c", "user.name=t", "-c", "user.email=t@example.test", "commit", "-q", "--allow-empty", "-m", "c")
	head := filepath.Join(dir, ".git", "HEAD")
	if err := os.Remove(head); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("refs/heads/main", head); err != nil {
		t.Fatal(err)
	}
	if _, err := gitenv.RunUnfiltered(t.Context(), exec.OSRunner{}, gitenv.Bin, dir, "status", "--porcelain"); err != nil {
		t.Errorf("status in a repository with a symlink HEAD: %v", err)
	}
}

// #1005 item 1, the deadline: a FIFO the HEAD check cannot see, a loose ref
// git resolves HEAD through, blocks git status for good (measured on git
// 2.43). The deadline ends the call with its own error.
// Mutation: run runUnfilteredAlso under ctx rather than the deadline's
// context in RunUnfilteredAlso: the call blocks and the bound fails the test.
func TestRunUnfilteredDeadlineEndsAGitThatBlocks(t *testing.T) {
	gitenvtest.RequireGit(t)
	gitenv.SetUnfilteredDeadline(t, time.Second)
	dir := t.TempDir()
	gitenvtest.Git(t, dir, "init", "-q", "-b", "main")
	gitenvtest.Git(t, dir, "-c", "user.name=t", "-c", "user.email=t@example.test", "commit", "-q", "--allow-empty", "-m", "c")
	gitenvtest.Git(t, dir, "pack-refs", "--all")
	if err := unix.Mkfifo(filepath.Join(dir, ".git", "refs", "heads", "main"), 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	err := returnsPromptly(t, 10*time.Second, func() error {
		_, err := gitenv.RunUnfiltered(t.Context(), exec.OSRunner{}, gitenv.Bin, dir, "status", "--porcelain")
		return err
	})
	if !errors.Is(err, gitenv.ErrUnfilteredDeadline) {
		t.Errorf("err = %v, want the deadline's error", err)
	}
}

// The deadline kills each git's whole process group, so a helper a git
// started dies with it rather than running on. The "git" here answers the
// listings, then starts a long sleep, records its pid and waits on it.
// Mutation: drop fexec.WithProcessGroup in RunUnfilteredAlso: only the
// direct child is killed, and the sleep is still running.
func TestRunUnfilteredDeadlineKillsTheProcessGroup(t *testing.T) {
	gitenv.SetUnfilteredDeadline(t, time.Second)
	pidFile := filepath.Join(t.TempDir(), "pid")
	bin := filepath.Join(t.TempDir(), "git")
	script := "#!/bin/sh\ncase \"$*\" in\n*--get-regexp*) exit 1 ;;\n*ls-files*) exit 0 ;;\nesac\nsleep 300 &\necho $! > '" + pidFile + "'\nwait\n"
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil { //nolint:gosec // G306: an executable stub
		t.Fatal(err)
	}
	err := returnsPromptly(t, 10*time.Second, func() error {
		_, err := gitenv.RunUnfiltered(t.Context(), exec.OSRunner{}, bin, t.TempDir(), "status")
		return err
	})
	if !errors.Is(err, gitenv.ErrUnfilteredDeadline) {
		t.Errorf("err = %v, want the deadline's error", err)
	}
	raw, rerr := os.ReadFile(filepath.Clean(pidFile))
	if rerr != nil {
		t.Fatalf("the stub recorded no helper pid: %v", rerr)
	}
	pid, perr := strconv.Atoi(strings.TrimSpace(string(raw)))
	if perr != nil {
		t.Fatalf("pid %q: %v", raw, perr)
	}
	deadline := time.Now().Add(5 * time.Second)
	for running(pid) {
		if time.Now().After(deadline) {
			_ = unix.Kill(pid, unix.SIGKILL)
			t.Fatal("the helper the git started is still running after the deadline")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// running reports whether pid is a live process: one that exists and, where
// /proc says so, is not a zombie waiting to be reaped.
func running(pid int) bool {
	if unix.Kill(pid, 0) != nil {
		return false
	}
	stat, err := os.ReadFile(filepath.Clean(fmt.Sprintf("/proc/%d/stat", pid)))
	if err != nil {
		return true
	}
	_, after, ok := strings.Cut(string(stat), ") ")
	return !ok || !strings.HasPrefix(after, "Z")
}
