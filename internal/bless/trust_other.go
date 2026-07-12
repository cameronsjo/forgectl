//go:build !unix

package bless

import (
	"fmt"
	"io/fs"
)

// statOwnerUID has no portable implementation off Unix — a platform where the
// owning uid can't be read cannot honor the root-owned-anchor requirement, so it
// fails closed rather than pretend the anchor is trustworthy.
func statOwnerUID(_ fs.FileInfo) (uint32, error) {
	return 0, fmt.Errorf("anchor ownership check is unsupported on this platform")
}
