// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

//go:build unix

package rexbin

import (
	"io/fs"
	"syscall"
)

// ownerOf reports the uid that owns the file.
func ownerOf(fi fs.FileInfo) (int, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(st.Uid), true
}
