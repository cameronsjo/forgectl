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

// Non-Unix builds are contributor/developer targets. Pinning with a second
// descriptor preserves the same-file proof where the platform permits it;
// secure legacy mutation is refused before this writer is reached.
func pinConfigTemp(f *os.File) (*os.File, error) {
	pinned, err := os.Open(f.Name())
	if err != nil {
		return nil, err
	}
	original, err := f.Stat()
	if err != nil {
		_ = pinned.Close()
		return nil, err
	}
	opened, err := pinned.Stat()
	if err != nil || !os.SameFile(original, opened) {
		_ = pinned.Close()
		return nil, errConfigTempIdentityLost
	}
	return pinned, nil
}

// Non-Unix builds are contributor/developer builds rather than shipped
// targets. They may make a normal config replacement visible, but cannot
// claim directory-entry durability or cross-process serialization.
func ensureNormalConfigParent(path string, _ configParentOps) ([]string, error) {
	return nil, os.MkdirAll(filepath.Dir(path), 0o700)
}
