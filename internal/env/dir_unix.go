//go:build unix

package env

import (
	"crypto/rand"
	"encoding/base32"
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// errIsSymlink is returned when a name that was resolved to a non-symlink is a
// symlink by the time it is opened. It names no path: the caller composes the
// message from the repo-relative form it already holds.
var errIsSymlink = errors.New("path became a symlink after it was resolved")

// errNotRegular is returned for a name that exists but is not a regular file —
// a directory, a socket, or a FIFO swapped in after resolution.
var errNotRegular = errors.New("path is not a regular file")

// dirFlags open a directory for use as an anchor. Each flag is load-bearing:
// O_DIRECTORY refuses a non-directory rather than returning a descriptor that
// fails later, O_NOFOLLOW refuses a symlinked final component, O_CLOEXEC keeps
// the descriptor out of any child process, and O_RDONLY is all an anchor needs
// — the writes happen through *at syscalls relative to it, not through it.
// The same set internal/privdir uses for the same reason.
const dirFlags = unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC | unix.O_RDONLY

// dirPin is an open handle on the directory containing a target, held from
// resolution until the last operation on that target completes.
//
// # Why a descriptor rather than a re-check
//
// This is what closes the confirmation-window exploit rather than narrowing
// it. Resolving a path and then operating on it by STRING re-walks every
// component at use time, so anything that changed in between takes effect —
// and for the --any-file confirmation the gap is the operator's think-time at
// a prompt, seconds to minutes. An O_NOFOLLOW on the final component is not
// enough, because swapping an intermediate DIRECTORY redirects the whole
// operation just as well: measured, a prompt reading "sub/config" wrote
// core.fsmonitor into .git/config and exited zero.
//
// A descriptor refers to the inode, not the name. Once this is open, every
// openat/renameat/fstatat relative to it reaches the directory that was
// resolved, whatever the path now spells. There is no window to race because
// there is no second resolution.
//
// # The boundary this does NOT claim
//
// Pinning happens by path, immediately after resolution, so the components of
// the parent path are walked once more at that moment — a window measured in
// microseconds rather than in operator think-time. Eliminating even that would
// need a component-by-component openat walk from the repository root. The
// exploitable window was the prompt; this closes it completely, and what
// remains is the ordinary same-uid local race that predates this code.
type dirPin struct{ fd int }

// pinDir opens path as an anchor directory.
func pinDir(path string) (*dirPin, error) {
	fd, err := unix.Open(path, dirFlags, 0)
	switch {
	case errors.Is(err, unix.ELOOP), errors.Is(err, unix.ENOTDIR):
		// Both mean "the thing at that name is not the directory we resolved".
		return nil, errIsSymlink
	case err != nil:
		return nil, err
	}
	return &dirPin{fd: fd}, nil
}

// close releases the descriptor. The error is dropped deliberately: the only
// failure a close on a read-only descriptor reports is that it was already
// closed, and no caller decision depends on it.
func (d *dirPin) close() {
	if d != nil && d.fd >= 0 {
		_ = unix.Close(d.fd)
		d.fd = -1
	}
}

// openRegular opens name inside the pinned directory for reading, refusing a
// symlink and anything that is not a regular file.
//
// O_NONBLOCK and the post-open regular-file check are both required, and each
// covers what the other misses. Without O_NONBLOCK, opening a FIFO BLOCKS
// FOREVER waiting for a writer — measured on darwin 25.5, and it is exactly
// the swap this guard exists to refuse, so the guard would hang instead of
// refusing. With O_NONBLOCK alone the open SUCCEEDS against the FIFO and reads
// zero bytes, which would then be written back as an empty document. Same
// pairing as internal/cli's openRegularNoFollow.
func (d *dirPin) openRegular(name string) (*os.File, error) {
	fd, err := unix.Openat(d.fd, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ELOOP) {
		return nil, errIsSymlink
	}
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), name)
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, errNotRegular
	}
	return f, nil
}

// createRetries bounds openatCreate's retry loop. A spurious ENOENT clears on
// the first or second attempt in practice; the bound is what keeps a genuine
// one (the directory really was removed) from spinning.
const createRetries = 8

// openatCreate is openat with O_CREAT, retrying a spurious ENOENT.
//
// # Why this retry exists
//
// It is not defensive padding. Measured on darwin 25.5 with four goroutines
// racing to create the SAME name in one pinned directory, 200 rounds:
// unix.Openat with O_CREAT succeeded 218 times out of 800 and returned ENOENT
// 582 times, while os.OpenFile with the identical flags on the identical
// directory succeeded 800 out of 800. Roughly one winner per round, everyone
// else told "no such file or directory" by a call whose purpose is to create
// the file. Sequentially it never happens, including a second O_CREAT open of
// a name that already exists.
//
// So this is a property of the fd-relative call on this platform, not a
// filesystem race and not a bug in the caller — and since the whole point of
// the pin is that operations go through a descriptor, the retry is the cost of
// that. What causes it is not established here; naming a mechanism would be
// guessing, and the retry is correct under any of them.
//
// ENOENT is retried ONLY because O_CREAT makes it self-contradictory. The
// read-only opens above must never retry it: there, ENOENT is the real answer
// that tells loadOrEmpty it is creating a new file rather than editing one.
func openatCreate(dirfd int, name string, flags int, perm uint32) (int, error) {
	var err error
	for attempt := 0; attempt < createRetries; attempt++ {
		var fd int
		fd, err = unix.Openat(dirfd, name, flags|unix.O_CREAT, perm)
		if err == nil {
			return fd, nil
		}
		if !errors.Is(err, unix.ENOENT) && !errors.Is(err, unix.EINTR) {
			return -1, err
		}
	}
	return -1, err
}

// openLock creates or opens name inside the pinned directory as a lock file,
// refusing a symlink.
//
// A FIFO here does NOT block — O_RDWR on a FIFO succeeds immediately — so the
// caller's Lstat is what refuses one, not this open. That is worth stating
// because the opposite is the intuitive guess: the blocking case is the
// read-only open above.
func (d *dirPin) openLock(name string) (*os.File, error) {
	fd, err := openatCreate(d.fd, name, unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if errors.Is(err, unix.ELOOP) {
		return nil, errIsSymlink
	}
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), name), nil
}

// lstat reports name's permission bits and whether it is a regular file,
// without following a symlink, plus whether it exists at all.
//
// Permission bits and the regular-file answer come back separately rather than
// as one os.FileMode because unix.Stat_t.Mode is a raw st_mode, whose type
// bits do not share a layout with os.FileMode's. Converting it wholesale and
// calling IsRegular() on the result compiles, runs, and is always wrong — so
// the type bits are tested here, against unix.S_IFMT, where they mean what
// they say.
func (d *dirPin) lstat(name string) (perm os.FileMode, regular, exists bool, err error) {
	var st unix.Stat_t
	if serr := unix.Fstatat(d.fd, name, &st, unix.AT_SYMLINK_NOFOLLOW); serr != nil {
		if errors.Is(serr, unix.ENOENT) {
			return 0, false, false, nil
		}
		return 0, false, false, serr
	}
	return os.FileMode(st.Mode).Perm(), st.Mode&unix.S_IFMT == unix.S_IFREG, true, nil
}

// tempNameBytes is the random component of a temp file name. Ten bytes of
// base32 is plenty to make a collision a non-event while keeping the name
// short enough to stay readable if one is ever left behind.
const tempNameBytes = 10

// createTemp creates a uniquely-named 0600 file inside the pinned directory
// and returns it with its name.
//
// os.CreateTemp cannot be used: it takes a directory PATH, which is the
// re-resolution this type exists to avoid. So the unique name is generated
// here and created with O_EXCL relative to the descriptor, which is also a
// stronger uniqueness guarantee than a probe-then-create.
func (d *dirPin) createTemp(prefix string) (*os.File, string, error) {
	buf := make([]byte, tempNameBytes)
	for attempt := 0; attempt < 10; attempt++ {
		if _, err := rand.Read(buf); err != nil {
			return nil, "", fmt.Errorf("generate temp file name: %w", err)
		}
		name := prefix + base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(buf) + ".tmp"
		fd, err := openatCreate(d.fd, name, unix.O_EXCL|unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
		if errors.Is(err, unix.EEXIST) {
			continue
		}
		if err != nil {
			return nil, "", err
		}
		return os.NewFile(uintptr(fd), name), name, nil
	}
	return nil, "", errors.New("could not create a uniquely named temp file")
}

// rename moves from to to, both inside the pinned directory. Renameat with the
// same descriptor on both sides is what keeps the swap atomic AND anchored:
// neither name is re-resolved from the process working directory.
func (d *dirPin) rename(from, to string) error {
	return unix.Renameat(d.fd, from, d.fd, to)
}

// remove unlinks name inside the pinned directory.
func (d *dirPin) remove(name string) error {
	return unix.Unlinkat(d.fd, name, 0)
}
