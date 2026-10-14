//go:build unix

package gitleaks

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

// scanHelperEnv names the scratch directory of a re-executed test binary
// playing forgectl mid-scan; unset, TestScanSignalHelper is a no-op.
const scanHelperEnv = "FORGECTL_TEST_GITLEAKS_SIGNAL_DIR"

// stuckScanner is a "gitleaks" that records its own pid (its group's id) and
// a helper's, marks its start, and waits on the helper for good.
const stuckScanner = `#!/bin/sh
dir=$(dirname "$0")
sleep 300 &
echo "$$ $!" > "$dir/pids"
: > "$dir/started"
wait
`

// TestScanSignalHelper is the re-executed forgectl: it scans one repo with
// stuckScanner and exits 0 if the scan returns.
func TestScanSignalHelper(t *testing.T) {
	dir := os.Getenv(scanHelperEnv)
	if dir == "" {
		return
	}
	_ = Scan(context.Background(), fexec.OSRunner{}, filepath.Join(dir, "gitleaks"), []string{dir}, nil, time.Minute)
	os.Exit(0)
}

// #1020: audit secrets starts gitleaks in a process group of its own, so the
// terminal's Ctrl-C reaches forgectl alone. A SIGINT sent to forgectl must
// still end the scanner and its helpers, and then forgectl by that signal.
// Mutation: start the scan under fexec.WithProcessGroup without the relay
// (the pre-fix code); forgectl dies, the scanner runs on.
func TestScanPassesCtrlCToTheScannersGroup(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "gitleaks"), []byte(stuckScanner), 0o700); err != nil { //nolint:gosec // G306: an executable stub
		t.Fatal(err)
	}
	cmd := osexec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestScanSignalHelper$") //nolint:gosec // G204: the test binary itself
	cmd.Env = append(os.Environ(), scanHelperEnv+"="+dir)
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
			t.Fatal("the stub gitleaks never started")
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
			t.Fatalf("the scanner or its helper (pids %v) outlived forgectl's SIGINT", pids)
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
