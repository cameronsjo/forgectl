package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	branchpkg "github.com/cameronsjo/forgectl/internal/branch"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// hostileRef is a refname git accepts that a hostile remote could advertise:
// a bidi override and a C1 CSI (U+009B) around a marker.
const hostileRef = "feat/\u202eMARKER\u009b2J"

// assertTerminalSafeLines: no line of out carries a raw unsafe rune.
func assertTerminalSafeLines(t *testing.T, out string) {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		for _, r := range line {
			if termsafe.IsUnsafeTerminalRune(r) {
				t.Fatalf("line %q carries raw rune %U", line, r)
			}
		}
	}
	if !strings.Contains(out, "MARKER") {
		t.Fatalf("output %q lost the name entirely; want it shown escaped", out)
	}
}

// TestPrintPruneResults_EscapesNamesAndErrors: prune's result lines print
// straight to stdout, past the root error handler, so the refname and the
// error must be escaped here (#658).
func TestPrintPruneResults_EscapesNamesAndErrors(t *testing.T) {
	for _, r := range []branchpkg.PruneResult{
		{Name: hostileRef, Err: errors.New("cause \u009b2J")},
		{Name: hostileRef, Skipped: true, Reason: "not safe \u202e"},
		{Name: hostileRef, Deleted: true},
	} {
		var out bytes.Buffer
		printPruneResults(&out, []branchpkg.PruneResult{r})
		assertTerminalSafeLines(t, out.String())
	}
}

func TestPrintBranchGroup_EscapesNames(t *testing.T) {
	var out bytes.Buffer
	printBranchGroup(&out, "safe-to-delete", []branchpkg.Classification{{
		Info: branchpkg.Info{Name: hostileRef}, Reason: "merged \u202e",
	}})
	assertTerminalSafeLines(t, out.String())
}
