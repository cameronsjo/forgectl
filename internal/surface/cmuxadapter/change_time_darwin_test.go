//go:build darwin

package cmuxadapter

import (
	"io/fs"
	"os"
	"syscall"
)

const changeTimeSupported = true

func liveSocketWithChangeTime(inode uint64, nsec int64) func(string) (os.FileInfo, error) {
	return func(string) (os.FileInfo, error) {
		return fakeInfo{
			sys: &syscall.Stat_t{
				Ino:       inode,
				Uid:       testUID,
				Ctimespec: syscall.Timespec{Nsec: nsec},
			},
			mode: fs.ModeSocket | 0o600,
		}, nil
	}
}
