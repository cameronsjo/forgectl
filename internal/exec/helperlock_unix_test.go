//go:build unix

package exec

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// heldPidFile keeps the helper's locked pid file open for the life of the
// process: an *os.File that became garbage would be closed by its finalizer,
// and the close would drop the lock.
var heldPidFile *os.File

// recordHelperPid records this helper's pid in dir as a file named for it,
// holding an exclusive flock on it until the process exits. The file is
// locked under a temporary name and renamed into place, so no reader ever
// sees the pid file before its lock is held; the temporary name does not
// parse as a pid, so killHelpers skips it.
func recordHelperPid(dir string) {
	pid := strconv.Itoa(os.Getpid())
	tmp := filepath.Join(dir, ".lock-"+pid)
	f, err := os.OpenFile(filepath.Clean(tmp), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil { //nolint:gosec // G115: a file descriptor fits an int
		_ = f.Close()
		return
	}
	if err := os.Rename(tmp, filepath.Join(dir, pid)); err != nil {
		_ = f.Close()
		return
	}
	heldPidFile = f
}

// helperLockHeld reports whether some process holds the lock on the pid file
// at path, which only the live helper that wrote it can: the lock dies with
// that helper, and a process that merely reuses its pid never takes it. A
// file it cannot open or try-lock reads as not held, so cleanup never
// signals a pid it cannot vouch for.
func helperLockHeld(path string) bool {
	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) //nolint:gosec // G115: a file descriptor fits an int
	if err == nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) //nolint:gosec // G115: a file descriptor fits an int
		return false
	}
	return errors.Is(err, syscall.EWOULDBLOCK)
}

// TestKillHelpers_KillsOnlyLiveHelpers: cleanup kills a helper that still
// holds its pid file's lock, and leaves alone a live process whose pid file
// nobody locks — the shape of a helper that exited and whose pid the OS then
// handed to an unrelated process (#1009).
//
// Mutations: make helperLockHeld return true and the impostor is killed;
// drop the Flock from recordHelperPid and the helper survives cleanup.
func TestKillHelpers_KillsOnlyLiveHelpers(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	pidDir := t.TempDir()
	start := func(env ...string) (*exec.Cmd, <-chan struct{}) {
		cmd := exec.CommandContext(t.Context(), self) //nolint:gosec // G204: re-runs this test binary in helper mode
		cmd.Env = append(os.Environ(), env...)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		t.Cleanup(func() { _ = cmd.Process.Kill(); <-done })
		return cmd, done
	}
	helper, helperDone := start(helperModeEnv+"=sleep:60s", helperPidDirEnv+"="+pidDir)
	impostor, impostorDone := start(helperModeEnv + "=sleep:60s")
	impostorFile := filepath.Join(pidDir, strconv.Itoa(impostor.Process.Pid))
	if err := os.WriteFile(filepath.Clean(impostorFile), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	helperFile := filepath.Join(pidDir, strconv.Itoa(helper.Process.Pid))
	for deadline := time.Now().Add(10 * time.Second); ; {
		if _, err := os.Stat(filepath.Clean(helperFile)); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the helper never recorded its pid")
		}
		time.Sleep(10 * time.Millisecond)
	}

	killHelpers(pidDir)

	select {
	case <-helperDone:
	case <-time.After(10 * time.Second):
		t.Error("a helper holding its lock survived cleanup")
	}
	select {
	case <-impostorDone:
		t.Error("cleanup killed a process whose pid file no helper locks")
	case <-time.After(500 * time.Millisecond):
	}
}
