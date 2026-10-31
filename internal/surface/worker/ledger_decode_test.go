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
	// The name comes from the file, so it reaches the error escaped and short.
	hostile := []byte(`{"version":1,"repo":"/r","session":"s","workers":[{"name":"w","branch":"b","stage":"launched","started_at":"2026-10-31T00:00:00Z","x\u001b]52;c;aGk=\u0007` + strings.Repeat("y", 200) + `":"x"}]}`)
	_, err = decodeLedger(hostile, "/r", "s")
	if err == nil || strings.ContainsAny(err.Error(), "\x1b\x07") || strings.Contains(err.Error(), strings.Repeat("y", 100)) {
		t.Fatalf("a hostile field name reached the error raw or long: %q", err)
	}
	if _, err := decodeLedger([]byte(`{"version":1,`), "/r", "s"); err == nil || strings.Contains(err.Error(), "newer forgectl") {
		t.Fatalf("a truncated file reads as %v; only an unknown field names a newer forgectl", err)
	}
}
