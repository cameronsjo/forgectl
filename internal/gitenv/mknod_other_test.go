//go:build unix && !linux && !darwin

package gitenv_test

import "errors"

// mknodZero is not built for the remaining unix platforms, whose Mknod takes
// a differently typed device number; the caller skips the device case.
func mknodZero(string) error {
	return errors.New("device fixture not built on this platform")
}
