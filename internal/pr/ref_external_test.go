package pr_test

// The locality boundary seen from OUTSIDE the package, which is where it
// actually has to hold: every consumer of pr.Ref (internal/cli,
// internal/review) can build a Ref literal, and none of them may produce a
// local one. The in-package tests can reach the unexported flag; these cannot,
// which is exactly the point.

import (
	"testing"

	"github.com/cameronsjo/forgectl/internal/pr"
)

// TestRef_ExternalConstructionIsNeverLocal states the invariant an external
// package can verify: no Ref an outside caller can build reports IsLocal(),
// whatever its Owner spells. Ref.local is unexported, so a composite literal
// simply cannot set it — and IsLocal() gates the Codex sandbox widening
// (launchCodex) and the PostReview refusal.
func TestRef_ExternalConstructionIsNeverLocal(t *testing.T) {
	literal := pr.Ref{Owner: "local", Repo: "tools", Number: 5}
	if literal.IsLocal() {
		t.Error("a Ref literal built outside the package reported IsLocal()")
	}
	if literal.String() != "local/tools#5" {
		t.Errorf("String() = %q, want local/tools#5", literal.String())
	}

	parsed, err := pr.ParseRef("local/tools#5")
	if err != nil {
		t.Fatalf("ParseRef rejected a real owner named %q: %v", "local", err)
	}
	if parsed.IsLocal() {
		t.Error("pr.ParseRef produced a local Ref from an external string")
	}
	if err := pr.CheckAgentForRef("codex", parsed); err == nil {
		t.Error("CheckAgentForRef(codex) must still refuse a remote ref owned by \"local\"")
	}
}
