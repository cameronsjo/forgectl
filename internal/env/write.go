// write.go is the atomic write path every env command that mutates a file
// goes through: a temp file created 0600 from the start (there is no chmod
// window to close), written, synced, then renamed into place.
package env

import (
	"fmt"
	"os"
)

// secureMode is the permission bits a .env file should carry: owner
// read/write only. The temp file is created at 0600 before umask, which can
// only narrow it further — 0600 has no group/other bits for umask to strip —
// so writeAtomic never needs an explicit chmod.
const secureMode = 0o600

// tempPrefix names the transient file. It deliberately does NOT match
// IsEnvFileName, so a leftover cannot later be reached through --file without
// --any-file, and it is covered by the usual `.env*` gitignore shape.
const tempPrefix = ".env-"

// writeAtomic writes data to the target by creating a temp file in the SAME
// PINNED DIRECTORY, writing, syncing, closing, then renaming over the target's
// name. The temp file is removed on any error before that final rename.
//
// Every filesystem call goes through target.dir rather than through a path.
// That is not stylistic: os.CreateTemp and os.Rename take paths and re-walk
// every component, so an intermediate directory replaced after resolution
// would redirect both the temp creation and the rename out of the repository.
// Relative to a descriptor there is nothing to redirect. See dirPin.
//
// A hardlink pointed at the target is neutralized by the rename — it swaps
// the directory entry to a fresh inode and never touches the old one through
// any other link.
//
// tightened reports whether the target existed beforehand with permission bits
// looser than secureMode (e.g. group/other read) — the CLI surfaces this as a
// one-line "tightened <file> to 0600" note. A brand-new file, or one already
// exactly (or more strictly than) secureMode, is not "tightened": there was
// nothing loose to fix. The stat does not follow a symlink, because the rename
// below replaces the LINK, so following one would report on an inode forgectl
// never writes.
//
// Every error here carries paths only — data is never interpolated into any
// error string.
func writeAtomic(target Target, data []byte) (tightened bool, err error) {
	priorMode, _, hadPrior, statErr := target.dir.lstat(target.base)
	if statErr != nil {
		return false, fmt.Errorf("stat %s: %w", target.Rel(), statErr)
	}

	tmp, tmpName, err := target.dir.createTemp(tempPrefix)
	if err != nil {
		return false, fmt.Errorf("create a temp file beside %s: %w", target.Rel(), err)
	}
	cleanup := func() {
		_ = tmp.Close()
		_ = target.dir.remove(tmpName)
	}

	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return false, fmt.Errorf("write %s: %w", target.Rel(), err)
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return false, fmt.Errorf("sync %s: %w", target.Rel(), err)
	}
	if err := tmp.Close(); err != nil {
		_ = target.dir.remove(tmpName)
		return false, fmt.Errorf("close %s: %w", target.Rel(), err)
	}

	if err := target.dir.rename(tmpName, target.base); err != nil {
		_ = target.dir.remove(tmpName)
		return false, fmt.Errorf("rename into place %s: %w", target.Rel(), err)
	}

	tightened = hadPrior && priorMode&^os.FileMode(secureMode) != 0
	return tightened, nil
}
