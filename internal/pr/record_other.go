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

// openRegularInRoot off Unix has no portable O_NONBLOCK, so the open is plain
// and only the regular-file Fstat after it applies; see the Unix counterpart.
// Every caller runs under the lifecycle lock, which refuses off Unix before
// reaching it, and no shipped binary runs here.
func openRegularInRoot(root *os.Root, name string) (*os.File, error) {
	f, err := root.Open(name)
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
		return nil, &os.PathError{Op: "open", Path: name, Err: errRecordNotRegular}
	}
	return f, nil
}
