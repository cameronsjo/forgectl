// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

//go:build unix

package rexbin

import (
	"io/fs"
	"syscall"
)

// sysInfo reports the owning uid and the device and inode of the file.
func sysInfo(fi fs.FileInfo) (uid int, dev, ino uint64, ok bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, 0, false
	}
	return int(st.Uid), uint64(st.Dev), uint64(st.Ino), true
}
