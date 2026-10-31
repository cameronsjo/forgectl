package worker

import (
	"errors"
	"strings"
	"testing"
)

// TestDecodeLedgerNamesANewerForgectl: fields are added without a version
// change, so a ledger holding one this build does not know stays refused
// (decoding is strict) and the error says to upgrade, naming the field.
func TestDecodeLedgerNamesANewerForgectl(t *testing.T) {
	data := []byte(`{"version":1,"repo":"/r","session":"s","workers":[{"name":"w","branch":"b","stage":"launched","started_at":"2026-10-31T00:00:00Z","from_the_future":"x"}]}`)
	_, err := decodeLedger(data, "/r", "s")
	if !errors.Is(err, ErrLedgerUnreadable) {
		t.Fatalf("err = %v, want ErrLedgerUnreadable", err)
	}
	if msg := err.Error(); !strings.Contains(msg, "newer forgectl") || !strings.Contains(msg, `"from_the_future"`) {
		t.Fatalf("err = %q, want it to name the field and a newer forgectl", msg)
	}
	if _, err := decodeLedger([]byte(`{"version":1,`), "/r", "s"); err == nil || strings.Contains(err.Error(), "newer forgectl") {
		t.Fatalf("a truncated file reads as %v; only an unknown field names a newer forgectl", err)
	}
}
