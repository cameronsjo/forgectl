//go:build unix

package gitstate

import (
	"context"
	"errors"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	fexec "github.com/cameronsjo/forgectl/internal/exec"
)

// stubGit runs bin wherever the code under test names git.
type stubGit struct{ bin string }

func (s stubGit) RunWithEnvFiltered(ctx context.Context, env map[string]string, unset []string, _ string, args ...string) (string, error) {
	return fexec.OSRunner{}.RunWithEnvFiltered(ctx, env, unset, s.bin, args...)
}

// statusHelperEnv names the scratch directory of a re-executed test binary
// playing forgectl mid-status; unset, TestStatusSignalHelper is a no-op.
const statusHelperEnv = "FORGECTL_TEST_GITSTATE_SIGNAL_DIR"

// stuckGit is a "git" that records its own pid (its group's id) and
// a helper's, marks its start, and waits on the helper for good.
const stuckGit = `#!/bin/sh
dir=$(dirname "$0")
sleep 300 &
echo "$$ $!" > "$dir/pids"
: > "$dir/started"
wait
`

// TestStatusSignalHelper is the re-executed forgectl: it asks one repo's
// status of stuckGit and exits 0 if the call returns.
func TestStatusSignalHelper(t *testing.T) {
	dir := os.Getenv(statusHelperEnv)
	if dir == "" {
		return
	}
	_, _ = Func(context.Background(), stubGit{filepath.Join(dir, "git")}, time.Minute)(dir, []string{".env"})
	os.Exit(0)
}

// #1020: audit secrets starts each repo's git status in a process group of
// its own, so the terminal's Ctrl-C reaches forgectl alone. A SIGINT sent to
// forgectl must still end the git and its helpers, and then forgectl by that
// signal. Mutation: start the status under fexec.WithProcessGroup without
// the relay (the pre-fix code); forgectl dies, the git runs on.
func TestStatusPassesCtrlCToTheGitsGroup(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(stuckGit), 0o700); err != nil { //nolint:gosec // G306: an executable stub
		t.Fatal(err)
	}
	cmd := osexec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestStatusSignalHelper$") //nolint:gosec // G204: the test binary itself
	cmd.Env = append(os.Environ(), statusHelperEnv+"="+dir)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()

	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if _, err := os.Stat(filepath.Join(dir, "started")); err == nil {
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
