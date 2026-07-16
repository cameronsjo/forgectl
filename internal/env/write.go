// write.go is the atomic write path every env command that mutates a file
// goes through: a temp file created 0600 from the start (os.CreateTemp's
// own default — there is no chmod window to close), written, synced,
// then renamed into place.
package env

import (
	"fmt"
	"os"
	"path/filepath"
)

// secureMode is the permission bits a .env file should carry: owner
// read/write only. os.CreateTemp already creates its file at 0600 (before
// umask, which can only narrow it further — 0600 has no group/other bits
// for umask to strip), so writeAtomic never needs an explicit os.Chmod.
const secureMode = 0o600

// writeAtomic writes data to realPath by creating a temp file in the same
// directory (0600 from creation), writing, syncing, closing, then renaming
// over realPath. The temp file is removed on any error before that final
// rename. A hardlink pointed at realPath is neutralized by the rename —
// rename swaps the directory entry to a fresh inode, it never touches the
// old one through any other link.
//
// tightened reports whether realPath existed beforehand with permission
// bits looser than secureMode (e.g. group/other read) — the CLI surfaces
// this as a one-line "tightened <file> to 0600" note. A brand-new file, or
// one that was already exactly (or more strictly than) secureMode, is not
// "tightened": there was nothing loose to fix.
//
// Every error here carries paths only — data is never interpolated into
// any error string.
func writeAtomic(realPath string, data []byte) (tightened bool, err error) {
	dir := filepath.Dir(realPath)

	hadPrior := false
	var priorMode os.FileMode
	if fi, statErr := os.Stat(realPath); statErr == nil {
		hadPrior = true
		priorMode = fi.Mode().Perm()
	}

	tmp, err := os.CreateTemp(dir, ".env-*.tmp")
	if err != nil {
		return false, fmt.Errorf("create temp file in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}

	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return false, fmt.Errorf("write %s: %w", filepath.Base(realPath), err)
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return false, fmt.Errorf("sync %s: %w", filepath.Base(realPath), err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return false, fmt.Errorf("close %s: %w", filepath.Base(realPath), err)
	}

	if err := os.Rename(tmpPath, realPath); err != nil {
		_ = os.Remove(tmpPath)
		return false, fmt.Errorf("rename into place %s: %w", filepath.Base(realPath), err)
	}

	tightened = hadPrior && priorMode&^secureMode != 0
	return tightened, nil
}
