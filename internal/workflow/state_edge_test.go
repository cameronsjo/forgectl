package workflow

import (
	"os"
	"strings"
	"testing"
)

// TestLoadState_RefusesNewerSchema pins the schema-version gate: a sidecar
// written by a future binary (schema > StateSchema) must be refused outright,
// not partially decoded and misread — mirrors the DSL's own version gate.
func TestLoadState_RefusesNewerSchema(t *testing.T) {
	dir := redirectStateDir(t)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}
	path, err := StatePath("futuristic")
	if err != nil {
		t.Fatalf("StatePath: %v", err)
	}
	body := "schema = 99\nworkflow = \"futuristic\"\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write state: %v", err)
	}

	_, ok, err := LoadState("futuristic")
	if err == nil || !strings.Contains(err.Error(), "newer than this binary understands") {
		t.Fatalf("expected a newer-schema refusal, got ok=%v err=%v", ok, err)
	}
}

// TestResumeFrom_ZeroStepPlan: a workflow with no steps has nothing to skip and
// nothing to run. ResumeFrom must report the plan already complete (its loop
// bound is len(plan.Steps), so it must terminate at 0 rather than looping or
// panicking on an empty slice).
func TestResumeFrom_ZeroStepPlan(t *testing.T) {
	plan := Plan{Name: "empty"}
	if got := ResumeFrom(RunState{}, plan); got != 0 {
		t.Fatalf("ResumeFrom(empty plan) = %d, want 0 (0 == len(Steps), i.e. already complete)", got)
	}
	// A non-empty prior state against an empty plan is a mismatched-plan
	// scenario a caller shouldn't hit in practice (the definition-hash guard
	// catches an edited file first), but ResumeFrom itself must still terminate
	// cleanly rather than index out of range.
	prior := RunState{Steps: []StepState{{Index: 0, InputHash: "sha256:stale"}}}
	if got := ResumeFrom(prior, plan); got != 0 {
		t.Fatalf("ResumeFrom(empty plan, stale prior) = %d, want 0", got)
	}
}

// TestMissingResumeExport_ZeroStepPlan: an empty plan references no export, so
// the guard must report no missing export rather than panicking on an
// out-of-range resumeFrom or an empty registry walk.
func TestMissingResumeExport_ZeroStepPlan(t *testing.T) {
	reg := testRegistry(t)
	plan := Plan{Name: "empty"}
	if _, _, missing := MissingResumeExport(plan, 0, reg); missing {
		t.Error("an empty plan must never report a missing export")
	}
}
