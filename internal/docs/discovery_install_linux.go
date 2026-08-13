//go:build linux

package docs

import "golang.org/x/sys/unix"

// installNoReplace renames tempName to finalName within one pinned directory,
// failing with EEXIST rather than replacing an existing final record.
//
// It returns the raw errno. The single translation into ErrGenerationCollision
// lives in installRecord, so the serving loop's retry predicate cannot drift
// apart from what one platform happens to return.
//
// There is no fallback to unix.Renameat. A kernel too old for RENAME_NOREPLACE
// (pre-3.15, or a filesystem that does not implement it) fails publication
// instead, because plain rename SUCCEEDS at overwriting a live sibling's
// record — a silent wrong answer in place of a loud missing feature.
func installNoReplace(dirFD int, tempName, finalName string) error {
	return unix.Renameat2(dirFD, tempName, dirFD, finalName, unix.RENAME_NOREPLACE)
}
