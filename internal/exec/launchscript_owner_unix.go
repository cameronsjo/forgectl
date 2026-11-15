//go:build unix

package exec

import (
	"os"
	"syscall"
)

// ownedByCurrentUser reports whether info names a file or directory owned by
// the effective user, the owner os.MkdirTemp gives what it creates. A mode
// check alone cannot tell a directory this process made from a foreign 0700
// one at the same path; the owner can.
func ownedByCurrentUser(info os.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Geteuid()
}
