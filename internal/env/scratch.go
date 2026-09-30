// scratch.go holds what every scratch directory forgectl creates beside a
// target shares: the `*` .gitignore it carries from creation, and the
// exclusive create that writes it. The `env set --sops` work directory
// (internal/sops) and writeAtomic's directory both use it.
//
// # Why a .gitignore, and why first
//
// A scratch directory sits beside the target, inside the repository. SIGKILL,
// an OOM kill, a crash or a power loss runs no handler, so it can be left
// holding plaintext: the whole new document for writeAtomic, a value or sops'
// decrypted copy for --sops. The next write's leftover scan refuses on it,
// but nothing stopped `git add -A` from committing it first. A `*` pattern
// ignores every entry, this file included, so git never lists the directory
// as untracked and no pathspec short of `git add -f` stages it
// (cameronsjo/forgectl#698, #737). The cost is that the leftover no longer
// shows in `git status`; the scan, which lists the parent directory rather
// than asking git, still finds it and refuses.
//
// It is written before anything else goes in, so no plaintext ever sits in
// the directory without it, and it is written exclusively: O_CREAT|O_EXCL
// fails on anything already at the name, including a planted symlink, which
// it never follows.
//
// What it does not cover: `git stash --all` (or `-a`) stashes ignored files
// too. It copies a live or leftover scratch directory, plaintext included,
// into a stash commit in the object store, and removes it from the working
// tree, where the leftover scan can no longer see it. Measured on git 2.43:
// `git show 'stash@{0}^3:<dir>/<file>'` prints the plaintext afterwards.
package env

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// errScratchNotEmpty reports a scratch directory left in place because
// something besides its .gitignore is still inside it.
var errScratchNotEmpty = errors.New("the scratch directory still holds something besides its .gitignore, so it and its .gitignore were left in place")

// scratchIgnoreRemovable is the teardown rule every scratch directory follows,
// whichever code removes it: the .gitignore is removed last, and only when it
// is the directory's sole remaining entry (or the directory is already
// empty). While anything else is inside, above all a plaintext file whose
// unlink failed, the .gitignore stays, so git still neither lists nor stages
// it, and the next write's leftover scan still refuses on the directory.
// Removing the .gitignore first would turn exactly that failure into a
// committable file.
func scratchIgnoreRemovable(names []string) bool {
	return len(names) == 0 || (len(names) == 1 && names[0] == ScratchIgnoreName)
}

// RemoveScratchDir removes the scratch directory dir by the teardown rule
// (scratchIgnoreRemovable): its .gitignore and then the directory, only when
// nothing else is left in it. It removes no other entry; a caller empties the
// directory of its own files first. When something is left, it returns an
// error and leaves the directory and its .gitignore in place.
func RemoveScratchDir(dir string) error {
	entries, err := os.ReadDir(filepath.Clean(dir))
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if !scratchIgnoreRemovable(names) {
		return errScratchNotEmpty
	}
	// The rule above is the only gate: nothing below checks again.
	if err := os.Remove(filepath.Join(dir, ScratchIgnoreName)); err != nil && !errors.Is(err, fs.ErrNotExist) { //nolint:gosec // G703: inside a scratch directory this process created
		return err
	}
	return os.Remove(filepath.Clean(dir)) //nolint:gosec // G703: a scratch directory this process created
}

// ScratchIgnoreName and ScratchIgnore are the .gitignore every scratch
// directory carries from the moment it exists. See the file comment.
const (
	ScratchIgnoreName = ".gitignore"
	ScratchIgnore     = "*\n"
)

// scratchDirMode is a scratch directory's permission bits. 0700, not 0600: a
// directory needs its execute bit to be entered at all.
const scratchDirMode = 0o700

// WriteFileExclusive creates path with O_CREAT|O_EXCL at 0600 and writes data
// to it. It fails, writing nothing, when anything already exists at path,
// including a symlink, which it never follows. A partially written file is
// left for the caller's directory removal to take, since the caller is always
// about to remove the scratch directory it sits in.
func WriteFileExclusive(path string, data []byte) error {
	f, err := os.OpenFile(filepath.Clean(path), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // G304/G703: callers pass a path inside the 0700 scratch directory this process just created
	if err != nil {
		return err
	}
	return writeAndClose(f, data)
}

// writeAndClose writes data to f and closes it, reporting the first error.
func writeAndClose(f *os.File, data []byte) error {
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// MakeScratchDir creates a 0700 directory in parent named pattern plus
// os.MkdirTemp's random suffix, and writes its .gitignore exclusively before
// returning. On any failure it removes what it created and returns "".
//
// It goes by path, which is right for the --sops work directory: that path is
// sops' TMPDIR and is handed to a child process by name anyway. writeAtomic
// does NOT use it. It works through a pinned descriptor (dirPin), and a path
// here would be the re-resolution the pin exists to avoid, so it creates its
// directory with the fd-relative dirPin.mkScratchDir, which writes the same
// .gitignore through the same writeAndClose.
func MakeScratchDir(parent, pattern string) (string, error) {
	dir, err := os.MkdirTemp(parent, pattern)
	if err != nil {
		return "", fmt.Errorf("create: %w", err)
	}
	// MkdirTemp creates 0700 before umask, and umask can only narrow it, so
	// this chmod is about an unusual umask that strips the owner's own bits.
	// Before the .gitignore exists nothing inside is this process's, so a
	// failure removes the directory only if it is still empty.
	if err := os.Chmod(dir, scratchDirMode); err != nil { //nolint:gosec // G302: a directory needs 0700; 0600 makes it non-traversable
		_ = os.Remove(dir) //nolint:gosec // G703: dir is the MkdirTemp directory this process just created
		return "", fmt.Errorf("secure: %w", err)
	}
	if err := WriteFileExclusive(filepath.Join(dir, ScratchIgnoreName), []byte(ScratchIgnore)); err != nil {
		if errors.Is(err, fs.ErrExist) {
			// Not this process's file: never unlink it.
			_ = os.Remove(dir) //nolint:gosec // G703: dir is the MkdirTemp directory this process just created
		} else {
			_ = RemoveScratchDir(dir)
		}
		return "", fmt.Errorf("write its .gitignore: %w", err)
	}
	return dir, nil
}
