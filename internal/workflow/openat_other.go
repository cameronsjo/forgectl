//go:build !unix

package workflow

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/cameronsjo/forgectl/internal/config"
)

// stateDir is the path-based fallback for platforms without openat-relative
// syscalls. It carries no fd — every operation re-resolves d.path, exactly as
// the pre-#128 code did. forgectl's supported hosts are macOS and Linux (both
// unix), so this file exists only to keep `go build ./...` honest on other GOOS
// values, where the same-user symlink hardening the unix build gets is simply
// unavailable.
type stateDir struct {
	path string
}

// openStateDir keeps the Lstat refuse-symlink/file guard and the 0o700 MkdirAll,
// then returns a path-only handle — byte-for-byte the old ensureStateDir
// behavior.
func openStateDir() (*stateDir, error) {
	dir, err := config.WorkflowStateDir()
	if err != nil {
		return nil, err
	}
	if err := guardAndMakeStateDir(dir); err != nil {
		return nil, err
	}
	return &stateDir{path: dir}, nil
}

// createTemp creates a temp file in d.path via os.CreateTemp and returns the
// open file plus its dir-relative base name.
func (d *stateDir) createTemp(pattern string) (*os.File, string, error) {
	f, err := os.CreateTemp(d.path, pattern)
	if err != nil {
		return nil, "", err
	}
	return f, filepath.Base(f.Name()), nil
}

// rename renames oldName to newName, both dir-relative, via os.Rename.
func (d *stateDir) rename(oldName, newName string) error {
	return os.Rename(filepath.Join(d.path, oldName), filepath.Join(d.path, newName))
}

// remove unlinks a dir-relative name; a missing entry is success.
func (d *stateDir) remove(name string) error {
	if err := os.Remove(filepath.Join(d.path, name)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// openLock opens (creating if absent) a dir-relative lock file via os.OpenFile —
// the pre-#128 behavior. There is no O_NOFOLLOW here (the platform lacks it) and
// the advisory lock itself is a documented no-op on non-unix, so the symlink
// hardening is moot.
func (d *stateDir) openLock(name string) (*os.File, error) {
	return os.OpenFile(filepath.Join(d.path, name), os.O_CREATE|os.O_RDWR, 0o600)
}

// syncDir reopens d.path and fsyncs it — the pre-#128 durability half of the
// atomic-rename write.
func (d *stateDir) syncDir() error {
	f, err := os.Open(d.path)
	if err != nil {
		return fmt.Errorf("open state dir for durable rename %s: %w", d.path, err)
	}
	if err := f.Sync(); err != nil {
		f.Close() //nolint:errcheck
		return fmt.Errorf("sync state dir %s: %w", d.path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close state dir after sync %s: %w", d.path, err)
	}
	return nil
}

// close is a no-op — the path-based handle holds no fd.
func (d *stateDir) close() error { return nil }
