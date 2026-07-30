//go:build !unix

package resume

import "os"

// processAlive reports whether pid names a running process. Off unix there is
// no signal-0 probe, and os.FindProcess never fails, so this degrades to a
// pid-shape check: every registry entry reads as live, which makes the
// live-session refusal conservative rather than wrong.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	_, err := os.FindProcess(pid)
	return err == nil
}
