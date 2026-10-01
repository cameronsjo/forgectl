//go:build unix

package gitenv_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

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
