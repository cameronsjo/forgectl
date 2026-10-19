package goal

import (
	"errors"
	"fmt"
	"strings"

	"github.com/cameronsjo/forgectl/internal/config"
)

type ModelSelection struct {
	Harness  string `json:"harness"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

type TeamSpec struct {
	Coordinator    ModelSelection `json:"coordinator"`
	Implementation ModelSelection `json:"implementation"`
	Reviewer       ModelSelection `json:"reviewer"`
}

func (team TeamSpec) Validate() error {
	for _, role := range []string{"coordinator", "implementation", "reviewer"} {
		selection, _ := team.Role(role)
		if err := selection.Validate(); err != nil {
			return fmt.Errorf("%s team selection: %w", role, err)
		}
	}
	return nil
}

func (selection ModelSelection) Validate() error {
	if config.CheckModelName(selection.Model) != nil {
		return errors.New("an exact model ID is required")
	}
	switch selection.Harness {
	case "codex":
		if selection.Provider != "openai" {
			return errors.New("codex requires the direct OpenAI provider")
		}
	case "pi":
		if selection.Provider != "openai" && selection.Provider != "openai-codex" {
			return errors.New("pi requires an explicit direct OpenAI provider")
		}
	case "claude":
		if selection.Provider != "anthropic" || !strings.HasPrefix(selection.Model, "claude-") {
			return errors.New("claude requires native Anthropic and an exact claude- model ID, not an alias")
		}
	default:
		return errors.New("supported goal harnesses are codex, pi and claude")
	}
	return nil
}

func (team TeamSpec) Role(name string) (ModelSelection, bool) {
	switch name {
	case "coordinator":
		return team.Coordinator, true
	case "implementation":
		return team.Implementation, true
	case "reviewer":
		return team.Reviewer, true
	default:
		return ModelSelection{}, false
	}
}

func (spec Spec) EffectiveTeam() TeamSpec {
	if spec.Team != nil {
		return *spec.Team
	}
	selection := ModelSelection{Harness: "codex", Provider: "openai", Model: spec.Model}
	return TeamSpec{Coordinator: selection, Implementation: selection, Reviewer: selection}
}
