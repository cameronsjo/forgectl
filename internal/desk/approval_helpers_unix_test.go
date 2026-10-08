//go:build unix

package desk

import (
	"strings"
	"testing"
)

// anySHA is a well-formed sha256 that names no item, for calls whose
// refusal must come from something other than the hash's shape.
var anySHA = strings.Repeat("0", 64)

// queuedSHA is the hash fixed when name was queued: the hash the operator
// approves, which Claim requires in full.
func queuedSHA(t *testing.T, d *Desk, name string) string {
	t.Helper()
	m, ok, err := d.readMeta(DirPending, name)
	if err != nil || !ok || !ValidSHA256(m.SHA256) {
		t.Fatalf("pending meta for %s: ok=%v err=%v sha=%q; scan the desk first", name, ok, err, m.SHA256)
	}
	return m.SHA256
}
