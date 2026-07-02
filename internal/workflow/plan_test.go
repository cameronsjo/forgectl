package workflow

import "testing"

func TestBuildPlan_EmbeddedCleanRoomReview(t *testing.T) {
	data, err := builtinFS.ReadFile("builtins/clean-room-review.workflow.toml")
	if err != nil {
		t.Fatalf("read embedded builtin: %v", err)
	}
	wf, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	plan, err := BuildPlan(wf, map[string]string{"repo": "cameronsjo/forgectl"})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}

	if plan.Name != "clean-room-review" {
		t.Errorf("plan.Name = %q, want clean-room-review", plan.Name)
	}
	if len(plan.Steps) != 5 {
		t.Fatalf("plan.Steps = %d, want 5", len(plan.Steps))
	}

	worktree := plan.Steps[0]
	if worktree.Repo != "cameronsjo/forgectl" {
		t.Errorf("worktree.Repo = %q, want cameronsjo/forgectl", worktree.Repo)
	}
	if worktree.Ref != "main" {
		t.Errorf("worktree.Ref = %q, want main (the branch param default)", worktree.Ref)
	}

	strip := plan.Steps[1]
	if len(strip.Globs) != 5 {
		t.Errorf("strip.Globs = %d entries, want 5", len(strip.Globs))
	}

	launch := plan.Steps[2]
	if launch.Skill != "code-review" {
		t.Errorf("launch.Skill = %q, want code-review (the skill param default)", launch.Skill)
	}
	if launch.Mode != "sync" {
		t.Errorf("launch.Mode = %q, want sync", launch.Mode)
	}
}

func TestBuildPlan_MissingRequiredParam(t *testing.T) {
	data, err := builtinFS.ReadFile("builtins/clean-room-review.workflow.toml")
	if err != nil {
		t.Fatalf("read embedded builtin: %v", err)
	}
	wf, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	// repo is required and not supplied.
	if _, err := BuildPlan(wf, nil); err == nil {
		t.Fatal("expected an error for missing required param repo")
	}
}

func TestBuildPlan_UnknownVariable(t *testing.T) {
	wf := Workflow{
		DSLVersion: 1,
		Name:       "bad-ref",
		Steps: []Step{
			{Uses: "run", Cmd: "echo", Args: []string{"${nope}"}},
		},
	}
	if _, err := BuildPlan(wf, nil); err == nil {
		t.Fatal("expected an error for an unresolved ${nope} reference")
	}
}
