//go:build unix && !aix

package cli

import "testing"

// TestHerdrLockSupported_OnUnix: config.WithFileLockNotify takes a real flock
// here, so organize --apply must not refuse on the lock's account.
func TestHerdrLockSupported_OnUnix(t *testing.T) {
	if !herdrLockSupported {
		t.Error("herdrLockSupported = false on a Unix build, want true")
	}
}
