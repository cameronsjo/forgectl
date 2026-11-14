// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

//go:build !unix

package rexbin

import "io/fs"

// ownerOf has no answer off unix; the ownership gate then refuses every
// bundle candidate, which is the safe reading (Rex is macOS-only).
func ownerOf(fs.FileInfo) (int, bool) { return 0, false }
