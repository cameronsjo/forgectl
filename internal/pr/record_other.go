//go:build !unix

package pr

import "os"

// OpenExclusive off Unix has O_EXCL but no portable O_NOFOLLOW. The exclusive
// create still refuses a pre-placed entry of any kind, which is the property
// the writer needs; the missing flag only matters for a symlink created
// between the check and the open, and no shipped binary runs here
// (goreleaser builds linux and darwin only).
func (osRecordFS) OpenExclusive(path string) (recordFile, error) {
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // a temp inside the sessions dir
}
