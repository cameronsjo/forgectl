//go:build !unix

package resume

import "time"

func processGone(int, time.Duration) bool { return true }
