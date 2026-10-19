//go:build unix

package tui

import (
	"fmt"
	"strings"

	"github.com/cameronsjo/forgectl/internal/surface/goal"
)

type projectGoalTeamPreset struct {
	ID   string
	Name string
	Hint string
	Team *goal.TeamSpec
}

func projectGoalTeamPresets(model string) []projectGoalTeamPreset {
	legacy := goal.ModelSelection{Harness: "codex", Provider: "openai", Model: model}
	mixed := goal.TeamSpec{
		Coordinator:    legacy,
		Implementation: goal.ModelSelection{Harness: "pi", Provider: "openai-codex", Model: model},
		Reviewer:       goal.ModelSelection{Harness: "claude", Provider: "anthropic", Model: "claude-opus-5-5"},
	}
	return []projectGoalTeamPreset{
		{ID: "codex", Name: "Codex trio", Hint: "legacy single-harness run; preserves the compact goal contract"},
		{ID: "mixed", Name: "Mixed native team", Hint: "Codex coordinates · Pi implements · Claude reviews; native credentials and exact model IDs are required", Team: &mixed},
		{ID: "custom", Name: "Custom team", Hint: "edit each role explicitly in Advanced"},
	}
}

func projectGoalTeamSelectionText(selection goal.ModelSelection) string {
	return fmt.Sprintf("%s,%s,%s", selection.Harness, selection.Provider, selection.Model)
}

func parseProjectGoalTeamSelection(value string) (goal.ModelSelection, error) {
	parts := strings.Split(value, ",")
	if len(parts) != 3 {
		return goal.ModelSelection{}, fmt.Errorf("team role must be harness,provider,model")
	}
	selection := goal.ModelSelection{
		Harness:  strings.TrimSpace(parts[0]),
		Provider: strings.TrimSpace(parts[1]),
		Model:    strings.TrimSpace(parts[2]),
	}
	if err := selection.Validate(); err != nil {
		return goal.ModelSelection{}, err
	}
	return selection, nil
}

func cloneProjectGoalTeam(team goal.TeamSpec) *goal.TeamSpec {
	clone := team
	return &clone
}

func projectGoalTeamLabel(selection goal.ModelSelection) string {
	return fmt.Sprintf("%s/%s/%s", selection.Harness, selection.Provider, selection.Model)
}
