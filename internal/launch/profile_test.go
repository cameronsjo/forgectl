package launch

import (
	"reflect"
	"testing"

	"github.com/cameronsjo/forgectl/internal/config"
)

const testHome = "/home/u"

func resolveAt(lc config.LaunchConfig, cwd string) Profile {
	return resolve(lc, cwd, testHome)
}

func TestResolve_NoProjects_UsesBuiltinDefaults(t *testing.T) {
	got := resolveAt(config.LaunchConfig{}, "/home/u/somewhere")
	if got.Model != "opus" {
		t.Errorf("Model = %q, want %q", got.Model, "opus")
	}
	if got.PermissionMode != "plan" {
		t.Errorf("PermissionMode = %q, want %q", got.PermissionMode, "plan")
	}
	if got.AllowDanger != true {
		t.Errorf("AllowDanger = %v, want true", got.AllowDanger)
	}
	if got.Match != "" {
		t.Errorf("Match = %q, want empty", got.Match)
	}
}

func TestResolve_DefaultsOverrideBuiltins(t *testing.T) {
	no := false
	lc := config.LaunchConfig{
		Defaults: config.LaunchDefaults{Model: "sonnet", PermissionMode: "acceptEdits", AllowDanger: &no},
	}
	got := resolveAt(lc, "/home/u/somewhere")
	if got.Model != "sonnet" {
		t.Errorf("Model = %q, want %q", got.Model, "sonnet")
	}
	if got.PermissionMode != "acceptEdits" {
		t.Errorf("PermissionMode = %q, want %q", got.PermissionMode, "acceptEdits")
	}
	if got.AllowDanger != false {
		t.Errorf("AllowDanger = %v, want false", got.AllowDanger)
	}
}

func TestResolve_LongestPrefixWins(t *testing.T) {
	lc := config.LaunchConfig{
		Projects: []config.LaunchProject{
			{Match: "~/Projects", Model: "sonnet"},
			{Match: "~/Projects/minute", Model: "haiku"},
		},
	}
	got := resolveAt(lc, "/home/u/Projects/minute/sub")
	if got.Model != "haiku" {
		t.Errorf("Model = %q, want %q", got.Model, "haiku")
	}
	if got.Match != "~/Projects/minute" {
		t.Errorf("Match = %q, want %q", got.Match, "~/Projects/minute")
	}
}

func TestResolve_ExactMatchCountsAsPrefix(t *testing.T) {
	lc := config.LaunchConfig{
		Projects: []config.LaunchProject{
			{Match: "~/Projects/minute", Model: "haiku"},
		},
	}
	got := resolveAt(lc, "/home/u/Projects/minute")
	if got.Model != "haiku" {
		t.Errorf("Model = %q, want %q", got.Model, "haiku")
	}
}

func TestResolve_ComponentBoundary_NoFalsePrefix(t *testing.T) {
	lc := config.LaunchConfig{
		Projects: []config.LaunchProject{
			{Match: "~/Projects/minute", Model: "haiku"},
		},
	}
	got := resolveAt(lc, "/home/u/Projects/minuteworld")
	if got.Match != "" {
		t.Errorf("Match = %q, want empty (no false prefix match)", got.Match)
	}
	if got.Model != "opus" {
		t.Errorf("Model = %q, want built-in default %q", got.Model, "opus")
	}
}

func TestResolve_ScalarMerge_ProjectWinsWhenSet_DefaultsOtherwise(t *testing.T) {
	lc := config.LaunchConfig{
		Defaults: config.LaunchDefaults{Model: "opus", PermissionMode: "plan"},
		Projects: []config.LaunchProject{
			{Match: "~/p", Model: "sonnet"},
		},
	}
	got := resolveAt(lc, "/home/u/p")
	if got.Model != "sonnet" {
		t.Errorf("Model = %q, want %q", got.Model, "sonnet")
	}
	if got.PermissionMode != "plan" {
		t.Errorf("PermissionMode = %q, want %q (from defaults)", got.PermissionMode, "plan")
	}
}

func TestResolve_AllowDangerOverrideToFalse(t *testing.T) {
	yes := true
	no := false
	lc := config.LaunchConfig{
		Defaults: config.LaunchDefaults{AllowDanger: &yes},
		Projects: []config.LaunchProject{
			{Match: "~/p", AllowDanger: &no},
		},
	}
	got := resolveAt(lc, "/home/u/p")
	if got.AllowDanger != false {
		t.Errorf("AllowDanger = %v, want false", got.AllowDanger)
	}
}

func TestResolve_EnvMerge_ProjectWins(t *testing.T) {
	lc := config.LaunchConfig{
		Defaults: config.LaunchDefaults{Env: map[string]string{"A": "1", "B": "2"}},
		Projects: []config.LaunchProject{
			{Match: "~/p", Env: map[string]string{"B": "3", "C": "4"}},
		},
	}
	got := resolveAt(lc, "/home/u/p")
	want := map[string]string{"A": "1", "B": "3", "C": "4"}
	if !reflect.DeepEqual(got.Env, want) {
		t.Errorf("Env = %v, want %v", got.Env, want)
	}
}

func TestResolve_AddDir_ConcatExpandDedupe(t *testing.T) {
	lc := config.LaunchConfig{
		Defaults: config.LaunchDefaults{AddDir: []string{"~/a", "~/shared"}},
		Projects: []config.LaunchProject{
			{Match: "~/p", AddDir: []string{"~/shared", "~/b"}},
		},
	}
	got := resolveAt(lc, "/home/u/p")
	want := []string{"/home/u/a", "/home/u/shared", "/home/u/b"}
	if !reflect.DeepEqual(got.AddDir, want) {
		t.Errorf("AddDir = %v, want %v", got.AddDir, want)
	}
}

func TestResolve_DefaultsOnly_NoMatch_StillExpandsAddDir(t *testing.T) {
	lc := config.LaunchConfig{
		Defaults: config.LaunchDefaults{AddDir: []string{"~/g"}},
	}
	got := resolveAt(lc, "/home/u/elsewhere")
	want := []string{"/home/u/g"}
	if !reflect.DeepEqual(got.AddDir, want) {
		t.Errorf("AddDir = %v, want %v", got.AddDir, want)
	}
}

func TestExpandTilde(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"~", testHome},
		{"~/Projects", "/home/u/Projects"},
		{"/abs/path", "/abs/path"},
		{"relative", "relative"},
		{"~notme/x", "~notme/x"},
	}
	for _, tc := range cases {
		got := expandTilde(tc.in, testHome)
		if got != tc.want {
			t.Errorf("expandTilde(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
