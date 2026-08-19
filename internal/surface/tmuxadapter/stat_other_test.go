// Mirrors stat_other.go's own `//go:build !unix` constraint exactly: a test
// for the non-unix stub belongs behind the same tag as the code it tests, or
// it either fails to compile on unix (where syscall.Stat_t-shaped assertions
// would not apply) or silently never runs on the platform it is meant for. A
// GOOS=windows compile check catches neither — only running the assertions
// under this tag proves the fail-closed contract stat_other.go documents.
//go:build !unix

package tmuxadapter

import (
	"testing"

	"github.com/cameronsjo/forgectl/internal/surface/backend"
)

// TestOwnerUIDAlwaysDeclinesOnNonUnix pins the ok=false half of the fail-closed
// design: the caller treats an unreadable owner as "cannot assert", never as
// "owned by us", so a stub that returned ok=true for any input would turn the
// socket-directory ownership check into one that silently passes when it
// cannot actually see the owner.
func TestOwnerUIDAlwaysDeclinesOnNonUnix(t *testing.T) {
	uid, ok := ownerUID(nil)
	if ok {
		t.Error("ownerUID reported ok=true on a platform with no Stat_t to read it from")
	}
	if uid != 0 {
		t.Errorf("ownerUID = %d, want 0 alongside ok=false", uid)
	}
}

// TestFillStatLeavesTheIncarnationInputZeroOnNonUnix pins the other half:
// Fingerprint requires a non-zero Inode, so a fillStat that left it untouched
// (rather than accidentally populating it from something else) is what makes
// fingerprinting refuse instead of producing a digest that could coincidentally
// match across a server restart.
func TestFillStatLeavesTheIncarnationInputZeroOnNonUnix(t *testing.T) {
	in := backend.IncarnationInput{Endpoint: "/tmp/tmux-501/default", Version: "tmux 3.7b"}
	before := in

	fillStat(&in, nil)

	if in != before {
		t.Errorf("fillStat mutated the incarnation input on a platform with no stat to read: got %+v, want unchanged %+v", in, before)
	}
	if in.Inode != 0 || in.Device != 0 {
		t.Errorf("fillStat left a non-zero Inode/Device (%d/%d) — Fingerprint would no longer refuse", in.Inode, in.Device)
	}
}
