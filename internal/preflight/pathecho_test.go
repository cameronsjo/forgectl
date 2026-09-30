package preflight

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// TestReadDocumentParseErrorQuotesAndCapsThePath is #847 item 6: a settings
// file that fails to parse printed its path raw beside the wrapped error.
//
// Mutation that turns it red: print path with a bare %s again in
// ReadDocument's "parse" error.
func TestReadDocumentParseErrorQuotesAndCapsThePath(t *testing.T) {
	dir := filepath.Join(t.TempDir(), strings.Repeat("a", 200), strings.Repeat("b", 200), strings.Repeat("c", 200))
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "rlo\u202esettings.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := ReadDocument(path)
	if err == nil {
		t.Fatal("ReadDocument on malformed JSON returned no error")
	}
	msg := err.Error()
	if strings.Contains(msg, path) || strings.Contains(msg, "\u202e") {
		t.Errorf("the settings path reached the message raw: %q", msg)
	}
	if want := "parse " + termsafe.QuotePath(path) + ": "; !strings.HasPrefix(msg, want) {
		t.Errorf("message %q does not lead with the capped, quoted path", msg)
	}
}
