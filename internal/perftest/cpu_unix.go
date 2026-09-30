//go:build unix

package perftest

import (
	"syscall"
	"time"
)

// cpuTime is the CPU time this process has used, user and system, and
// whether it could be read.
func cpuTime() (time.Duration, bool) {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0, false
	}
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano()), true
}
