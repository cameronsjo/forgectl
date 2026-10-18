//go:build unix

package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/cameronsjo/forgectl/internal/surface/goal"
	"github.com/cameronsjo/forgectl/internal/theme"
)

func projectGoalsPreparedFixture() goal.Prepared {
	return goal.Prepared{
		Spec: goal.Spec{
			Version:      goal.Version,
			Title:        "Improve the parser",
			Objective:    "Make the parser accept the requested input.",
			Repository:   "/repo",
			BaseRevision: strings.Repeat("a", 40),
			AllowedPaths: []string{"internal/parser"},
			Acceptance:   []string{"The parser accepts the requested input."},
			Checks:       [][]string{{"git", "diff", "--check"}},
			Model:        "gpt-5-codex",
			LimitMinutes: 10,
		},
		SourceBranch: "main",
	}
}

func projectGoalsStateFixture() goal.State {
	prepared := projectGoalsPreparedFixture()
	return goal.State{
		Version:          goal.Version,
		ID:               "goal-0123456789abcdef01234567",
		Spec:             prepared.Spec,
		GoalSHA256:       strings.Repeat("b", 64),
		Phase:            goal.PhaseImplementing,
		Summary:          "Implementation is in progress",
		Root:             "/state/forgectl/projects",
		RunDir:           "/state/forgectl/projects/run",
		Worktree:         "/state/forgectl/projects/worktree",
		Branch:           "goal/parser",
		CreatedAt:        time.Now().UTC(),
		UpdatedAt:        time.Now().UTC(),
		HeartbeatAt:      time.Now().UTC(),
		Deadline:         time.Now().UTC().Add(10 * time.Minute),
		SupervisorActive: true,
	}
}

func projectGoalsKey(text string) tea.KeyPressMsg { return tea.KeyPressMsg{Text: text} }

func TestProjectGoalsNarrowDashboardDoesNotOverflow(t *testing.T) {
	model := newProjectGoalsModel(context.Background(), ProjectGoalsOptions{Theme: theme.Default(), Repository: "/repo", Model: "gpt-5-codex"})
	model.width, model.height = 50, 16
	model.states = []goal.State{projectGoalsStateFixture()}
	model.selectedID = model.states[0].ID
	model.reconcileSelection()
	out := model.View().Content
	if len(strings.Split(out, "\n")) != 16 {
		t.Fatalf("rendered %d rows, want 16: %q", len(strings.Split(out, "\n")), out)
	}
	for _, line := range strings.Split(out, "\n") {
		if ansi.StringWidth(line) > 50 {
			t.Fatalf("line overflowed 50 columns: %d: %q", ansi.StringWidth(line), line)
		}
	}
	if strings.Contains(out, "\x1b[31m") {
		t.Fatal("raw terminal escape sequence appeared in goal text")
	}
}

func TestProjectGoalsDashboardDisplaysConfiguredRegistry(t *testing.T) {
	for _, root := range []string{"/state/default", "/private/tmp/explicit"} {
		model := newProjectGoalsModel(context.Background(), ProjectGoalsOptions{Theme: theme.Default(), Root: root, Repository: "/repo", Model: "gpt-5-codex"})
		model.width, model.height = 80, 24
		if !strings.Contains(model.View().Content, root) {
			t.Fatalf("dashboard does not display registry root %q", root)
		}
	}
}

func TestProjectGoalsReviewDisplaysTitle(t *testing.T) {
	prepared := projectGoalsPreparedFixture()
	model := newProjectGoalsModel(context.Background(), ProjectGoalsOptions{Theme: theme.Default()})
	model.prepared = &prepared
	model.draft = prepared.Spec
	model.screen = projectGoalsReview
	model.width = 100
	if out := strings.Join(model.renderReview(96), "\n"); !strings.Contains(out, "Title: Improve the parser") {
		t.Fatalf("review omitted title: %q", out)
	}
}

func TestProjectGoalsLongObjectiveTailVisibleWhenScrolled(t *testing.T) {
	prepared := projectGoalsPreparedFixture()
	prepared.Spec.Objective = strings.Repeat("objective ", 1200) + "approved-tail-marker"
	model := newProjectGoalsModel(context.Background(), ProjectGoalsOptions{Theme: theme.Default()})
	model.prepared = &prepared
	model.draft = prepared.Spec
	model.screen = projectGoalsReview
	model.width, model.height = 50, 16
	model.scroll = 100000
	if out := model.View().Content; !strings.Contains(out, "approved-tail-marker") {
		t.Fatalf("scrolled review omitted the tail of the approved objective: %q", out)
	}
}

func TestProjectGoalsWizardFooterMatchesMultilineInput(t *testing.T) {
	model := newProjectGoalsModel(context.Background(), ProjectGoalsOptions{Theme: theme.Default()})
	model.width = 50
	model.screen = projectGoalsWizard
	model.wizardStep = 2
	if got := model.footerText(); got != "^c quit · esc back · ctrl+s next · enter nl" || ansi.StringWidth(got) > 46 {
		t.Fatalf("narrow multiline footer = %q width=%d", got, ansi.StringWidth(got))
	}
	model.wizardStep = 1
	if got := model.footerText(); got != "^c quit · esc back · ctrl+s/enter next" {
		t.Fatalf("narrow single-line footer = %q", got)
	}
}

func TestProjectGoalsArtifactMessagePreservesLinesAndSanitizes(t *testing.T) {
	model := newProjectGoalsModel(context.Background(), ProjectGoalsOptions{Theme: theme.Default()})
	_, command := model.Update(projectGoalsArtifactMsg{kind: "patch", text: "first\nsecond\x1b[31m"})
	if command != nil || model.screen != projectGoalsArtifact {
		t.Fatalf("artifact message screen=%v command=%v", model.screen, command != nil)
	}
	if len(model.viewerText) != 2 || model.viewerText[0] != "first" || !strings.HasPrefix(model.viewerText[1], "second") {
		t.Fatalf("viewer lines = %#v; want separate sanitized lines", model.viewerText)
	}
	if strings.Contains(model.viewerText[1], "\x1b") {
		t.Fatal("artifact viewer retained a raw terminal escape")
	}
}

func TestProjectGoalsPasteIsBoundedToInputScreens(t *testing.T) {
	model := newProjectGoalsModel(context.Background(), ProjectGoalsOptions{Theme: theme.Default()})
	model.screen = projectGoalsWizard
	model.edit = []rune(strings.Repeat("a", projectGoalsEditPasteLimit))
	model.cursor = len(model.edit)
	_, command := model.Update(tea.PasteMsg{Content: "x"})
	if command != nil || len(model.edit) != projectGoalsEditPasteLimit || model.errText == "" {
		t.Fatalf("oversize goal paste changed state: command=%v length=%d error=%q", command != nil, len(model.edit), model.errText)
	}

	model.screen = projectGoalsQuestion
	model.answer = []rune(strings.Repeat("a", projectGoalsAnswerPasteLimit))
	model.answerCursor = len(model.answer)
	model.errText = ""
	_, command = model.Update(tea.PasteMsg{Content: "x"})
	if command != nil || len(model.answer) != projectGoalsAnswerPasteLimit || model.errText == "" {
		t.Fatalf("oversize answer paste changed state: command=%v length=%d error=%q", command != nil, len(model.answer), model.errText)
	}
}

func TestProjectGoalsPasteOutsideInputScreensDoesNothing(t *testing.T) {
	model := newProjectGoalsModel(context.Background(), ProjectGoalsOptions{Theme: theme.Default()})
	model.screen = projectGoalsDashboard
	model.edit = []rune("unchanged")
	model.answer = []rune("also unchanged")
	model.errText = ""
	_, command := model.Update(tea.PasteMsg{Content: "must not be accepted"})
	if command != nil || string(model.edit) != "unchanged" || string(model.answer) != "also unchanged" || model.errText != "" {
		t.Fatalf("paste changed non-input screen: command=%v edit=%q answer=%q error=%q", command != nil, model.edit, model.answer, model.errText)
	}
}

func TestSupervisorLabelDoesNotInferStoppedFromMissingLease(t *testing.T) {
	state := projectGoalsStateFixture()
	state.SupervisorActive = false
	state.Phase = goal.PhaseUnknown
	if got := supervisorLabel(state); got != "unknown; inspect receipts; no replay" {
		t.Fatalf("unknown supervisor label = %q", got)
	}
	state.Phase = goal.PhaseInterrupted
	if got := supervisorLabel(state); got != "unknown; inspect receipts; no replay" {
		t.Fatalf("interrupted supervisor label = %q", got)
	}
	state.CleanupComplete = true
	state.CancelRequested = true
	if got := supervisorLabel(state); got != "stopped; cleanup confirmed" {
		t.Fatalf("cleanup-confirmed supervisor label = %q", got)
	}
}

func TestProjectGoalsMalformedChecksDoNotPrepareOrStart(t *testing.T) {
	prepared := 0
	model := newProjectGoalsModel(context.Background(), ProjectGoalsOptions{
		Theme:      theme.Default(),
		Repository: "/repo",
		Model:      "gpt-5-codex",
		Prepare: func(context.Context, goal.Spec) (goal.Prepared, error) {
			prepared++
			return projectGoalsPreparedFixture(), nil
		},
		Start: func(context.Context, goal.Prepared) (goal.State, error) {
			t.Fatal("start called for malformed form")
			return goal.State{}, nil
		},
	})
	model.screen = projectGoalsWizard
	model.wizardStep = 5
	model.edit = []rune(`[["git",]]`)
	model.cursor = len(model.edit)
	_, command := model.Update(projectGoalsKey("enter"))
	if command != nil || prepared != 0 {
		t.Fatalf("malformed checks scheduled work: command=%v prepare=%d", command != nil, prepared)
	}
	if model.errText == "" {
		t.Fatal("malformed checks did not produce an inline error")
	}
}

func TestProjectGoalsReviewRequiresExplicitStartKey(t *testing.T) {
	starts := 0
	model := newProjectGoalsModel(context.Background(), ProjectGoalsOptions{
		Theme: theme.Default(),
		Start: func(_ context.Context, prepared goal.Prepared) (goal.State, error) {
			starts++
			if prepared.Spec.BaseRevision == "" {
				t.Fatal("start received an unprepared spec")
			}
			return projectGoalsStateFixture(), nil
		},
	})
	prepared := projectGoalsPreparedFixture()
	model.prepared = &prepared
	model.draft = prepared.Spec
	model.screen = projectGoalsReview
	_, command := model.Update(projectGoalsKey("enter"))
	if command != nil || starts != 0 {
		t.Fatalf("enter crossed approval boundary: command=%v starts=%d", command != nil, starts)
	}
	_, command = model.Update(projectGoalsKey("s"))
	if command == nil {
		t.Fatal("explicit start key did not schedule Start")
	}
	message := command()
	if _, ok := message.(projectGoalsStartedMsg); !ok || starts != 1 {
		t.Fatalf("start command = %T, starts=%d", message, starts)
	}
}

func TestProjectGoalsCancelRequestsOnlyAfterConfirmation(t *testing.T) {
	cancels := 0
	model := newProjectGoalsModel(context.Background(), ProjectGoalsOptions{
		Theme:  theme.Default(),
		Cancel: func(context.Context, string) error { cancels++; return nil },
	})
	model.states = []goal.State{projectGoalsStateFixture()}
	model.selectedID = model.states[0].ID
	model.screen = projectGoalsDetail
	model.Update(projectGoalsKey("c"))
	if model.screen != projectGoalsCancel {
		t.Fatalf("cancel did not open confirmation: %v", model.screen)
	}
	model.Update(projectGoalsKey("n"))
	if cancels != 0 || model.screen != projectGoalsDetail {
		t.Fatalf("no confirmation still canceled: calls=%d screen=%v", cancels, model.screen)
	}
	model.Update(projectGoalsKey("c"))
	_, command := model.Update(projectGoalsKey("y"))
	if command == nil {
		t.Fatal("confirmation did not schedule cancel")
	}
	message := command()
	if _, ok := message.(projectGoalsActionMsg); !ok || cancels != 1 {
		t.Fatalf("cancel command=%T calls=%d", message, cancels)
	}
}

func TestProjectGoalsQuestionUsesExactIdentity(t *testing.T) {
	var gotGoal, gotQuestion, gotAnswer string
	model := newProjectGoalsModel(context.Background(), ProjectGoalsOptions{
		Theme: theme.Default(),
		Respond: func(_ context.Context, goalID, questionID, answer string) error {
			gotGoal, gotQuestion, gotAnswer = goalID, questionID, answer
			return nil
		},
	})
	state := projectGoalsStateFixture()
	state.Question = &goal.Question{ID: "question-1", Role: "coordinator", Text: "Which parser behavior is intended?"}
	model.states = []goal.State{state}
	model.selectedID = state.ID
	model.screen = projectGoalsDetail
	model.Update(projectGoalsKey("a"))
	if model.screen != projectGoalsQuestion {
		t.Fatalf("question action opened %v", model.screen)
	}
	model.Update(projectGoalsKey("yes"))
	_, command := model.Update(projectGoalsKey("enter"))
	if command == nil {
		t.Fatal("answer did not schedule Respond")
	}
	message := command()
	if _, ok := message.(projectGoalsActionMsg); !ok {
		t.Fatalf("answer command=%T", message)
	}
	if gotGoal != state.ID || gotQuestion != "question-1" || gotAnswer != "yes" {
		t.Fatalf("identity=%q/%q answer=%q", gotGoal, gotQuestion, gotAnswer)
	}
}
