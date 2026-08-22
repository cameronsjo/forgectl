//go:build !darwin && !linux

package cmuxadapter

import "os"

const changeTimeSupported = false

func liveSocketWithChangeTime(inode uint64, _ int64) func(string) (os.FileInfo, error) {
	return liveSocket(inode)
}
