package pr

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// TestRecordTextIsCapped is #934 for pr's record and report fields: an error,
// a refusal or a repair reason is escaped AND bounded at the source, so a
// multi-megabyte stderr reaches neither --json, the breadcrumb, nor the repair
// log whole.
//
// Mutation that turns it red: make recordText return termsafe.SafeLine(s).
func TestRecordTextIsCapped(t *testing.T) {
	got := recordText("HEAD" + strings.Repeat("x\u202e", 50_000))
	if n := utf8.RuneCountInString(got); n > recordTextMaxRunes+utf8.RuneCountInString(termsafe.TruncatedMarker) {
		t.Errorf("recordText kept %d runes; want at most %d plus the marker", n, recordTextMaxRunes)
	}
	if !strings.HasPrefix(got, "HEAD") || !strings.HasSuffix(got, termsafe.TruncatedMarker) {
		t.Errorf("recordText = %.80q…; want the head kept and the truncation marker", got)
	}
	if got := recordText("short \u202e"); got != `short \u202e` {
		t.Errorf("recordText(short) = %q; want it escaped and whole", got)
	}
}
