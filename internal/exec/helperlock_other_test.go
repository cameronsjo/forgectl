//go:build !unix

package exec

import (
	"os"
	"path/filepath"
	"strconv"
)

// recordHelperPid records this helper's pid in dir as an empty file named
// for it. Without flock there is no lock to hold, so helperLockHeld cannot
// tell a live helper from a reused pid.
func recordHelperPid(dir string) {
	_ = os.WriteFile(filepath.Join(dir, strconv.Itoa(os.Getpid())), nil, 0o600) //nolint:gosec // G703: a directory the test itself passes
}

// helperLockHeld reports true: with no lock to try, cleanup falls back to
// killing every recorded pid.
func helperLockHeld(string) bool { return true }
