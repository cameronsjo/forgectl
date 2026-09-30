package clean

import (
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// TestDeleteRefusalQuotesAndCapsTheTarget is #847 item 2: the delete
// refusals printed the target path raw, so only the root handler escaped it,
// and nothing capped it.
//
// Mutation that turns it red: print target with a bare %s again in the
// ".git is never a reclaim target" refusal.
func TestDeleteRefusalQuotesAndCapsTheTarget(t *testing.T) {
	target := "/r/" + strings.Repeat("a", 2*termsafe.PathEchoMaxRunes) + "\u202e/.git/node_modules"
	err := (&Client{}).delete("/r", target)
	if err == nil {
		t.Fatal("delete of a .git path returned no error")
	}
	msg := err.Error()
	if strings.Contains(msg, target) || strings.Contains(msg, "\u202e") {
		t.Errorf("the target reached the message raw: %q", msg)
	}
	if want := "refusing to delete " + termsafe.QuotePath(target) + ": "; !strings.HasPrefix(msg, want) {
		t.Errorf("message %q does not lead with the capped, quoted target", msg)
	}
}
