//go:build !unix

package exec

import "os"

// ownedByCurrentUser has no owner to read off unix, so it cannot refuse: the
// mode check in NewLaunchScript stands alone there. Rex, the only user of a
// launch script, runs on macOS.
func ownedByCurrentUser(os.FileInfo) bool { return true }
