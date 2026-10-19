//go:build unix

package tui

import (
	"context"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/surface/goal"
	"github.com/cameronsjo/forgectl/internal/theme"
)

func TestProjectGoalTeamPresetsKeepLegacyNilAndOfferMixedNativeRoles(t *testing.T) {
	model := newProjectGoalsModel(context.Background(), ProjectGoalsOptions{Theme: theme.Default(), Model: "gpt-6.1-sol"})
	if model.draft.Team != nil || model.teamID != "codex" {
		t.Fatal("default team changed the legacy all-Codex spec")
	}
	model.applyTeamPreset("mixed", false)
	if model.draft.Team == nil {
		t.Fatal("mixed team did not materialize a TeamSpec")
	}
	team := model.draft.EffectiveTeam()
	if team.Coordinator.Harness != "codex" || team.Implementation.Harness != "pi" || team.Reviewer.Harness != "claude" {
		t.Fatalf("mixed team harnesses = %#v", team)
	}
	if team.Reviewer.Model != "claude-opus-5-5" {
		t.Fatalf("mixed reviewer model = %q", team.Reviewer.Model)
	}
	model.startSetup()
	if model.draft.Team != nil || model.teamID != "codex" {
		t.Fatal("new setup did not reset to the legacy all-Codex team")
	}
}

func TestProjectGoalTeamRoleEditInvalidatesApproval(t *testing.T) {
	model := newProjectGoalsModel(context.Background(), ProjectGoalsOptions{Theme: theme.Default(), Model: "gpt-6.1-sol"})
	model.applyTeamPreset("mixed", false)
	prepared := projectGoalsPreparedFixture()
	prepared.Spec = model.draft
	prepared.Spec.BaseRevision = strings.Repeat("a", 40)
	model.prepared = &prepared
	model.setupAdvanced = true
	model.setupFocus = model.setupButtonIndex(projectGoalsFieldReviewer)
	model.beginSetupEdit()
	model.edit = []rune("claude,anthropic,claude-opus-5-5")
	model.cursor = len(model.edit)
	if err := model.commitSetupEdit(); err != nil {
		t.Fatalf("commit reviewer role: %v", err)
	}
	if model.prepared != nil || model.teamID != "custom" {
		t.Fatal("editing a team role did not invalidate approval")
	}
	if got := model.draft.Team.Reviewer.Model; got != "claude-opus-5-5" {
		t.Fatalf("reviewer model after edit = %q", got)
	}
}

func TestProjectGoalTeamApprovalShowsEffectiveRoles(t *testing.T) {
	model := newProjectGoalsModel(context.Background(), ProjectGoalsOptions{Theme: theme.Default(), Model: "gpt-6.1-sol"})
	model.applyTeamPreset("mixed", false)
	prepared := projectGoalsPreparedFixture()
	prepared.Spec = model.draft
	model.prepared = &prepared
	model.screen = projectGoalsSetup
	model.setupFocus = model.setupButtonIndex(projectGoalsFieldStart)
	out := strings.Join(model.renderSetup(100), "\n")
	for _, want := range []string{"Coordinator: codex/openai/gpt-6.1-sol", "Implementation: pi/openai-codex/gpt-6.1-sol", "Reviewer: claude/anthropic/claude-opus-5-5"} {
		if !strings.Contains(out, want) {
			t.Fatalf("approval omitted %q: %s", want, out)
		}
	}
}

func TestProjectGoalTeamRoleEditorShowsNarrowTriples(t *testing.T) {
	model := newProjectGoalsModel(context.Background(), ProjectGoalsOptions{Theme: theme.Default(), Model: "gpt-6.1-sol"})
	model.applyTeamPreset("mixed", false)
	model.setupAdvanced = true
	model.setupFocus = model.setupButtonIndex(projectGoalsFieldPrepare)
	model.beginSetupEdit()

	output := strings.Join(model.renderSetup(50), "\n")
	for _, want := range []string{
		"codex,openai,gpt-6.1-sol",
		"pi,openai-codex,gpt-6.1-sol",
		"claude,anthropic,claude-opus-5-5",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("narrow role editor omitted %q: %s", want, output)
		}
	}

	model.setupFocus = model.setupButtonIndex(projectGoalsFieldReviewer)
	model.beginSetupEdit()
	output = strings.ReplaceAll(strings.Join(model.renderSetup(50), "\n"), "▏", "")
	if !strings.Contains(output, "claude,anthropic,claude-opus-5-5") {
		t.Fatalf("focused narrow reviewer editor omitted full triple: %s", output)
	}
}

func TestProjectGoalTeamRoleParserRejectsAmbiguousValues(t *testing.T) {
	if _, err := parseProjectGoalTeamSelection("pi,openai-codex"); err == nil {
		t.Fatal("role parser accepted a missing model")
	}
	if _, err := parseProjectGoalTeamSelection("claude,openai,gpt-6.1-sol"); err == nil {
		t.Fatal("role parser accepted an invalid provider/harness pair")
	}
	if got, err := parseProjectGoalTeamSelection("pi,openai-codex,gpt-6.1-sol"); err != nil || got.Harness != "pi" {
		t.Fatalf("valid role parse = %#v, %v", got, err)
	}
}

func TestProjectGoalsStartDoesNotUseNormalLetterShortcut(t *testing.T) {
	starts := 0
	model := newProjectGoalsModel(context.Background(), ProjectGoalsOptions{
		Theme: theme.Default(),
		Start: func(context.Context, goal.Prepared) (goal.State, error) {
			starts++
			return goal.State{}, nil
		},
	})
	prepared := projectGoalsPreparedFixture()
	model.prepared = &prepared
	model.draft = prepared.Spec
	model.screen = projectGoalsSetup
	model.setupFocus = model.setupButtonIndex(projectGoalsFieldStart)
	model.beginSetupEdit()
	_, command := model.Update(projectGoalsKey("s"))
	if command != nil || starts != 0 {
		t.Fatalf("normal letter started goal: command=%v starts=%d", command != nil, starts)
	}
}
