//go:build unix

package resume

import (
	"errors"
	"syscall"
)

// processAlive reports whether pid names a running process.
//
// Signal 0 performs the permission and existence checks without delivering
// anything. EPERM means the process exists but belongs to someone else —
// alive, not absent — so only ESRCH (and a nonsense pid) count as dead.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	if err == nil {
		return true
	}
	return errors.Is(err, syscall.EPERM)
}

// terminateProcess sends SIGTERM. SIGTERM, not SIGINT: the two were measured
// equivalent on Claude Code 2.1.285 (both exit at once, remove the registry
// file, and keep the transcript), and SIGTERM is the conventional "stop now".
func terminateProcess(pid int) error {
	if pid <= 0 {
		return errors.New("refusing to signal a non-positive pid")
	}
	return syscall.Kill(pid, syscall.SIGTERM)
}
