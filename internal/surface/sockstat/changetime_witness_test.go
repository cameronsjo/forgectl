// The test needs a platform whose changeTime reads the stat: Darwin and Linux.
// Every other unix returns zero by contract (changetime_other.go), and has no
// statWithChangeTime fixture, so the test is excluded there rather than faked.
//go:build linux || darwin

package sockstat

import (
	"testing"

	"github.com/cameronsjo/forgectl/internal/surface/backend"
)

// TestFillCarriesTheChangeTimeAsASecondWitness pins the field cmux and herdr
// gained, and pins it as the PROPERTY rather than as a value.
//
// Asserting that ChangedAtUnixNano is non-zero would pass for a Fill that wrote
// a constant. What matters is that two sockets differing ONLY in change time
// fingerprint differently — which is the whole reason to read it, since cmux and
// herdr had nothing but the inode before it.
//
// The inode is deliberately held equal here. That is the case the second witness
// exists for: a server that rebinds and happens to land on the same inode number
// is exactly when the first witness says nothing.
func TestFillCarriesTheChangeTimeAsASecondWitness(t *testing.T) {
	const inode = 4242
	base := backend.IncarnationInput{Endpoint: "/tmp/herdr.sock", Version: "herdr/20"}

	first := base
	Fill(&first, fakeInfo{sys: statWithChangeTime(inode, 1_000)})
	if first.ChangedAtUnixNano == 0 {
		t.Fatal("Fill read no change time; cmux and herdr gained no second witness")
	}

	second := base
	Fill(&second, fakeInfo{sys: statWithChangeTime(inode, 2_000)})

	if first.Inode != second.Inode {
		t.Fatalf("the fixtures differ in inode (%d vs %d); this test would pass for the "+
			"wrong reason", first.Inode, second.Inode)
	}

	a, err := backend.Fingerprint(first)
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}
	b, err := backend.Fingerprint(second)
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}
	if a.Matches(b) {
		t.Error("two sockets differing only in change time fingerprinted identically; " +
			"a restart onto the same inode would go undetected")
	}
}
