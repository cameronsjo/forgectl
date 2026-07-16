package workflow

import (
	"os"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/config"
)

// redirectStateDir points os.UserConfigDir (via config.WorkflowStateDir) at a
// temp HOME on both macOS and Linux, and returns the resolved state directory.
func redirectStateDir(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("XDG_CONFIG_HOME", tmp)
	dir, err := config.WorkflowStateDir()
	if err != nil {
		t.Fatalf("WorkflowStateDir: %v", err)
	}
	return dir
}

func TestWriteLoadClearState_RoundTrip(t *testing.T) {
	redirectStateDir(t)

	st := RunState{
		Schema:         StateSchema,
		Workflow:       "demo",
		RunID:          "20260715T000000Z-abcd",
		DefinitionHash: DefinitionHash([]byte("hello")),
		StartedAt:      "2026-07-15T00:00:00Z",
		UpdatedAt:      "2026-07-15T00:00:01Z",
		Steps: []StepState{
			{Index: 0, Uses: "run", InputHash: "sha256:aa", CompletedAt: "2026-07-15T00:00:01Z"},
		},
	}
	if err := WriteState(st); err != nil {
		t.Fatalf("WriteState: %v", err)
	}

	got, ok, err := LoadState("demo")
	if err != nil || !ok {
		t.Fatalf("LoadState: ok=%v err=%v", ok, err)
	}
	if got.Workflow != st.Workflow || got.DefinitionHash != st.DefinitionHash || len(got.Steps) != 1 {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	if got.Steps[0].Uses != "run" || got.Steps[0].InputHash != "sha256:aa" {
		t.Errorf("step round-trip mismatch: %+v", got.Steps[0])
	}

	// Sidecar must be private (0o600) — it can carry a workspace path and drives
	// what a resume executes.
	path, _ := StatePath("demo")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat state file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("state file perm = %o, want 600", perm)
	}

	if err := ClearState("demo"); err != nil {
		t.Fatalf("ClearState: %v", err)
	}
	if _, ok, _ := LoadState("demo"); ok {
		t.Error("state should be gone after ClearState")
	}
}

func TestLoadState_MissingIsNotAnError(t *testing.T) {
	redirectStateDir(t)
	st, ok, err := LoadState("never-run")
	if err != nil {
		t.Fatalf("missing state must not error, got %v", err)
	}
	if ok {
		t.Errorf("missing state must report ok=false, got %+v", st)
	}
}

func TestClearState_MissingIsNoOp(t *testing.T) {
	redirectStateDir(t)
	if err := ClearState("never-run"); err != nil {
		t.Errorf("clearing absent state must be a no-op, got %v", err)
	}
}

func TestStatePath_RejectsTraversal(t *testing.T) {
	redirectStateDir(t)
	for _, bad := range []string{"../evil", "a/b", "..", ""} {
		if _, err := StatePath(bad); err == nil {
			t.Errorf("StatePath(%q) should be rejected", bad)
		}
	}
}

func TestHashPlanStep_StableAndSensitive(t *testing.T) {
	base := PlanStep{Uses: "run", Cmd: "echo", Args: []string{"a", "b"}}
	if HashPlanStep(base) != HashPlanStep(base) {
		t.Fatal("hash must be stable for identical inputs")
	}
	changed := base
	changed.Args = []string{"a", "c"}
	if HashPlanStep(base) == HashPlanStep(changed) {
		t.Error("a changed arg must change the hash")
	}
	// The NUL separator must keep ("ab","") from colliding with ("a","b").
	x := PlanStep{Uses: "run", Cmd: "ab"}
	y := PlanStep{Uses: "run", Cmd: "a", Repo: "b"}
	if HashPlanStep(x) == HashPlanStep(y) {
		t.Error("field boundaries must not collide")
	}
}

func TestResumeFrom(t *testing.T) {
	plan := Plan{Steps: []PlanStep{
		{Uses: "run", Cmd: "a"},
		{Uses: "run", Cmd: "b"},
		{Uses: "run", Cmd: "c"},
	}}
	hashOf := func(i int) string { return HashPlanStep(plan.Steps[i]) }

	cases := []struct {
		name  string
		prior RunState
		want  int
	}{
		{
			name:  "nothing checkpointed",
			prior: RunState{},
			want:  0,
		},
		{
			name: "first two done",
			prior: RunState{Steps: []StepState{
				{Index: 0, InputHash: hashOf(0)},
				{Index: 1, InputHash: hashOf(1)},
			}},
			want: 2,
		},
		{
			name: "all done",
			prior: RunState{Steps: []StepState{
				{Index: 0, InputHash: hashOf(0)},
				{Index: 1, InputHash: hashOf(1)},
				{Index: 2, InputHash: hashOf(2)},
			}},
			want: 3,
		},
		{
			name: "input hash changed at step 1 forces re-run from 1",
			prior: RunState{Steps: []StepState{
				{Index: 0, InputHash: hashOf(0)},
				{Index: 1, InputHash: "sha256:stale"},
				{Index: 2, InputHash: hashOf(2)},
			}},
			want: 1,
		},
		{
			name: "gap at step 1 stops the skip even though step 2 is present",
			prior: RunState{Steps: []StepState{
				{Index: 0, InputHash: hashOf(0)},
				{Index: 2, InputHash: hashOf(2)},
			}},
			want: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ResumeFrom(tc.prior, plan); got != tc.want {
				t.Errorf("ResumeFrom = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestMissingResumeExport(t *testing.T) {
	reg := testRegistry(t)

	// worktree exports ${workspace}; the run step consumes it.
	exportPlan := Plan{Steps: []PlanStep{
		{Uses: "worktree", Repo: "r"},
		{Uses: "run", Cmd: "echo", Args: []string{"${workspace}"}},
		{Uses: "teardown"},
	}}

	// Resuming from step 1 skips the worktree that produced ${workspace}: refuse.
	if _, idx, missing := MissingResumeExport(exportPlan, 1, reg); !missing || idx != 1 {
		t.Errorf("expected a missing-export refusal at step index 1, got missing=%v idx=%d", missing, idx)
	}
	// Resuming from step 0 re-runs the worktree, so the export is produced fresh.
	if _, _, missing := MissingResumeExport(exportPlan, 0, reg); missing {
		t.Error("resuming from 0 re-produces ${workspace}; must not refuse")
	}

	// A plan of independent run steps never depends on a skipped export.
	independent := Plan{Steps: []PlanStep{
		{Uses: "run", Cmd: "a"},
		{Uses: "run", Cmd: "b"},
		{Uses: "run", Cmd: "c"},
	}}
	if _, _, missing := MissingResumeExport(independent, 2, reg); missing {
		t.Error("independent run steps must never trip the missing-export guard")
	}
}

func TestResumeRecorder_KeepsPrefixAndAppends(t *testing.T) {
	redirectStateDir(t)

	prior := RunState{
		Schema:         StateSchema,
		Workflow:       "demo",
		RunID:          "run-1",
		DefinitionHash: "sha256:def",
		StartedAt:      "2026-07-15T00:00:00Z",
		Steps: []StepState{
			{Index: 0, Uses: "run", InputHash: "sha256:h0"},
			{Index: 1, Uses: "run", InputHash: "sha256:h1"},
			{Index: 2, Uses: "run", InputHash: "sha256:h2"},
		},
	}

	rec := NewResumeRecorder(prior, 1, time.Now())
	if err := rec.Record(1, PlanStep{Uses: "run", Cmd: "b"}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	got, ok, err := LoadState("demo")
	if err != nil || !ok {
		t.Fatalf("LoadState: ok=%v err=%v", ok, err)
	}
	if got.RunID != "run-1" || got.DefinitionHash != "sha256:def" {
		t.Errorf("resume must preserve prior run identity, got runID=%q defHash=%q", got.RunID, got.DefinitionHash)
	}
	if len(got.Steps) != 2 {
		t.Fatalf("expected prefix [step 0] + newly recorded [step 1], got %d steps: %+v", len(got.Steps), got.Steps)
	}
	if got.Steps[0].Index != 0 || got.Steps[1].Index != 1 {
		t.Errorf("steps out of order: %+v", got.Steps)
	}
	// The newly recorded step's hash must reflect the plan-time step it was given.
	if got.Steps[1].InputHash != HashPlanStep(PlanStep{Uses: "run", Cmd: "b"}) {
		t.Errorf("recorded input hash mismatch: %q", got.Steps[1].InputHash)
	}
}
