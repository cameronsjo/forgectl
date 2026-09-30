package sessions

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// hostileLedgerDir returns a directory under t.TempDir() whose path runs past
// termsafe.PathEchoMaxRunes and holds a bidi override, so a raw print of it
// is both uncapped and unescaped.
func hostileLedgerDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), strings.Repeat("a", 200), strings.Repeat("b", 200), strings.Repeat("c", 200), "rlo\u202eld")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestReadLedgerQuotesAndCapsThePath is the library-package half of
// forgectl#844: "open ledger %s: %w" printed the path raw beside the wrapped
// *PathError, so the root backstop capped the copy inside the error but only
// escaped the %s one. Both copies are now quoted and capped at the site.
//
// The open fails because a path element is a regular file (ENOTDIR), which
// every unix reports whatever its name-length limit (#847 item 7). Windows
// reports that case as ERROR_PATH_NOT_FOUND, which os.IsNotExist accepts, so
// ReadLedger reads it as a missing ledger and returns no error to check.
//
// Mutation that turns it red: print path with a bare %s again in ReadLedger.
func TestReadLedgerQuotesAndCapsThePath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows reports a non-directory path element as not-exist, which ReadLedger treats as an empty ledger")
	}
	notADir := filepath.Join(hostileLedgerDir(t), "file")
	if err := os.WriteFile(notADir, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(notADir, "sessions.jsonl")
	_, _, err := ReadLedger(path)
	if err == nil {
		t.Fatal("ReadLedger under a regular file returned no error")
	}
	msg := err.Error()
	if strings.Contains(msg, path) || strings.Contains(msg, "\u202e") {
		t.Errorf("the ledger path reached the message raw: %q", msg)
	}
	if !strings.HasPrefix(msg, "open ledger "+termsafe.QuotePath(path)+": ") {
		t.Errorf("message %q does not lead with the capped, quoted path", msg)
	}
}

// TestReadLedgerScanErrorQuotesAndCapsThePath is #847 item 6: a read failure
// after a successful open printed the path raw. Opening a directory succeeds
// and reading it fails, which reaches the scan error.
//
// Mutation that turns it red: print path with a bare %s again in the
// "scan ledger" error.
func TestReadLedgerScanErrorQuotesAndCapsThePath(t *testing.T) {
	path := hostileLedgerDir(t)
	_, _, err := ReadLedger(path)
	if err == nil {
		t.Fatal("ReadLedger on a directory returned no error")
	}
	msg := err.Error()
	if strings.Contains(msg, path) || strings.Contains(msg, "\u202e") {
		t.Errorf("the ledger path reached the message raw: %q", msg)
	}
	if !strings.HasPrefix(msg, "scan ledger "+termsafe.QuotePath(path)+": ") {
		t.Errorf("message %q does not lead with the capped, quoted path", msg)
	}
}
