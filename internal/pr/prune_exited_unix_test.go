//go:build unix

package pr

import (
	"context"
	"strings"
	"testing"
	"time"
)

// An exited tmux server's leftover socket still refuses the ref-bearing file,
// and the refusal names that state's own remedy rather than the generic
// "could not be read", whose `tmux list-windows -a` only prints "no server
// running" (forgectl#805, #815 item 4).
//
// Mutation that turns it red: set prune's refusal reason to the fixed
// generic text again instead of windowListUnreadable(listErr).
func TestPrune_AnExitedServerRefusesWithItsOwnRemedy(t *testing.T) {
	c := pruneClient(t, deadSocketServer(t))
	future := []byte(`{"workspace":"/tmp/forgectl-workflow-x","ref":"o/r#1","agent":"claude",` +
		`"createdAt":"2026-09-12T00:00:00Z","version":3,"phase":"active","revision":4}` + "\n")
	withRef := seedAside(t, c, "o-r-1-1.json", 60*24*time.Hour, future)

	report, err := c.Prune(context.Background(), defaultPruneOpts())
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	var got *PruneItem
	for i := range report.Items {
		if report.Items[i].Path == withRef {
			got = &report.Items[i]
		}
	}
	if got == nil {
		t.Fatalf("no report item for %s", withRef)
	}
	if got.Outcome != pruneOutcomeRefused {
		t.Fatalf("outcome = %q, want %q — a refused connect is not an absent window", got.Outcome, pruneOutcomeRefused)
	}
	if !strings.Contains(got.Reason, "server has exited") || strings.Contains(got.Reason, "could not be read") {
		t.Errorf("reason = %q, want the exited-server remedy", got.Reason)
	}
}
