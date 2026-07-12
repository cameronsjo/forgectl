package workflow

import (
	"strings"
	"testing"
)

func TestBuildPlan_EmbeddedCleanRoomReview(t *testing.T) {
	data, err := builtinFS.ReadFile("builtins/clean-room-review.workflow.toml")
	if err != nil {
		t.Fatalf("read embedded builtin: %v", err)
	}
	wf, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	plan, err := BuildPlan(wf, map[string]string{"repo": "cameronsjo/forgectl"}, testRegistry(t))
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
	if _, err := BuildPlan(wf, nil, testRegistry(t)); err == nil {
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
	if _, err := BuildPlan(wf, nil, testRegistry(t)); err == nil {
		t.Fatal("expected an error for an unresolved ${nope} reference")
	}
}

func TestBuildPlan_RejectsUndeclaredParam(t *testing.T) {
	// An undeclared --param must be refused, not silently passed through — a
	// standalone hardening fix that also closes the run-step injection surface
	// (#10). Declared params still merge, default, and require as before.
	wf := Workflow{
		DSLVersion: 1,
		Name:       "declared-only",
		Params:     map[string]Param{"who": {Default: "world"}},
		Steps: []Step{
			{Uses: "run", Cmd: "echo", Args: []string{"${who}"}},
		},
	}

	if _, err := BuildPlan(wf, map[string]string{"who": "there", "stranger": "x"}, testRegistry(t)); err == nil {
		t.Fatal("expected an error for the undeclared param 'stranger'")
	}

	// The declared param alone still resolves.
	plan, err := BuildPlan(wf, map[string]string{"who": "there"}, testRegistry(t))
	if err != nil {
		t.Fatalf("BuildPlan with only declared params: %v", err)
	}
	if got := plan.Steps[0].Args[0]; got != "there" {
		t.Errorf("declared param not applied: args[0] = %q, want there", got)
	}

	// And its default applies when omitted.
	plan, err = BuildPlan(wf, nil, testRegistry(t))
	if err != nil {
		t.Fatalf("BuildPlan with default: %v", err)
	}
	if got := plan.Steps[0].Args[0]; got != "world" {
		t.Errorf("default not applied: args[0] = %q, want world", got)
	}
}

func TestBuildPlan_RejectsParamExportCollision(t *testing.T) {
	// Params and step exports share one Context namespace at execution time,
	// and an export only overwrites its name if its step Sets it — so a param
	// named after an export could ride a name the bless-time injection guard
	// (#10) trusts as step-produced. The collision is refused at plan time.
	wf := Workflow{
		DSLVersion: 1,
		Name:       "collision",
		Params:     map[string]Param{"workspace": {Default: "/tmp/x"}},
		Steps: []Step{
			{Uses: "worktree", Repo: "cameronsjo/forgectl"},
			{Uses: "run", Cmd: "make", Args: []string{"-C", "${workspace}"}},
		},
	}
	_, err := BuildPlan(wf, nil, testRegistry(t))
	if err == nil {
		t.Fatal("expected an error for param 'workspace' colliding with the worktree export")
	}
	if !strings.Contains(err.Error(), "collides") {
		t.Errorf("error %q does not name the collision", err)
	}
}
