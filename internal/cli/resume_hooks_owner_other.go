//go:build !unix

package cli

import "io/fs"

// fileOwner reports no owner off unix; the watcher is macOS-only.
func fileOwner(fs.FileInfo) (int, bool) { return 0, false }
