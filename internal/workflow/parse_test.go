package workflow

import (
	"errors"
	"testing"
)

func TestParse_EmbeddedCleanRoomReview(t *testing.T) {
	data, err := builtinFS.ReadFile("builtins/clean-room-review.workflow.toml")
	if err != nil {
		t.Fatalf("read embedded builtin: %v", err)
	}

	wf, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if wf.DSLVersion != 1 {
		t.Errorf("DSLVersion = %d, want 1", wf.DSLVersion)
	}
	if wf.Name != "clean-room-review" {
		t.Errorf("Name = %q, want clean-room-review", wf.Name)
	}
	if wf.Version != "1.0.0" {
		t.Errorf("Version = %q, want 1.0.0", wf.Version)
	}
	if len(wf.Steps) != 5 {
		t.Fatalf("Steps = %d, want 5", len(wf.Steps))
	}
	wantUses := []string{"worktree", "strip", "launch", "collect", "teardown"}
	for i, want := range wantUses {
		if wf.Steps[i].Uses != want {
			t.Errorf("step %d uses = %q, want %q", i, wf.Steps[i].Uses, want)
		}
	}

	repoParam, ok := wf.Params["repo"]
	if !ok {
		t.Fatal("expected params.repo to be declared")
	}
	if !repoParam.Required {
		t.Error("params.repo should be required")
	}

	branchParam, ok := wf.Params["branch"]
	if !ok {
		t.Fatal("expected params.branch to be declared")
	}
	if branchParam.Default != "main" {
		t.Errorf("params.branch default = %q, want main", branchParam.Default)
	}
}

func TestParse_UnsupportedDSLVersion(t *testing.T) {
	data := []byte(`
dsl_version = 99
name = "future"
version = "1.0.0"

[[step]]
uses = "run"
cmd = "echo"
`)

	_, err := Parse(data)
	if err == nil {
		t.Fatal("expected an error for unsupported dsl_version")
	}

	var uerr *UnsupportedDSLVersionError
	if !errors.As(err, &uerr) {
		t.Fatalf("expected *UnsupportedDSLVersionError, got %T: %v", err, err)
	}
	if uerr.Got != 99 {
		t.Errorf("Got = %d, want 99", uerr.Got)
	}
}

func TestParse_UnsupportedDSLVersion_RefusedBeforePlanning(t *testing.T) {
	// A file with a bogus/missing field elsewhere in the workflow-typed struct
	// (branch's default is an int, which would fail decode into a string) must
	// still be refused by the dsl_version gate, not by a downstream decode
	// error — proving the version check runs first.
	data := []byte(`
dsl_version = 2

[params]
branch = { default = 12345 }
`)

	_, err := Parse(data)
	var uerr *UnsupportedDSLVersionError
	if !errors.As(err, &uerr) {
		t.Fatalf("expected *UnsupportedDSLVersionError before any other parse error, got %T: %v", err, err)
	}
}
