//go:build unix

package cli

import (
	"io/fs"
	"syscall"
)

// fileOwner returns fi's owning uid, when the platform reports one.
func fileOwner(fi fs.FileInfo) (int, bool) {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return int(st.Uid), true
	}
	return 0, false
}
