package workflow

import (
	"testing"

	"github.com/cameronsjo/forgectl/internal/launch"
	"github.com/cameronsjo/forgectl/internal/quarantine"
)

// testRegistry mirrors the prod wiring (internal/cli's stepContributions):
// engine builtins ∪ quarantine's strip ∪ launch's stub. Tests exercise the
// same merged vocabulary BuildPlan and the Executor receive at runtime, so
// plan-time deferral of module exports (launch's ${review}) is covered here
// exactly as shipped.
func testRegistry(t *testing.T) StepRegistry {
	t.Helper()
	reg, err := NewRegistry(quarantine.Steps(nil), launch.Steps())
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	return reg
}

// TestNewRegistry_CollisionErrors pins the no-silent-last-wins contract:
// a contribution shadowing a builtin, and two contributions claiming the
// same verb, must both error.
func TestNewRegistry_CollisionErrors(t *testing.T) {
	builtinShadow := StepRegistry{"run": {}}
	if _, err := NewRegistry(builtinShadow); err == nil {
		t.Error("expected an error for a module verb shadowing the run builtin")
	}

	a := StepRegistry{"deploy": {}}
	b := StepRegistry{"deploy": {}}
	if _, err := NewRegistry(a, b); err == nil {
		t.Error("expected an error for two modules claiming the deploy verb")
	}
}

// TestNewRegistry_MergesContributions verifies the merged registry carries
// builtins and contributions together, exports intact.
func TestNewRegistry_MergesContributions(t *testing.T) {
	reg := testRegistry(t)
	for _, verb := range []string{"run", "worktree", "clone", "teardown", "collect", "strip", "launch"} {
		if _, ok := reg[verb]; !ok {
			t.Errorf("merged registry missing verb %q", verb)
		}
	}
	if got := reg["launch"].Exports; len(got) != 1 || got[0] != "review" {
		t.Errorf("launch exports = %v, want [review] — plan-time deferral depends on it", got)
	}
}
