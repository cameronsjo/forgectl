//go:build !unix || aix

package cli

// herdrLockSupported says whether config.WithFileLockNotify really serializes
// organize --apply runs here. Off Unix it runs its function with no lock, and
// on AIX it refuses, so --apply refuses up front instead of running two
// organize runs against one session at once (#732).
var herdrLockSupported = false
