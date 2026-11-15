//go:build !unix

package exec

import "os"

// ownedByCurrentUser has no owner to read off unix, so it refuses, like the
// ownership gate in internal/rexbin does. Nothing is lost on a supported
// platform: Rex, the only user of a launch script, runs on macOS, and on
// Windows Go reports every directory as mode 0777, so the mode check in
// NewLaunchScript already refuses every call there.
func ownedByCurrentUser(os.FileInfo) bool { return false }
