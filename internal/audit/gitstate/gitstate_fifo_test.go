//go:build unix && !aix && !illumos && !solaris

package gitstate

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/audit"
	fexec "github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/gitenv"
)

// liveRepo makes a git working tree for the FIFO tests.
func liveRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	repo := t.TempDir()
	cmd := exec.Command("git", "-C", repo, "init", "-q") //nolint:gosec,noctx // G204: test setup running git with fixed arguments
	cmd.Env = gitenv.Env(gitenv.Local, os.Environ())
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	return repo
}

// TestStatus_UntrackedFIFOIsUnknown: git lists a FIFO in no listing, so an
// untracked FIFO named .env.local comes back absent (unknown), not ignored.
func TestStatus_UntrackedFIFOIsUnknown(t *testing.T) {
	repo := liveRepo(t)
	if err := syscall.Mkfifo(filepath.Join(repo, ".env.local"), 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	got, err := Status(context.Background(), fexec.OSRunner{}, repo, []string{".env.local"})
	if err != nil {
		t.Fatal(err)
	}
	if st, ok := got[".env.local"]; ok {
		t.Errorf(".env.local = %v, want absent: git never listed it", st)
	}
}

// TestFuncWithTimeout_FIFOGitignoreReturns: a FIFO .gitignore makes
// `ls-files --exclude-standard` wait forever for a writer. The per-repo
// deadline must end the call with an error, promptly.
//
// Mutation that turns it red: call Status with the caller's ctx instead of
// the deadline's (the test then hangs to its own 30s watchdog).
func TestFuncWithTimeout_FIFOGitignoreReturns(t *testing.T) {
	repo := liveRepo(t)
	if err := syscall.Mkfifo(filepath.Join(repo, ".gitignore"), 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".env"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	status := FuncWithTimeout(context.Background(), fexec.OSRunner{}, 300*time.Millisecond)
	type result struct {
		m   map[string]audit.GitState
		err error
	}
	done := make(chan result, 1)
	start := time.Now()
	go func() {
		m, err := status(repo, []string{".env"})
		done <- result{m, err}
	}()
	select {
	case r := <-done:
		if r.err == nil {
			t.Errorf("status = %v with no error; a hung git must read as unanswered", r.m)
		}
		if d := time.Since(start); d > 10*time.Second {
			t.Errorf("returned after %s; the deadline did not bound it", d)
		}
	case <-time.After(30 * time.Second):
		// Unblock the stuck reader so git can exit.
		if f, err := os.OpenFile(filepath.Clean(filepath.Join(repo, ".gitignore")), os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			_ = f.Close()
		}
		t.Fatal("git status over a FIFO .gitignore hung past the per-repo deadline")
	}
}
