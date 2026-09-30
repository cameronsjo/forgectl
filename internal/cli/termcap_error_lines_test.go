package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	branchpkg "github.com/cameronsjo/forgectl/internal/branch"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// TestErrorLinesAreCapped is #934's follow-up to #927: termsafe.Error escapes
// an error and caps the paths inside it, but not its whole text, so a text
// printer that renders one routes it through safeText rather than a bare %v.
// printPruneResults stands for the eight sites that printed one directly; the
// source pin (TestTextPrintersUseCappedHelpers in internal/termsafe) flags any
// termsafe.Error handed straight to a fmt print call.
//
// Mutation that turns it red: print r.Err in printPruneResults as
// `%v`, termsafe.Error(r.Err).
func TestErrorLinesAreCapped(t *testing.T) {
	var out bytes.Buffer
	printPruneResults(&out, []branchpkg.PruneResult{{
		Name: "feature",
		Err:  errors.New("HEAD" + strings.Repeat("x\u202e", 50_000)),
	}})
	line := strings.TrimRight(out.String(), "\n")
	if n := utf8.RuneCountInString(line); n > textMaxRunes+64 {
		t.Errorf("FAILED line is %d runes; want the error capped at %d", n, textMaxRunes)
	}
	if !strings.HasPrefix(line, "FAILED  feature: HEAD") || !strings.HasSuffix(line, termsafe.TruncatedMarker) {
		t.Errorf("FAILED line = %.120q…; want the head kept and the truncation marker", line)
	}
}
