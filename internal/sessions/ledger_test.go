package sessions

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// TestReadLedgerQuotesAndCapsThePath is the library-package half of
// forgectl#844: "open ledger %s: %w" printed the path raw beside the wrapped
// *PathError, so the root backstop capped the copy inside the error but only
// escaped the %s one. Both copies are now quoted and capped at the site.
//
// Mutation that turns it red: print path with a bare %s again in ReadLedger.
func TestReadLedgerQuotesAndCapsThePath(t *testing.T) {
	// One over-long component makes the open fail with a name-too-long error,
	// which ReadLedger does not treat as a missing file.
	path := filepath.Join(t.TempDir(), strings.Repeat("a", 2*termsafe.PathEchoMaxRunes)+"\x1b[31m", "sessions.jsonl")
	_, _, err := ReadLedger(path)
	if err == nil {
		t.Fatal("ReadLedger on an over-long path returned no error")
	}
	msg := err.Error()
	if strings.Contains(msg, path) || strings.Contains(msg, "\x1b") {
		t.Errorf("the ledger path reached the message raw: %q", msg)
	}
	if !strings.HasPrefix(msg, "open ledger "+termsafe.QuotePath(path)+": ") {
		t.Errorf("message %q does not lead with the capped, quoted path", msg)
	}
}
