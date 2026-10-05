//go:build unix

package gitenvtest

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// FIFOHeadRepo makes a real repository with one commit in a new directory
// and replaces its HEAD with a FIFO, on which git itself blocks for good
// (measured on git 2.43: `git status` there never returns), as a FIFO
// planted from an archive would (cameronsjo/forgectl#1005). It skips t when
// git is not on PATH or the system refuses the FIFO.
func FIFOHeadRepo(t testing.TB) string {
	t.Helper()
	RequireGit(t)
	dir := t.TempDir()
	Git(t, dir, "init", "-q", "-b", "main")
	Git(t, dir, "-c", "user.name=t", "-c", "user.email=t@example.test", "commit", "-q", "--allow-empty", "-m", "c")
	head := filepath.Join(dir, ".git", "HEAD")
	if err := os.Remove(head); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(head, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	return dir
}
