// `_unix` is NOT an implicit GOOS suffix the way `_linux` and `_darwin` are, so
// without this constraint the file would compile everywhere and its
// syscall.Stat_t reference would break a Windows build instead of being
// excluded from it.
//go:build unix

package sockstat

import (
	"os"
	"syscall"

	"github.com/cameronsjo/forgectl/internal/surface/backend"
)

func ownerUID(info os.FileInfo) (int, bool) {
	sys, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(sys.Uid), true
}

// Dev is int32 on Darwin and uint64 on Linux, so the conversion is written
// explicitly rather than leaning on an untyped constant.
func fill(in *backend.IncarnationInput, info os.FileInfo) {
	sys, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return
	}
	in.Device = uint64(sys.Dev) //nolint:unconvert,gosec // G115: widening; Dev is int32 on Darwin, uint64 on Linux
	in.Inode = uint64(sys.Ino)  //nolint:unconvert // widening for the same reason
}
