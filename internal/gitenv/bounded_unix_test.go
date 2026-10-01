//go:build unix

package gitenv_test

import (
	"context"
	"errors"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/gitenv"
)

// boundedHelperEnv names the scratch directory of a re-executed test binary
// playing forgectl mid-status; unset, TestBoundedHelper is a no-op.
const boundedHelperEnv = "FORGECTL_TEST_BOUNDED_HELPER_DIR"

// stuckGitScript is a "git" that answers RunUnfiltered's two listings as a
// repository with no driver and no submodule does, and on the call itself
// records its own pid (its group's id) and a helper's, marks its start, and
// waits on the helper, as a git blocked on a FIFO would wait for good.
const stuckGitScript = `#!/bin/sh
dir=$(dirname "$0")
case "$*" in
*--get-regexp*) exit 1 ;;
*ls-files*) exit 0 ;;
esac
sleep 300 &
echo "$$ $!" > "$dir/pids"
: > "$dir/started"
wait
`

// TestBoundedHelper is the re-executed forgectl: it runs a status through
// RunUnfiltered against stuckGitScript and exits 0 if the call returns.
func TestBoundedHelper(t *testing.T) {
	dir := os.Getenv(boundedHelperEnv)
	if dir == "" {
		return
	}
	_, _ = gitenv.RunUnfiltered(context.Background(), exec.OSRunner{}, filepath.Join(dir, "git"), dir, "status")
	os.Exit(0)
}

// Review of #1008: a git in its own process group no longer receives the
// terminal's Ctrl-C, which reaches the foreground group, forgectl, only. A
// SIGINT sent to forgectl alone must still end the git and its helpers, and
// then forgectl itself, by that signal, as when they shared a group.
// Mutations: make interruptible return ctx unchanged (forgectl dies, but
// the git and its sleep run on with init as their parent); drop the
// re-raise in its stop (the git dies, but forgectl exits 0 and would carry
// on to the next repository).
func TestBoundedPassesCtrlCToTheGitsGroup(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(stuckGitScript), 0o700); err != nil { //nolint:gosec // G306: an executable stub
		t.Fatal(err)
	}
	cmd := osexec.Command(os.Args[0], "-test.run=^TestBoundedHelper$") //nolint:gosec // G204: the test binary itself
	cmd.Env = append(os.Environ(), boundedHelperEnv+"="+dir)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()

	started := filepath.Join(dir, "started")
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if _, err := os.Stat(started); err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			t.Fatal("the stub git never started")
		}
	}
	raw, err := os.ReadFile(filepath.Clean(filepath.Join(dir, "pids")))
	if err != nil {
		t.Fatal(err)
	}
	var pids []int
	for _, f := range strings.Fields(string(raw)) {
		pid, err := strconv.Atoi(f)
		if err != nil {
			t.Fatalf("pids %q: %v", raw, err)
		}
		pids = append(pids, pid)
	}
	t.Cleanup(func() {
		for _, pid := range pids {
			_ = unix.Kill(pid, unix.SIGKILL)
		}
	})

	if err := unix.Kill(cmd.Process.Pid, unix.SIGINT); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-waited:
		var ee *osexec.ExitError
		ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
		if !errors.As(err, &ee) || !ok || !ws.Signaled() || ws.Signal() != syscall.SIGINT {
			t.Errorf("forgectl ended with %v, want death by SIGINT", err)
		}
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("forgectl did not stop on SIGINT")
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		alive := false
		for _, pid := range pids {
			alive = alive || running(pid)
		}
		if !alive {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the git or its helper (pids %v) outlived forgectl's SIGINT", pids)
		}
	}
}
