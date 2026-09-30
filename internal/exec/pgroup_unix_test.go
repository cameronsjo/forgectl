//go:build unix

package exec

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestWithProcessGroupKillsDescendants: a timed-out child that forked a
// helper leaves no helper running when the run opted into its own group.
func TestWithProcessGroupKillsDescendants(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	ctx, cancel := context.WithTimeout(WithProcessGroup(t.Context()), 300*time.Millisecond)
	defer cancel()
	// The helper's output goes to /dev/null so it holds none of our pipes.
	_, err := OSRunner{}.Run(ctx, "/bin/sh", "-c", `sleep 30 >/dev/null 2>&1 & echo $! > "$0"; wait`, pidFile)
	if err == nil {
		t.Fatal("want the timeout to fail the run")
	}
	data, rerr := os.ReadFile(pidFile) // #nosec G304 -- test temp dir
	if rerr != nil {
		t.Fatal(rerr)
	}
	pid, perr := strconv.Atoi(strings.TrimSpace(string(data)))
	if perr != nil {
		t.Fatal(perr)
	}
	deadline := time.Now().Add(2 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL) // clean up the survivor before failing
			t.Fatalf("helper pid %d survived the group kill", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A deadline that fires after the group has already exited must not read as
// a cancellation: Cancel reports os.ErrProcessDone, which os/exec ignores.
func TestProcessGroupCancelAfterExitIsProcessDone(t *testing.T) {
	cmd := exec.CommandContext(context.Background(), "/bin/sh", "-c", "exit 0")
	setProcessGroup(cmd)
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Cancel(); !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("Cancel after exit = %v, want os.ErrProcessDone", err)
	}
}
