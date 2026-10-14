//go:build unix

package desk

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// renameNoReplace moves fromSub/file to toSub/file and fails with an error
// matching fs.ErrExist, never replacing it, when toSub/file already exists.
// A missing source fails with fs.ErrNotExist. Both directories are opened
// through the pinned root, so the rename never leaves the desk.
//
// The claim uses it: a plain rename replaces its target, and running/ may
// already hold an item of the same name whose record must survive
// (forgectl#1157).
func (d *Desk) renameNoReplace(fromSub, toSub, file string) error {
	from, err := d.root.Open(fromSub)
	if err != nil {
		return err
	}
	defer from.Close() //nolint:errcheck // a directory opened for its fd
	to, err := d.root.Open(toSub)
	if err != nil {
		return err
	}
	defer to.Close() //nolint:errcheck // a directory opened for its fd
	if err := renameatNoReplace(int(from.Fd()), file, int(to.Fd()), file); err != nil {
		return &os.LinkError{Op: "rename", Old: fromSub + "/" + file, New: toSub + "/" + file, Err: err}
	}
	return nil
}

// linkUnlink is the portable form of renameatNoReplace: link(2) never
// replaces its target, and the source name is removed once the link exists.
// Between the two steps the file has two links; a scan that sees it then
// refuses it until the next scan, as it does an item mid-add.
func linkUnlink(fromfd int, from string, tofd int, to string) error {
	if err := unix.Linkat(fromfd, from, tofd, to, 0); err != nil {
		return err
	}
	if err := unlinkSource(fromfd, from, 0); err != nil {
		// The file must not stay under both names: drop the new link, so it
		// is where it was (or, if someone else moved the source meanwhile,
		// only where they put it).
		if uerr := unix.Unlinkat(tofd, to, 0); uerr != nil {
			return fmt.Errorf("remove %s after linking it: %w (and the new link could not be removed: %w)", from, err, uerr)
		}
		return fmt.Errorf("remove %s after linking it: %w", from, err)
	}
	return nil
}

// unlinkSource removes linkUnlink's source name. Tests replace it to fail
// that step.
var unlinkSource = unix.Unlinkat

// unsupportedRename reports a no-replace rename the filesystem does not
// offer, for which linkUnlink stands in.
func unsupportedRename(err error) bool {
	return errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EOPNOTSUPP)
}
