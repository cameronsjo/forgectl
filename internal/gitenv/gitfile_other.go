//go:build !unix

package gitenv

import "os"

// openGitfile opens a gitfile for reading. There is no non-blocking,
// no-follow open off unix; the caller's Lstat found a regular file, and
// it checks the open handle again.
func openGitfile(path string) (*os.File, error) {
	return os.Open(path) //nolint:gosec // G304: a gitfile the caller found with Lstat
}
