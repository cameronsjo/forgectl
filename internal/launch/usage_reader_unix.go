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

// inspectUsageStoreUnix reports presence and safety without creating or
// repairing anything. Doctor must be able to describe a broken store without
// becoming the process that half-fixes it.
func inspectUsageStoreUnix(paths UsageStorePaths) (UsageStoreStatus, error) {
	status := UsageStoreStatus{Paths: paths}

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

	status.DataPresent = usageEntryPresent(stateFD, usageDataName)
	status.LockPresent = usageEntryPresent(stateFD, usageLockName)

	// Presence alone is not health: prove each present file is one forgectl
	// would actually be willing to open, using the same opener the writer and
	// reader use — read-only and create-free, so inspecting repairs nothing.
	for name, present := range map[string]bool{
		usageDataName: status.DataPresent,
		usageLockName: status.LockPresent,
	} {
		if !present {
			continue
		}
		file, openErr := openUsageFileAt(stateFD, name, unix.O_RDONLY, false)
		if openErr != nil {
			status.Refusal = openErr
			return status, nil
		}
		file.Close() //nolint:errcheck // read-only
	}
	return status, nil
}

// usageEntryPresent reports whether the fixed name resolves to anything at
// all. Only ENOENT counts as absent: any other failure means the entry exists
// in some state this process cannot describe, and reporting that as "nothing
// recorded yet" would turn a broken store into a clean bill of health. Routing
// it through the present branch instead sends it to the safe opener, which
// says exactly what it refused.
func usageEntryPresent(stateFD int, name string) bool {
	var stat unix.Stat_t
	err := unix.Fstatat(stateFD, name, &stat, unix.AT_SYMLINK_NOFOLLOW)
	return err == nil || !errors.Is(err, unix.ENOENT)
}
