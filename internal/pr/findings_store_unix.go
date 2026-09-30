//go:build unix

package pr

import (
	"fmt"
	"os"
	"syscall"
)

// findingsStoreOwner is the uid the findings store must belong to, a seam so
// a test can stand in for another user without root.
var findingsStoreOwner = os.Geteuid

// verifyFindingsStore refuses a findings store that is not private to this
// user before cleanup decides anything from it (forgectl#680): one owned by
// another uid, or one that grants group or world write. Either lets someone
// else plant or swap entries in the store that cleanup would then judge and
// remove. It is the privdir posture (owner is the euid, checked on the pinned
// descriptor), except that a broad store is refused rather than narrowed:
// cleanup is not the store's creator, and a chmod is not its call.
//
// It stats the opened handle, not the path, so it checks the directory
// cleanup will actually work in; a symlinked store is checked at its target.
// The refusal is categorical and never names the path.
func verifyFindingsStore(store *os.Root) error {
	info, err := store.Stat(".")
	if err != nil {
		return fmt.Errorf("%w: it cannot be inspected", errFindingsStoreUnsafe)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("%w: its owner cannot be read", errFindingsStoreUnsafe)
	}
	if int(st.Uid) != findingsStoreOwner() {
		return fmt.Errorf("%w: it is owned by another user", errFindingsStoreUnsafe)
	}
	if info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%w: it is group- or world-writable; remove that permission (chmod go-w) and retry", errFindingsStoreUnsafe)
	}
	return nil
}
