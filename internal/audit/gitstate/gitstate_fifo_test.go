//go:build unix && !aix && !illumos && !solaris

package gitstate

import (
	"context"
	"errors"
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

// fifoGitignoreRepo is a working tree whose .gitignore is a FIFO, which
// makes `ls-files --exclude-standard` wait forever for a writer.
func fifoGitignoreRepo(t *testing.T) string {
	t.Helper()
	repo := liveRepo(t)
	if err := syscall.Mkfifo(filepath.Join(repo, ".gitignore"), 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".env"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	return repo
}

// callWithin runs status over repo and fails the test past limit.
func callWithin(t *testing.T, status audit.GitStatusFunc, repo string, limit time.Duration) (map[string]audit.GitState, error) {
	t.Helper()
	type result struct {
		m   map[string]audit.GitState
		err error
	}
	done := make(chan result, 1)
	go func() {
		m, err := status(repo, []string{".env"})
		done <- result{m, err}
	}()
	select {
	case r := <-done:
		return r.m, r.err
	case <-time.After(limit):
		if f, err := os.OpenFile(filepath.Clean(filepath.Join(repo, ".gitignore")), os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			_ = f.Close()
		}
		t.Fatalf("git status over a FIFO .gitignore ran past %s", limit)
		return nil, nil
	}
}

// TestFunc_PerRepoSliceEndsAFIFOHang: with budget to spare, the per-repo
// cap ends the hung call with git's error, not ErrBudgetExhausted.
//
// Mutation that turns it red: drop the per-repo WithTimeout in Func.
func TestFunc_PerRepoSliceEndsAFIFOHang(t *testing.T) {
	repo := fifoGitignoreRepo(t)
	_, err := callWithin(t, Func(context.Background(), fexec.OSRunner{}, 300*time.Millisecond), repo, 20*time.Second)
	if err == nil || errors.Is(err, audit.ErrBudgetExhausted) {
		t.Errorf("err = %v, want git's own failure", err)
	}
}

// TestFunc_BudgetCutsTheSlice: the slice is min(per-repo cap, what is left
// of the budget), and a call the budget's deadline ends reads as
// ErrBudgetExhausted.
func TestFunc_BudgetCutsTheSlice(t *testing.T) {
	repo := fifoGitignoreRepo(t)
	budget, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := callWithin(t, Func(budget, fexec.OSRunner{}, time.Hour), repo, 20*time.Second)
	if !errors.Is(err, audit.ErrBudgetExhausted) {
		t.Errorf("err = %v, want ErrBudgetExhausted", err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("took %s; the budget did not cut the hour-long slice", d)
	}
}
