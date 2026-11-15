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
	// Dev and Ino are compared for equality only, never ordered or used as sizes,
	// so a sign-extended Dev (int32 on darwin) still identifies the same file.
	return int(st.Uid), uint64(st.Dev), uint64(st.Ino), true //nolint:gosec // G115: identity tokens, compared for equality only
}
