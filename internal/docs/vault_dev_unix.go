//go:build darwin || linux

package docs

import "golang.org/x/sys/unix"

// deviceOf reports path's device id (st_dev), the signal detectRootKind
// uses to stop a walk-up at a mount boundary. The bool is false when path
// cannot be stat'd (already-gone, permission denied); detectRootKind then
// stops the walk and keeps the docs default, since it can no longer prove
// the parent is on the same device. That matches discovery_dir_unix.go's use of golang.org/x/sys/unix
// for the same platform pair.
func deviceOf(path string) (uint64, bool) {
	var stat unix.Stat_t
	if err := unix.Stat(path, &stat); err != nil {
		return 0, false
	}
	return uint64(stat.Dev), true //nolint:gosec // G115: st_dev is a small platform identifier, never a hostile input
}
