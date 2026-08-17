//go:build unix

package privdir

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// dirOpenFlags is the only way this package opens a directory.
//
// Each flag is load-bearing. O_DIRECTORY refuses a non-directory rather than
// discovering it later; O_NOFOLLOW refuses a symlink at the final component,
// which is the substitution being defended against; O_CLOEXEC keeps the
// descriptor out of any child this process execs; O_RDONLY is all that is
// needed, since every write below goes through a descriptor-relative call.
const dirOpenFlags = unix.O_DIRECTORY | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_RDONLY

// unsafe wraps a refusal reason under ErrUnsafe, terminal-safe because callers
// print it.
func unsafe(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrUnsafe, termsafe.SafeLine(fmt.Sprintf(format, args...)))
}

// Pin returns a descriptor on the spec's leaf directory.
//
// After this returns, every file beneath the leaf must be opened with openat
// against the returned descriptor, never by rebuilding a path string. That is
// the whole guarantee: an attacker who swaps the visible leaf for a symlink
// after this call cannot redirect a single later operation, because no later
// operation consults the name again.
//
// The caller owns the descriptor and must close it.
func Pin(s Spec) (int, error) {
	if err := s.validate(); err != nil {
		return -1, err
	}

	baseFD, err := unix.Open(s.Base, dirOpenFlags, 0)
	if errors.Is(err, unix.ENOENT) && s.Create {
		if mkErr := s.createBase(); mkErr != nil {
			return -1, mkErr
		}
		baseFD, err = unix.Open(s.Base, dirOpenFlags, 0)
	}
	switch {
	case errors.Is(err, unix.ENOENT):
		return -1, ErrAbsent
	case errors.Is(err, unix.ELOOP), errors.Is(err, unix.ENOTDIR):
		return -1, unsafe("base is a symlink or not a directory")
	case err != nil:
		return -1, unsafe("open base: %s", err)
	}
	defer unix.Close(baseFD) //nolint:errcheck // read-only descriptor

	if s.Create {
		// Created at the final mode, not created broad and narrowed after.
		// verify below would repair a broad leaf, but a repair leaves a window
		// between the mkdirat and the fchmod in which the directory exists and
		// is open to others. The narrowing path is for directories that were
		// already there; this is for ones we make.
		if err := unix.Mkdirat(baseFD, s.Leaf, uint32(s.Mode.Perm())); err != nil && !errors.Is(err, unix.EEXIST) {
			return -1, unsafe("create leaf: %s", err)
		}
	}

	leafFD, err := unix.Openat(baseFD, s.Leaf, dirOpenFlags, 0)
	switch {
	case errors.Is(err, unix.ENOENT):
		return -1, ErrAbsent
	case errors.Is(err, unix.ELOOP), errors.Is(err, unix.ENOTDIR):
		return -1, unsafe("leaf is a symlink or not a directory")
	case err != nil:
		return -1, unsafe("open leaf: %s", err)
	}

	if err := s.verify(baseFD, leafFD); err != nil {
		//nolint:errcheck,gosec // G104: refusing, and nothing was written; a
		// close failure here cannot change the refusal we are about to return.
		unix.Close(leafFD)
		return -1, err
	}
	return leafFD, nil
}

// validate refuses a spec that cannot describe a checkable directory, before
// anything is created. A multi-segment or traversing leaf is the case that
// matters: it would put intermediate names back into the resolution this
// package exists to remove.
//
// The separator test is explicit rather than a round-trip through
// filepath.Base, because Base is not injective on the inputs that matter.
// filepath.Base("/") returns "/" unchanged — documented behaviour — so a
// round-trip admits the one absolute path that must never get through. POSIX
// ignores the directory descriptor when a path is absolute, so an accepted "/"
// would make openat hand back a descriptor on the root filesystem, whose name
// then agrees with itself under the identity check; a caller running as root
// would go on to chmod / to the leaf mode.
func (s Spec) validate() error {
	if s.Base == "" {
		return fmt.Errorf("%w: no base directory", ErrUnsafe)
	}
	switch {
	case s.Leaf == "", s.Leaf == ".", s.Leaf == "..":
		return fmt.Errorf("%w: leaf must be a single path component", ErrUnsafe)
	case strings.ContainsRune(s.Leaf, filepath.Separator):
		return fmt.Errorf("%w: leaf must not contain a path separator", ErrUnsafe)
	case strings.ContainsRune(s.Leaf, 0):
		// The syscall layer refuses this too, but only after the base has been
		// created — a spec this package can see is wrong should cost nothing.
		return fmt.Errorf("%w: leaf must not contain a NUL", ErrUnsafe)
	}
	if s.Mode.Perm() == 0 {
		return fmt.Errorf("%w: no leaf mode", ErrUnsafe)
	}
	// Only permission bits are honoured — setuid, setgid, and sticky are
	// dropped by Perm() on the way to mkdirat and fchmod. Refusing them is
	// better than silently narrowing: a caller that asked for setgid and got
	// 0700 would believe it has semantics it does not have.
	if s.Mode != s.Mode.Perm() {
		return fmt.Errorf("%w: leaf mode carries bits beyond permissions", ErrUnsafe)
	}
	if s.Create && s.AncestorMode.Perm() == 0 {
		return fmt.Errorf("%w: no ancestor mode", ErrUnsafe)
	}
	if s.AncestorMode != s.AncestorMode.Perm() {
		return fmt.Errorf("%w: ancestor mode carries bits beyond permissions", ErrUnsafe)
	}
	return nil
}

// createBase materialises the base directory, private to this user, without
// imposing that privacy on directories it merely passes through.
func (s Spec) createBase() error {
	if err := os.MkdirAll(filepath.Dir(s.Base), s.AncestorMode.Perm()); err != nil {
		return unsafe("create base ancestors: %s", err)
	}
	if err := os.Mkdir(s.Base, s.Mode.Perm()); err != nil && !errors.Is(err, os.ErrExist) {
		return unsafe("create base: %s", err)
	}
	return nil
}

// verify proves the pinned descriptor is the same object the leaf name
// resolves to, that it belongs to this user, and that it carries the mode the
// caller asked for — in that order, and all of it before any file beneath it
// is created or opened.
//
// Note the third clause is "the caller's mode", not "private": validate
// accepts any permission bits, so how private the result is remains the
// caller's choice. Both of forgectl's callers ask for 0o700.
func (s Spec) verify(baseFD, leafFD int) error {
	want := uint32(s.Mode.Perm())

	var pinned unix.Stat_t
	if err := unix.Fstat(leafFD, &pinned); err != nil {
		return unsafe("stat leaf: %s", err)
	}
	if pinned.Mode&unix.S_IFMT != unix.S_IFDIR {
		return unsafe("leaf is not a directory")
	}
	if int(pinned.Uid) != os.Geteuid() {
		return unsafe("leaf is owned by another user")
	}

	// The name must still resolve to the object we pinned. Without this, a
	// swap between the open and the first use would go unnoticed — the
	// descriptor would be fine and the name would point somewhere else.
	//
	// Against a symlink that was already in place, this and the O_NOFOLLOW on
	// the open are redundant: either refuses it alone. They stop being
	// redundant only against a *live* swap, which no test here exercises — so
	// removing either one leaves the suite green, and that is a gap in the
	// tests rather than evidence the check is dead. Keep both.
	var named unix.Stat_t
	if err := unix.Fstatat(baseFD, s.Leaf, &named, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return unsafe("stat leaf entry: %s", err)
	}
	if named.Dev != pinned.Dev || named.Ino != pinned.Ino {
		return unsafe("leaf entry does not match the pinned directory")
	}

	// Narrow a too-broad leaf only after identity and ownership are proven,
	// so a chmod can never land on an object that was never ours.
	// Stat_t.Mode is uint16 on Darwin and uint32 on Linux, so every comparison
	// against the wanted bits widens explicitly rather than relying on an
	// untyped constant to paper over the difference.
	if uint32(pinned.Mode)&0o7777 != want {
		if err := unix.Fchmod(leafFD, want); err != nil {
			return unsafe("restrict leaf mode: %s", err)
		}
		// The re-check below has no non-racing test, and is kept deliberately.
		// It covers the window *during* the repair, reachable only by an actual
		// concurrent swap. Every other check here is confirmable from a
		// statically planted hostile directory; this one is not, so it reads as
		// dead code to a mutation probe and is not.
		var rechecked unix.Stat_t
		if err := unix.Fstat(leafFD, &rechecked); err != nil {
			return unsafe("re-stat leaf: %s", err)
		}
		if rechecked.Ino != pinned.Ino || rechecked.Dev != pinned.Dev ||
			uint32(rechecked.Mode)&0o7777 != want || int(rechecked.Uid) != os.Geteuid() {
			return unsafe("leaf changed identity while being restricted")
		}
	}
	return nil
}
