//go:build !unix || aix

package cli

import "testing"

// TestHerdrLockSupported_OffUnix: config.WithFileLockNotify serializes nothing
// here, so organize --apply must refuse rather than run unlocked (#732).
func TestHerdrLockSupported_OffUnix(t *testing.T) {
	if herdrLockSupported {
		t.Error("herdrLockSupported = true off Unix, want false")
	}
}
