//go:build unix

package launch

import (
	"errors"
	"io"

	"golang.org/x/sys/unix"
)

// openUsageReaderUnix opens the store for reading under the same safety policy
// the writer uses. A nil reader with a nil error means the store has never
// been created — an empty report, not a fault.
//
// The lock is shared and nonblocking for the same reason the writer's is
// exclusive and nonblocking: a stats run should say "busy" immediately rather
// than hang behind a launch.
func openUsageReaderUnix(base string) (io.ReadCloser, func(), error) {
	stateFD, err := pinUsageLeaf(base, false)
	if errors.Is(err, errUsageAbsent) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}

	data, err := openUsageFileAt(stateFD, usageDataName, unix.O_RDONLY, false)
	if errors.Is(err, errUsageAbsent) {
		unix.Close(stateFD) //nolint:errcheck // read-only directory descriptor
		return nil, nil, nil
	}
	if err != nil {
		unix.Close(stateFD) //nolint:errcheck
		return nil, nil, err
	}

	// The lock file may legitimately be missing while data exists — an
	// operator can delete one and not the other. Creating it is the only
	// write a read path performs, and it creates no data.
	lock, err := openUsageFileAt(stateFD, usageLockName, unix.O_RDWR, true)
	if err != nil {
		data.Close()        //nolint:errcheck // read-only
		unix.Close(stateFD) //nolint:errcheck
		return nil, nil, err
	}
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_SH|unix.LOCK_NB); err != nil {
		lock.Close()        //nolint:errcheck
		data.Close()        //nolint:errcheck
		unix.Close(stateFD) //nolint:errcheck
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, nil, ErrUsageBusy
		}
		return nil, nil, unsafeStore("lock the usage store: %s", err)
	}

	// Nothing here was opened for writing, so every close is discardable: a
	// close error on a read-only descriptor cannot have lost data, and the
	// caller has already consumed whatever it read.
	release := func() {
		unix.Flock(int(lock.Fd()), unix.LOCK_UN) //nolint:errcheck // closing releases it anyway
		lock.Close()                             //nolint:errcheck // read-only, see above
		data.Close()                             //nolint:errcheck // read-only, see above
		unix.Close(stateFD)                      //nolint:errcheck // read-only, see above
	}
	return data, release, nil
}

// inspectUsageStoreUnix reports presence and safety without creating anything.
// Doctor must be able to describe a broken store without becoming the process
// that half-fixes it.
//
// It is not, however, side-effect free. Inspection goes through the same
// pinUsageLeaf/openUsageFileAt path the writer and reader use, and that path
// narrows a mode broader than the store's own — 0700 for the leaf, 0600 for
// the files — as a condition of handing back a descriptor. Refusing to reuse
// the safe opener here would mean doctor judging the store by weaker rules
// than the writer applies, so the narrowing stays and is reported instead:
// every path tightened lands in status.Narrowed, because "something widened
// your store" is exactly the finding an operator runs doctor to get.
func inspectUsageStoreUnix(paths UsageStorePaths) (UsageStoreStatus, error) {
	status := UsageStoreStatus{Paths: paths}

	// Read the leaf's mode by name before the pin, since the pin is what
	// changes it. This stat informs the report only — every safety decision
	// below is made against a pinned descriptor, never against this result.
	leafMode, leafModeKnown := usageNamedMode(paths.Leaf)

	stateFD, err := pinUsageLeaf(paths.Base, false)
	if errors.Is(err, errUsageAbsent) {
		return status, nil
	}
	if err != nil {
		status.Refusal = err
		return status, nil
	}
	defer unix.Close(stateFD) //nolint:errcheck // read-only directory descriptor
	status.LeafPresent = true
	if leafModeKnown && leafMode != usageLeafMode {
		status.Narrowed = append(status.Narrowed, paths.Leaf)
	}

	dataPresent, dataMode, dataModeKnown := usageEntryMode(stateFD, usageDataName)
	lockPresent, lockMode, lockModeKnown := usageEntryMode(stateFD, usageLockName)
	status.DataPresent = dataPresent
	status.LockPresent = lockPresent

	// Presence alone is not health: prove each present file is one forgectl
	// would actually be willing to open, using the same opener the writer and
	// reader use. A slice rather than a map because Narrowed is printed, and a
	// reported list must not reorder itself between runs.
	entries := []struct {
		name      string
		path      string
		present   bool
		mode      uint32
		modeKnown bool
	}{
		{usageDataName, paths.Data, dataPresent, dataMode, dataModeKnown},
		{usageLockName, paths.Lock, lockPresent, lockMode, lockModeKnown},
	}
	for _, entry := range entries {
		if !entry.present {
			continue
		}
		file, openErr := openUsageFileAt(stateFD, entry.name, unix.O_RDONLY, false)
		if openErr != nil {
			status.Refusal = openErr
			return status, nil
		}
		file.Close() //nolint:errcheck // read-only
		if entry.modeKnown && entry.mode != usageFileMode {
			status.Narrowed = append(status.Narrowed, entry.path)
		}
	}
	return status, nil
}

// usageNamedMode reads a path's permission bits without following a final
// symlink. A failure means "cannot describe", never "mode zero" — the caller
// must not report a narrowing it could not observe.
func usageNamedMode(path string) (uint32, bool) {
	var stat unix.Stat_t
	if err := unix.Lstat(path, &stat); err != nil {
		return 0, false
	}
	return uint32(stat.Mode) & 0o7777, true
}

// usageEntryMode reports whether the fixed name resolves to anything at all,
// and its permission bits when they could be read. Only ENOENT counts as
// absent: any other failure means the entry exists in some state this process
// cannot describe, and reporting that as "nothing recorded yet" would turn a
// broken store into a clean bill of health. Routing it through the present
// branch instead sends it to the safe opener, which says exactly what it
// refused.
func usageEntryMode(stateFD int, name string) (present bool, mode uint32, modeKnown bool) {
	var stat unix.Stat_t
	err := unix.Fstatat(stateFD, name, &stat, unix.AT_SYMLINK_NOFOLLOW)
	if err == nil {
		return true, uint32(stat.Mode) & 0o7777, true
	}
	if errors.Is(err, unix.ENOENT) {
		return false, 0, false
	}
	return true, 0, false
}
