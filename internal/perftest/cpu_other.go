//go:build !unix

package perftest

import "time"

// cpuTime is unavailable off unix; timed falls back to the wall clock.
func cpuTime() (time.Duration, bool) { return 0, false }
