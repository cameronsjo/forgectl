// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

//go:build !unix

package rexbin

import "io/fs"

// sysInfo has no answer off unix; the ownership gate then refuses every
// bundle candidate, which is the safe reading (Rex is macOS-only).
func sysInfo(fs.FileInfo) (int, uint64, uint64, bool) { return 0, 0, 0, false }
