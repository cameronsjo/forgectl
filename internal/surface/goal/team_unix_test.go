//go:build unix

package goal

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestTeamIsAdditiveAndLegacyBytesAreUnchanged(t *testing.T) {
	const retained = `{"version":1,"title":"Fixture","objective":"Preserve native goal bytes","repository":"/source","base_revision":"","allowed_paths":["src"],"acceptance":["unchanged"],"checks":[["git","diff","--check"]],"model":"gpt-5","limit_minutes":5}`
	var spec Spec
	if err := json.Unmarshal([]byte(retained), &spec); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(spec)
	if err != nil || string(encoded) != retained {
		t.Fatalf("legacy spec bytes changed: %s, %v", encoded, err)
	}
	team := spec.EffectiveTeam()
	for _, name := range []string{"coordinator", "implementation", "reviewer"} {
		selection, found := team.Role(name)
		if !found || selection != (ModelSelection{Harness: "codex", Provider: "openai", Model: "gpt-5"}) {
			t.Fatalf("legacy %s selection = %#v", name, selection)
		}
	}
	if spec.Team != nil {
		t.Fatal("effective defaults mutated the retained spec")
	}
	if _, found := team.Role("unknown"); found {
		t.Fatal("unknown role resolved to a teammate")
	}
}

func TestTeamRequiresExplicitNativeSelectionAndChangesApproval(t *testing.T) {
	spec := goalSpec("/source")
	legacy, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	team := spec.EffectiveTeam()
	team.Implementation = ModelSelection{Harness: "pi", Provider: "openai-codex", Model: "gpt-5"}
	team.Reviewer = ModelSelection{Harness: "claude", Provider: "anthropic", Model: "claude-opus-5-5"}
	spec.Team = &team
	if err := spec.Validate(); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(spec)
	if err != nil || Digest(encoded) == Digest(legacy) {
		t.Fatal("explicit team did not invalidate the legacy approval digest")
	}
	for _, selection := range []ModelSelection{
		{Harness: "pi", Provider: "anthropic", Model: "claude-opus-5-5"},
		{Harness: "claude", Provider: "openai", Model: "claude-opus-5-5"},
		{Harness: "claude", Provider: "anthropic", Model: "opus"},
		{Harness: "hermes", Provider: "openai", Model: "gpt-5"},
		{Harness: "codex", Provider: "", Model: "gpt-5"},
		{Harness: "codex", Provider: "openai", Model: "--model"},
		{},
	} {
		team.Reviewer = selection
		if err := spec.Validate(); err == nil || !strings.Contains(err.Error(), "reviewer") {
			t.Fatalf("invalid reviewer selection accepted: %#v, %v", selection, err)
		}
	}
}
