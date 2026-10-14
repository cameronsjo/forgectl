//go:build darwin

package desk

import "golang.org/x/sys/unix"

// renameatNoReplace is renameatx_np(RENAME_EXCL), or link and unlink on a
// filesystem without it.
func renameatNoReplace(fromfd int, from string, tofd int, to string) error {
	err := unix.RenameatxNp(fromfd, from, tofd, to, unix.RENAME_EXCL)
	if err != nil && unsupportedRename(err) {
		return linkUnlink(fromfd, from, tofd, to)
	}
	return err
}
