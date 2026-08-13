//go:build !unix

package cli

import (
	"os"
	"path/filepath"
)

func syncDirectory(string) error { return errDirectoryDurabilityUnsupported }

func openRegularNoFollow(path string) (*os.File, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, errConfigDestinationNonRegular
	}
	return f, nil
}

// Non-Unix builds are contributor/developer builds rather than shipped
// targets. They may make a normal config replacement visible, but cannot
// claim directory-entry durability or cross-process serialization.
func ensureNormalConfigParent(path string, _ configParentOps) ([]string, error) {
	return nil, os.MkdirAll(filepath.Dir(path), 0o700)
}
