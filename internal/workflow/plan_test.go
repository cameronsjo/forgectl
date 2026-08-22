package workflow

import (
	"context"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
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

	// The built-in strip step carries NO globs on purpose: it inherits the
	// canonical default set (quarantine.DefaultTargets, via [workflow]
	// strip_globs). A literal list here is a second copy of a security
	// enumeration, and the one that used to live here had already drifted.
	strip := plan.Steps[1]
	if len(strip.Globs) != 0 {
		t.Errorf("strip.Globs = %v, want none (inherit the canonical default set)", strip.Globs)
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

// TestBuildPlan_RejectsEveryRegistryExportWhenItsVerbIsAbsent pins #246's
// global reservation: an export name is owned by the merged registry, not only
// by the subset of exporting verbs a particular workflow happens to contain.
func TestBuildPlan_RejectsEveryRegistryExportWhenItsVerbIsAbsent(t *testing.T) {
	reg := testRegistry(t)
	for verb, def := range reg {
		for _, exp := range def.Exports {
			t.Run(verb+"/"+exp, func(t *testing.T) {
				wf := Workflow{
					DSLVersion: 1,
					Name:       "absent-exporter-collision",
					Params:     map[string]Param{exp: {Default: "attacker-controlled"}},
					Steps:      []Step{{Uses: "teardown"}},
				}
				_, err := BuildPlan(wf, nil, reg)
				if err == nil {
					t.Fatalf("param %q must be refused even though exporter %q is absent", exp, verb)
				}
				for _, want := range []string{exp, "reserved step export"} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error %q should mention %q", err, want)
					}
				}
			})
		}
	}
}

// TestBuildPlan_RegistryContributionReservesExports proves module/custom verbs
// extend the same namespace policy. The contributed verb is deliberately absent
// from the workflow: presence is not what confers ownership of an export name.
func TestBuildPlan_RegistryContributionReservesExports(t *testing.T) {
	reg, err := NewRegistry(StepRegistry{
		"publish": {Exports: []string{"artifact"}},
	})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	wf := Workflow{
		DSLVersion: 1,
		Name:       "contributed-export-collision",
		Params:     map[string]Param{"artifact": {Default: "forged"}},
		Steps:      []Step{{Uses: "run", Cmd: "true"}},
	}
	if _, err := BuildPlan(wf, nil, reg); err == nil || !strings.Contains(err.Error(), "artifact") {
		t.Fatalf("contributed export must reserve its name, got %v", err)
	}
}

// TestBuildPlan_CollisionIsStructuralAndPrecedesValueErrors defines failure
// precedence. A reserved declaration is invalid even before values are merged,
// so it wins over both a missing-required value and an unknown CLI parameter.
func TestBuildPlan_CollisionIsStructuralAndPrecedesValueErrors(t *testing.T) {
	wf := Workflow{
		DSLVersion: 1,
		Name:       "structural-collision",
		Params:     map[string]Param{"workspace": {Required: true}},
		Steps:      []Step{{Uses: "teardown"}},
	}
	_, err := BuildPlan(wf, map[string]string{"unknown": "value"}, testRegistry(t))
	if err == nil {
		t.Fatal("expected reserved export collision")
	}
	if !strings.Contains(err.Error(), "reserved step export") {
		t.Fatalf("collision must precede missing/unknown value errors, got %v", err)
	}
	if strings.Contains(err.Error(), "missing required") || strings.Contains(err.Error(), "unknown param") {
		t.Fatalf("value-resolution error took precedence over structural collision: %v", err)
	}
}

func TestBuildPlan_MultipleCollisionsReportAlphabetically(t *testing.T) {
	wf := Workflow{
		DSLVersion: 1,
		Name:       "multiple-collisions",
		Params: map[string]Param{
			"workspace": {},
			"review":    {},
		},
	}
	_, err := BuildPlan(wf, nil, testRegistry(t))
	if err == nil {
		t.Fatal("expected reserved export collision")
	}
	if !strings.Contains(err.Error(), `param "review"`) {
		t.Fatalf("first alphabetical collision must be reported, got %v", err)
	}
}

func TestBuildPlan_AllowsNonReservedParams(t *testing.T) {
	wf := Workflow{
		DSLVersion: 1,
		Name:       "legitimate-param",
		Params:     map[string]Param{"repo": {Required: true}},
		Steps:      []Step{{Uses: "worktree", Repo: "${repo}"}},
	}
	plan, err := BuildPlan(wf, map[string]string{"repo": "owner/project"}, testRegistry(t))
	if err != nil {
		t.Fatalf("non-reserved param was rejected: %v", err)
	}
	if got := plan.Steps[0].Repo; got != "owner/project" {
		t.Errorf("repo = %q, want owner/project", got)
	}
}

// TestBuildPlan_LoneTeardownWorkspaceParamStopsBeforeSink is the exact #246
// route. A test sink replaces teardown's real RemoveAll-backed runner: if plan
// validation regresses, execution reaches it and the call count exposes that
// the refusal happened too late.
func TestBuildPlan_LoneTeardownWorkspaceParamStopsBeforeSink(t *testing.T) {
	reg := testRegistry(t)
	sinkCalls := 0
	teardown := reg["teardown"]
	teardown.Runner = func(context.Context, exec.Runner, *Context, PlanStep) error {
		sinkCalls++
		return nil
	}
	reg["teardown"] = teardown

	wf := Workflow{
		DSLVersion: 1,
		Name:       "lone-teardown",
		Params:     map[string]Param{"workspace": {Default: "/tmp/forgectl-workflow-victim"}},
		Steps:      []Step{{Uses: "teardown"}},
	}
	plan, planErr := BuildPlan(wf, nil, reg)
	if planErr == nil {
		exe := NewExecutor(&exec.FakeRunner{}, reg)
		_ = exe.Run(context.Background(), plan, NewContext(map[string]string{"workspace": "/tmp/forgectl-workflow-victim"}))
	}
	if sinkCalls != 0 {
		t.Fatalf("teardown sink called %d time(s); planning must refuse first", sinkCalls)
	}
	if planErr == nil {
		t.Fatal("lone teardown with workspace param unexpectedly planned")
	}
}
