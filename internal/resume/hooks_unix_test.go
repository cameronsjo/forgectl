//go:build unix

package resume

import (
	"syscall"
	"time"
)

// processGone waits up to d for pid to disappear, killing it if it does not.
func processGone(pid int, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL) // clean up the survivor
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
	return true
}
