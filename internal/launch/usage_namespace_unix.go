//go:build unix

package launch

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"

	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// errUsageAbsent distinguishes "the namespace has never been created" from
// "the namespace exists and forgectl refused it". The reader and doctor treat
// the first as an empty store and the second as a problem worth reporting.
var errUsageAbsent = errors.New("launch usage namespace is absent")

const dirOpenFlags = unix.O_DIRECTORY | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_RDONLY

// unsafeStore wraps a refusal reason under ErrUsageUnsafeStore. The reason is
// terminal-safe because doctor prints it.
func unsafeStore(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrUsageUnsafeStore, termsafe.SafeLine(fmt.Sprintf(format, args...)))
}

// pinUsageLeaf returns a descriptor on the fixed `forgectl` state leaf, with
// every check that descriptor's later use depends on already performed.
//
// The pin is the whole point: after this returns, lock and data are opened
// with openat against the returned fd, never by rebuilding a path string. An
// attacker who swaps the visible leaf for a symlink after this call still
// cannot redirect a single later operation, because no later operation
// consults the name again.
//
// The state base itself is created with an ordinary MkdirAll and then pinned
// O_NOFOLLOW on its final component. A strict component-by-component no-follow
// walk was tried and rejected: on macOS the standard temp and state roots sit
// under symlinked ancestors (/var → /private/var), so the strict walk refuses
// paths the operator legitimately uses. Everything below the base — where the
// names are predictable and therefore worth attacking — is descriptor-relative
// and fully verified.
func pinUsageLeaf(base string, create bool) (int, error) {
	baseFD, err := unix.Open(base, dirOpenFlags, 0)
	if errors.Is(err, unix.ENOENT) && create {
		if mkErr := createUsageBase(base); mkErr != nil {
			return -1, mkErr
		}
		baseFD, err = unix.Open(base, dirOpenFlags, 0)
	}
	switch {
	case errors.Is(err, unix.ENOENT):
		return -1, errUsageAbsent
	case errors.Is(err, unix.ELOOP), errors.Is(err, unix.ENOTDIR):
		return -1, unsafeStore("state base is a symlink or not a directory")
	case err != nil:
		return -1, unsafeStore("open state base: %s", err)
	}
	defer unix.Close(baseFD) //nolint:errcheck // read-only descriptor

	if create {
		if err := unix.Mkdirat(baseFD, usageLeafDir, usageLeafMode); err != nil && !errors.Is(err, unix.EEXIST) {
			return -1, unsafeStore("create state leaf: %s", err)
		}
	}

	stateFD, err := unix.Openat(baseFD, usageLeafDir, dirOpenFlags, 0)
	switch {
	case errors.Is(err, unix.ENOENT):
		return -1, errUsageAbsent
	case errors.Is(err, unix.ELOOP), errors.Is(err, unix.ENOTDIR):
		return -1, unsafeStore("state leaf is a symlink or not a directory")
	case err != nil:
		return -1, unsafeStore("open state leaf: %s", err)
	}

	if err := verifyUsageLeaf(baseFD, stateFD); err != nil {
		unix.Close(stateFD) //nolint:errcheck // refusing; nothing was written
		return -1, err
	}
	return stateFD, nil
}

// createUsageBase materialises the state base, private to this user, without
// imposing that privacy on directories it merely passes through.
//
// The split matters because the base is often several levels below a shared
// root an operator has not created yet — enabling statistics on a machine with
// no ~/.local should not be what decides ~/.local is 0700 forever. Ancestors
// therefore get the conventional directory mode and only the base itself is
// narrowed; the leaf beneath it, which is what actually holds the data, is
// created and pinned at usageLeafMode by the caller either way.
func createUsageBase(base string) error {
	if err := os.MkdirAll(filepath.Dir(base), usageAncestorMode); err != nil {
		return unsafeStore("create state base ancestors: %s", err)
	}
	if err := os.Mkdir(base, usageLeafMode); err != nil && !errors.Is(err, os.ErrExist) {
		return unsafeStore("create state base: %s", err)
	}
	return nil
}

// verifyUsageLeaf proves the pinned descriptor is the same object the leaf
// name resolves to, that it belongs to this user, and that it is not readable
// by anyone else — in that order, and all of it before any file beneath it is
// created or opened.
func verifyUsageLeaf(baseFD, stateFD int) error {
	var pinned unix.Stat_t
	if err := unix.Fstat(stateFD, &pinned); err != nil {
		return unsafeStore("stat state leaf: %s", err)
	}
	if pinned.Mode&unix.S_IFMT != unix.S_IFDIR {
		return unsafeStore("state leaf is not a directory")
	}
	if int(pinned.Uid) != os.Geteuid() {
		return unsafeStore("state leaf is owned by another user")
	}

	var named unix.Stat_t
	if err := unix.Fstatat(baseFD, usageLeafDir, &named, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return unsafeStore("stat state leaf entry: %s", err)
	}
	if named.Dev != pinned.Dev || named.Ino != pinned.Ino {
		return unsafeStore("state leaf entry does not match the pinned directory")
	}

	// Narrow a too-broad leaf only after identity and ownership are proven,
	// so a chmod can never land on an object that was never ours.
	if pinned.Mode&0o7777 != usageLeafMode {
		if err := unix.Fchmod(stateFD, usageLeafMode); err != nil {
			return unsafeStore("restrict state leaf mode: %s", err)
		}
		var rechecked unix.Stat_t
		if err := unix.Fstat(stateFD, &rechecked); err != nil {
			return unsafeStore("re-stat state leaf: %s", err)
		}
		if rechecked.Ino != pinned.Ino || rechecked.Dev != pinned.Dev ||
			rechecked.Mode&0o7777 != usageLeafMode || int(rechecked.Uid) != os.Geteuid() {
			return unsafeStore("state leaf changed identity while being restricted")
		}
	}
	return nil
}
