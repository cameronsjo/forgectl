//go:build !darwin && !linux

package docs

import (
	"io"
	"os"
)

// openLegacyRecord is the portable legacy reader. It makes no ownership or
// link-count claim — those are Unix mode bits, and this build target has no
// equivalent this code can honestly check — but it still refuses anything that
// is not a regular file, so a directory or a device at that path cannot stall
// or mislead the reader.
func openLegacyRecord(path string) (io.ReadCloser, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		file.Close() //nolint:errcheck // already failing
		return nil, err
	}
	if !info.Mode().IsRegular() {
		file.Close() //nolint:errcheck // refusing this record
		return nil, errUnsafeRecord
	}
	return file, nil
}
