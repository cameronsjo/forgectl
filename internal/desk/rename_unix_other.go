//go:build unix && !darwin && !linux

package desk

// renameatNoReplace is link and unlink: no portable no-replace rename exists.
func renameatNoReplace(fromfd int, from string, tofd int, to string) error {
	return linkUnlink(fromfd, from, tofd, to)
}
