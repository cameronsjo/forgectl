//go:build !unix

package cli

import (
	"fmt"
	"os"
)

// Non-Unix platforms lack the uid/link-count and O_NOFOLLOW guarantees used
// on macOS and Linux. This fallback conservatively rejects a leaf symlink or
// non-regular file and verifies that the opened descriptor is the same object
// observed immediately before open.
func openDocsTokenFile(path string) (openedDocsTokenFile, error) {
	displayPath := safeDocsTokenPath(path)
	before, err := os.Lstat(path)
	if err != nil {
		return nil, wrapDocsTokenDescriptorError("inspect", path, err)
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() {
		return nil, fmt.Errorf("token file is not a regular file: %s", displayPath)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, wrapDocsTokenDescriptorError("open", path, err)
	}
	after, err := file.Stat()
	if err != nil {
		return nil, closeInvalidDocsTokenFile(file, wrapDocsTokenDescriptorError("inspect opened", displayPath, err))
	}
	if !after.Mode().IsRegular() || !os.SameFile(before, after) {
		return nil, closeInvalidDocsTokenFile(file, fmt.Errorf("token file changed while opening: %s", displayPath))
	}
	return file, nil
}
