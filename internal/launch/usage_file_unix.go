//go:build unix

package launch

import (
	"errors"
	"io/fs"
	"os"

	"github.com/cameronsjo/forgectl/internal/privdir"
)

// openUsageFileAt opens one fixed name relative to the pinned state leaf and
// proves it is safe before returning it. Writer, reader, and doctor all come
// through here, so a fixture that fools one of them cannot be safe for the
// others.
//
// The check itself is privdir.OpenFile, which the surface worker ledger
// shares (forgectl#1048): a no-follow, non-blocking openat, then type,
// ownership, link count and identity proven on the descriptor before a
// broad mode is narrowed to usageFileMode. What stays here is the
// translation back into the two error identities the reader and doctor
// branch on.
func openUsageFileAt(stateFD int, name string, flags int, create bool) (*os.File, error) {
	if create {
		flags |= os.O_CREATE
	}
	file, err := privdir.OpenFile(stateFD, name, flags, usageFileMode)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, errUsageAbsent
	case err != nil:
		return nil, unsafeStore("%s", privdir.Reason(err))
	}
	return file, nil
}
