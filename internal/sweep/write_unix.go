//go:build unix

package sweep

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"

	"github.com/cameronsjo/forgectl/internal/attention"
	"github.com/cameronsjo/forgectl/internal/privdir"
)

const (
	leafName = "sweep.json"
	fileMode = 0o600
	// newDirMode is the mode of <state>/cadence when Write has to create it.
	newDirMode = 0o700
	// ancestorMode is for the state directory itself, when absent.
	ancestorMode = 0o755
)

// Write replaces <stateBase>/cadence/sweep.json with data, atomically: it
// writes a fresh O_EXCL temporary file (0600) beside it and renames it over
// the target, so a reader sees the old file or the new one, never half of one.
//
// <state>/cadence is shared with the attention spool and other logs, so Write
// does not narrow an existing one (a wider mode is what the spool left, and
// narrowing it would change what other tools see). It refuses one that is a
// symlink, is another user's, or that anyone else can write to, and creates a
// missing one at 0700. The file's own 0600 mode is what keeps the ledger
// private.
func Write(stateBase string, data []byte) error {
	if !filepath.IsAbs(stateBase) {
		return errors.New("sweep: the state directory must be an absolute path")
	}
	fd, err := openParent(filepath.Join(filepath.Clean(stateBase), spoolParent))
	if err != nil {
		return err
	}
	defer unix.Close(fd) //nolint:errcheck // nothing buffered

	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Errorf("sweep: tmp name: %w", err)
	}
	tmp := "." + leafName + ".tmp-" + hex.EncodeToString(b[:])
	f, err := privdir.OpenFile(fd, tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fileMode)
	if err != nil {
		return fmt.Errorf("sweep: create %s: %s", tmp, privdir.Reason(err))
	}
	_, werr := f.Write(data)
	serr := f.Sync()
	cerr := f.Close()
	if err := errors.Join(werr, serr, cerr); err != nil {
		_ = unix.Unlinkat(fd, tmp, 0) // cleanup; the write error is the one returned
		return fmt.Errorf("sweep: write %s: %w", tmp, err)
	}
	if err := unix.Renameat(fd, tmp, fd, leafName); err != nil {
		_ = unix.Unlinkat(fd, tmp, 0) // cleanup; the rename error is the one returned
		return fmt.Errorf("sweep: replace %s: %w", leafName, err)
	}
	return nil
}

// spoolParent is the directory under the state base that holds sweep.json
// and the attention spool.
const spoolParent = "cadence"

// openParent opens dir without following a symlink and checks it is safe to
// put the ledger in, creating it (and the state directory above) when absent.
func openParent(dir string) (int, error) {
	open := func() (int, error) {
		return unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	}
	fd, err := open()
	if errors.Is(err, unix.ENOENT) {
		if err := os.MkdirAll(filepath.Dir(dir), ancestorMode); err != nil {
			return -1, fmt.Errorf("sweep: create the state directory: %w", err)
		}
		if err := unix.Mkdir(dir, newDirMode); err != nil && !errors.Is(err, unix.EEXIST) {
			return -1, fmt.Errorf("sweep: create %s: %w", dir, err)
		}
		fd, err = open()
	}
	switch {
	case errors.Is(err, unix.ELOOP), errors.Is(err, unix.ENOTDIR):
		return -1, fmt.Errorf("sweep: %s is a symlink or not a directory; refusing to write there", dir)
	case err != nil:
		return -1, fmt.Errorf("sweep: open %s: %w", dir, err)
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		_ = unix.Close(fd) // refusing; the stat error is the one returned
		return -1, fmt.Errorf("sweep: stat %s: %w", dir, err)
	}
	switch {
	case int(st.Uid) != os.Geteuid():
		_ = unix.Close(fd) // refusing
		return -1, fmt.Errorf("sweep: %s is not yours; refusing to write there", dir)
	case uint32(st.Mode)&0o022 != 0:
		_ = unix.Close(fd) // refusing
		return -1, fmt.Errorf("sweep: %s is writable by others (mode %04o); fix it with chmod go-w", dir, uint32(st.Mode)&0o7777)
	}
	return fd, nil
}

// Path is where Write puts the ledger, for messages.
func Path(stateBase string) string { return attention.SweepPath(stateBase) }
