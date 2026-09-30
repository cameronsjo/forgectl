// write.go is the atomic write path every env command that mutates a file
// goes through: a temp file created 0600 from the start (there is no chmod
// window to close) inside a gitignored scratch directory beside the target,
// written, synced, then renamed into place.
package env

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// secureMode is the permission bits a .env file should carry: owner
// read/write only. The temp file is created at 0600 before umask, which can
// only narrow it further — 0600 has no group/other bits for umask to strip —
// so writeAtomic never needs an explicit chmod.
const secureMode = 0o600

// tempPrefix begins the name of the temp file writeAtomic wrote directly
// beside the target, before it wrote into a scratch directory
// (cameronsjo/forgectl#737). Nothing creates that name any more. The leftover
// scan still refuses on its scoped form (Target.envTempPrefix) and warns on
// its unscoped one, because a run of an older forgectl can have left one.
const tempPrefix = ".env-"

// envScratchPrefix begins the name of writeAtomic's scratch directory. It
// deliberately does NOT match IsEnvFileName, so a leftover cannot later be
// reached through --file without --any-file, and neither it nor
// sopsScratchPrefix (which internal/cli's __sops-edit checks on its own) is a
// prefix of the other, so no name matches both. The
// full name also carries the target's scope tag (Target.envScratchDirPrefix),
// so a leftover can be attributed to the target whose lock covers it. See
// leftover.go.
const envScratchPrefix = ".forgectl-env-"

// scratchTempPrefix begins the temp file's name inside the scratch directory.
// The directory is fresh and private to one run, so the name only has to be
// unique within it.
const scratchTempPrefix = "new."

// scratchWritten observes the scratch directory's name once the whole new
// document is written, synced and closed inside it, just before the rename.
// It is a no-op in production. Tests replace it to see, and to leave behind by
// panicking, exactly what a run killed at that point leaves: writeAtomic
// deliberately defers no cleanup, so a panic here strands the directory as
// SIGKILL would.
var scratchWritten = func(string) {}

// writeAtomic writes data to the target by creating a scratch directory in the
// SAME PINNED DIRECTORY, creating a temp file inside it, writing, syncing,
// closing, then renaming the temp file over the target's name and removing
// the directory. Everything it created is removed on any error before that
// final rename.
//
// # Why a directory, not a bare temp file
//
// The temp file holds the whole new document, secrets included, and SIGKILL,
// a crash or a power loss runs no cleanup. A bare temp file beside the target
// is then an untracked file that `git add -A` commits. The scratch directory
// carries a `*` .gitignore written before the temp file exists, so git neither
// lists nor stages what a killed run leaves (cameronsjo/forgectl#737, the same
// mechanism as the --sops work directory's, #698). The rename stays atomic
// because the directory is a child of the target's directory, so both names
// are on the same filesystem.
//
// Every filesystem call goes through target.dir, or through the scratch
// directory's own descriptor, rather than through a path. That is not
// stylistic: os.CreateTemp and os.Rename take paths and re-walk every
// component, so an intermediate directory replaced after resolution would
// redirect both the temp creation and the rename out of the repository.
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
// Teardown follows the scratch rule (scratchIgnoreRemovable): the temp file
// is unlinked, and the .gitignore and the directory go only if nothing else is
// left. If the unlink fails (EIO, a read-only remount), the document stays
// under its .gitignore, still ignored by git, and the next write's leftover
// scan refuses on it. After a successful rename the temp file is already
// gone, so a directory that will not come down then is not reported as a
// failed write; the scan names it.
//
// Every error here carries paths only — data is never interpolated into any
// error string.
func writeAtomic(target Target, data []byte) (tightened bool, err error) {
	return writeAtomicTracked(target, data, nil)
}

// writeAtomicTracked is writeAtomic, reporting the scratch directory's
// absolute path to track (when non-nil) as soon as the directory exists and
// before anything is written into it. An error from track abandons the write:
// the directory is removed and the error returned, wrapped.
func writeAtomicTracked(target Target, data []byte, track func(scratchDir string) error) (tightened bool, err error) {
	priorMode, _, hadPrior, statErr := target.dir.lstat(target.base)
	if statErr != nil {
		return false, fmt.Errorf("stat %s: %w", termsafe.QuotePath(target.Rel()), termsafe.Error(statErr))
	}

	scratch, scratchName, err := target.dir.mkScratchDir(target.envScratchDirPrefix())
	if err != nil {
		return false, fmt.Errorf("create a scratch directory beside %s: %w", termsafe.QuotePath(target.Rel()), termsafe.Error(err))
	}
	if track != nil {
		if err := track(filepath.Join(filepath.Dir(target.Abs()), scratchName)); err != nil {
			_ = target.dir.removeScratchDir(scratch, scratchName)
			return false, fmt.Errorf("write %s: %w", termsafe.QuotePath(target.Rel()), err)
		}
	}

	tmp, tmpName, err := scratch.createTemp(scratchTempPrefix)
	if err != nil {
		_ = target.dir.removeScratchDir(scratch, scratchName)
		return false, fmt.Errorf("create a temp file in the scratch directory beside %s: %w", termsafe.QuotePath(target.Rel()), termsafe.Error(err))
	}
	removeScratch := func() { _ = target.dir.removeScratchDir(scratch, scratchName, tmpName) }
	cleanup := func() {
		_ = tmp.Close()
		removeScratch()
	}

	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return false, fmt.Errorf("write %s: %w", termsafe.QuotePath(target.Rel()), termsafe.Error(err))
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return false, fmt.Errorf("sync %s: %w", termsafe.QuotePath(target.Rel()), termsafe.Error(err))
	}
	if err := tmp.Close(); err != nil {
		removeScratch()
		return false, fmt.Errorf("close %s: %w", termsafe.QuotePath(target.Rel()), termsafe.Error(err))
	}
	scratchWritten(scratchName)

	if err := target.dir.renameFrom(scratch, tmpName, target.base); err != nil {
		removeScratch()
		return false, fmt.Errorf("rename into place %s: %w", termsafe.QuotePath(target.Rel()), termsafe.Error(err))
	}
	// The temp file is renamed out, so there is nothing of this process's
	// left to unlink but the .gitignore.
	_ = target.dir.removeScratchDir(scratch, scratchName)

	tightened = hadPrior && priorMode&^os.FileMode(secureMode) != 0
	return tightened, nil
}
