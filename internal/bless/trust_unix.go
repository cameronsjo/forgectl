//go:build unix

package bless

import (
	"fmt"
	"io/fs"
	"syscall"
)

// statOwnerUID reads the owning uid from a Unix stat result. macOS and Linux
// both back os.FileInfo.Sys() with *syscall.Stat_t, so the anchor's root
// ownership can be verified. An unexpected Sys() type fails closed.
func statOwnerUID(info fs.FileInfo) (uint32, error) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("cannot read anchor owner uid: unexpected stat type %T", info.Sys())
	}
	return st.Uid, nil
}
