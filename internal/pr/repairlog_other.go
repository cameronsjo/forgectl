//go:build !unix

package pr

import "os"

// openRepairLogNoFollow off Unix is a plain open: there is no portable
// O_NOFOLLOW or O_NONBLOCK to reach for, so a symlink is followed and a named
// pipe can block here. openRepairLogFile's regular-file check still runs on
// the open handle. Every caller runs under the lifecycle lock, which refuses
// off Unix before any of them is reached, and no shipped binary runs here
// (goreleaser builds linux and darwin only).
func openRepairLogNoFollow(path string, flag int, perm os.FileMode) (*os.File, error) {
	return os.OpenFile(path, flag, perm) //nolint:gosec // inside the 0700 sessions dir
}
