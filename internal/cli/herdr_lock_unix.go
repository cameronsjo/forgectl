//go:build unix

package cli

// herdrLockSupported says whether config.WithFileLockNotify really serializes
// organize --apply runs here. On Unix it takes an flock.
var herdrLockSupported = true
