//go:build unix && !darwin && !linux

package desk

import (
	"os"
	"time"
)

// procStart is unavailable here; liveness falls back to signal 0 alone, which
// cannot tell a reused pid from the original.
func procStart(int) (int64, bool, error) { return 0, false, errProcStartUnsupported }

func fileBirth(*os.File) (time.Time, bool) { return time.Time{}, false }
