//go:build linux || darwin

package gitenv_test

import "golang.org/x/sys/unix"

// mknodZero makes p a character device with /dev/zero's numbers. Mknod's
// device argument is an int here and a uint64 on the BSDs, hence the split.
func mknodZero(p string) error {
	return unix.Mknod(p, unix.S_IFCHR|0o600, int(unix.Mkdev(1, 5))) //nolint:gosec // G115: device numbers 1,5 fit
}
