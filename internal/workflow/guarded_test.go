package workflow

import (
	"strings"
	"testing"
)

func TestGuardedValues_MapsEveryStepField(t *testing.T) {
	s := Step{
		Uses:    "kitchen-sink",
		Repo:    "owner/x",
		Ref:     "main",
		Globs:   []string{"CLAUDE.md", ".claude/"},
		Skill:   "code-review",
		Posture: "opus",
		Mode:    "sync",
		From:    "${review}",
		To:      "out.md",
		Cmd:     "make",
		Args:    []string{"-C", "dir"},
	}
	want := map[string][]string{
		"Repo":    {"owner/x"},
		"Ref":     {"main"},
		"Globs":   {"CLAUDE.md", ".claude/"},
		"Skill":   {"code-review"},
		"Posture": {"opus"},
		"Mode":    {"sync"},
		"From":    {"${review}"},
		"To":      {"out.md"},
		"Cmd":     {"make"},
		"Args":    {"-C", "dir"},
	}

	fields := make([]string, 0, len(want))
	for f := range want {
		fields = append(fields, f)
	}
	got, err := GuardedValues(s, fields)
	if err != nil {
		t.Fatalf("GuardedValues: %v", err)
	}
	for field, wantVals := range want {
		gotVals, ok := got[field]
		if !ok {
			t.Errorf("field %q missing from the result", field)
			continue
		}
		if len(gotVals) != len(wantVals) {
			t.Errorf("field %q = %v, want %v", field, gotVals, wantVals)
			continue
		}
		for i := range wantVals {
			if gotVals[i] != wantVals[i] {
				t.Errorf("field %q[%d] = %q, want %q", field, i, gotVals[i], wantVals[i])
			}
		}
	}
}

// TestGuardedValues_UnknownFieldIsHardError is the typo guard. A Def naming
// "Glob" instead of "Globs" must FAIL LOUDLY — silently skipping it would
// disable the param-injection guard on the one field that is a security control.
func TestGuardedValues_UnknownFieldIsHardError(t *testing.T) {
	_, err := GuardedValues(Step{Uses: "strip", Globs: []string{"CLAUDE.md"}}, []string{"Glob"})
	if err == nil {
		t.Fatal("a GuardedFields entry naming an unknown field must error, not silently skip")
	}
	for _, want := range []string{"strip", "Glob"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %q", err, want)
		}
	}
}

// TestGuardedValues_RegistryDefsNameRealFields walks the ENGINE's own registry:
// every GuardedFields entry a builtin verb declares must map to a real Step
// field. Module contributions (strip, launch) are covered end-to-end by the CLI
// bless tests, which build their StepChecks through the merged registry.
func TestGuardedValues_RegistryDefsNameRealFields(t *testing.T) {
	for verb, def := range builtinRegistry() {
		if _, err := GuardedValues(Step{Uses: verb}, def.GuardedFields); err != nil {
			t.Errorf("builtin verb %q declares a bad guarded field: %v", verb, err)
		}
	}
}

func TestGuardedValues_NoGuardedFieldsIsEmpty(t *testing.T) {
	got, err := GuardedValues(Step{Uses: "worktree", Repo: "${repo}", Ref: "${branch}"}, nil)
	if err != nil {
		t.Fatalf("GuardedValues: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("a verb with no guarded fields must yield no values, got %v", got)
	}
}
