//go:build !unix

package pr

import "os"

// openInRootNoFollowNonblock opens name inside dir. Off Unix there is no
// O_NOFOLLOW or O_NONBLOCK to pass, so it is dir's own open: still bound to
// the directory dir pinned, which is the forgectl#685 property. The caller's
// Fstat regular-file check still runs on the returned handle.
func openInRootNoFollowNonblock(dir *os.Root, name string) (*os.File, error) {
	return dir.OpenFile(name, os.O_RDONLY, 0)
}
