package launch

import (
	"reflect"
	"testing"
)

func TestModelChoices_StandardAlias_NoDuplication(t *testing.T) {
	for _, m := range []string{"opus", "sonnet", "haiku"} {
		got := modelChoices(m)
		want := []string{"opus", "sonnet", "haiku"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("modelChoices(%q) = %v, want %v", m, got, want)
		}
	}
}

func TestModelChoices_CustomModel_PrependedAndSelectable(t *testing.T) {
	got := modelChoices("claude-opus-4-8")
	want := []string{"claude-opus-4-8", "opus", "sonnet", "haiku"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("modelChoices(custom) = %v, want %v", got, want)
	}
}

func TestModelChoices_Empty_FallsBackToAliases(t *testing.T) {
	got := modelChoices("")
	want := []string{"opus", "sonnet", "haiku"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("modelChoices(\"\") = %v, want %v", got, want)
	}
}

func TestModelOptions_CodexUsesNativeDefaultInsteadOfClaudeAliases(t *testing.T) {
	got := modelOptions(Profile{Harness: "codex"})
	if len(got) != 1 || got[0].Value != "" {
		t.Fatalf("Codex default options = %#v", got)
	}
}

func TestModelOptions_CodexConfiguredModelCanFallBackToNativeDefault(t *testing.T) {
	got := modelOptions(Profile{Harness: "codex", Model: "gpt-5.6-sol"})
	if len(got) != 2 || got[0].Value != "gpt-5.6-sol" || got[1].Value != "" {
		t.Fatalf("Codex configured options = %#v", got)
	}
}

func TestSessionMode(t *testing.T) {
	cases := []struct {
		in   string
		want SessionMode
	}{
		{"new", New},
		{"resume", Resume},
		{"fork", Fork},
		{"anything-else", New},
	}
	for _, tc := range cases {
		got := sessionMode(tc.in)
		if got != tc.want {
			t.Errorf("sessionMode(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
