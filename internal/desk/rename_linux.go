//go:build linux

package desk

import "golang.org/x/sys/unix"

// renameatNoReplace is renameat2(RENAME_NOREPLACE), or link and unlink on a
// filesystem without it.
func renameatNoReplace(fromfd int, from string, tofd int, to string) error {
	err := unix.Renameat2(fromfd, from, tofd, to, unix.RENAME_NOREPLACE)
	if err != nil && unsupportedRename(err) {
		return linkUnlink(fromfd, from, tofd, to)
	}
	return err
}
