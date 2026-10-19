//go:build unix

package tui

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

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

func TestProjectGoalsRenderingPreservesQuestionState(t *testing.T) {
	state := projectGoalsStateFixture()
	state.Question = &goal.Question{ID: "question-one", Text: "Keep this question visible?"}
	model := newProjectGoalsModel(t.Context(), ProjectGoalsOptions{Theme: theme.Default()})
	model.width, model.height = 50, 16
	model.states = []goal.State{state}
	model.selectedID = state.ID
	model.screen = projectGoalsRun
	model.questionEditing = true
	model.questionID = state.Question.ID
	model.answer = []rune("first line\nsecond line")
	model.answerCursor = len(model.answer)
	for range 3 {
		model.View()
		current, ok := model.selectedState()
		if !ok || current.Question == nil || current.Question.ID != state.Question.ID {
			t.Fatal("rendering mutated the retained question")
		}
		if string(model.answer) != "first line\nsecond line" || !model.questionEditing {
			t.Fatal("rendering mutated the in-progress answer")
		}
	}
}

func TestProjectGoalsAnswerCompletionReleasesBusyAfterQuestionRefresh(t *testing.T) {
	state := projectGoalsStateFixture()
	state.Question = &goal.Question{ID: "question-one", Text: "First question?"}
	model := newProjectGoalsModel(t.Context(), ProjectGoalsOptions{
		Theme: theme.Default(),
		Respond: func(context.Context, string, string, string) error {
			return nil
		},
	})
	model.states = []goal.State{state}
	model.selectedID = state.ID
	model.screen = projectGoalsRun
	model.questionEditing = true
	model.questionID = state.Question.ID
	model.answer = []rune("first answer")
	_, completion := model.sendAnswer()
	if completion == nil || !model.actionBusy {
		t.Fatal("answer request did not start")
	}
	state.Question = &goal.Question{ID: "question-two", Text: "Second question?"}
	model.Update(projectGoalsLoadedMsg{goalID: state.ID, state: state})
	model.Update(completion())
	if model.actionBusy || model.questionID != "question-two" {
		t.Fatalf("answer completion left busy=%v question=%q", model.actionBusy, model.questionID)
	}
}

func TestProjectGoalsPendingArtifactCannotTrapQuestionEditor(t *testing.T) {
	state := projectGoalsStateFixture()
	state.Question = &goal.Question{ID: "question-one", Text: "First question?"}
	model := newProjectGoalsModel(t.Context(), ProjectGoalsOptions{Theme: theme.Default()})
	model.states = []goal.State{state}
	model.selectedID = state.ID
	model.screen = projectGoalsRun
	model.actionBusy = true
	model.updateDetail("a", "a")
	if model.questionEditing {
		t.Fatal("artifact load entered an unavailable answer editor")
	}
	model.questionEditing = true
	model.updateQuestion("esc", "")
	if model.questionEditing {
		t.Fatal("pending action prevented leaving the answer editor")
	}
}

func TestProjectGoalsTabBindsQuestionAndCyclesAnswerControls(t *testing.T) {
	state := projectGoalsStateFixture()
	state.Question = &goal.Question{ID: "question-one", Text: "Question?"}
	model := newProjectGoalsModel(t.Context(), ProjectGoalsOptions{Theme: theme.Default()})
	model.states = []goal.State{state}
	model.selectedID = state.ID
	model.screen = projectGoalsRun
	model.width = 50
	model.updateDetail("tab", "")
	if model.questionID != state.Question.ID || !model.questionEditing {
		t.Fatal("Tab did not bind the current question before polling")
	}
	model.updateQuestion("q", "q")
	model.updateQuestion("tab", "")
	if model.runFocus != 1 {
		t.Fatal("Tab did not focus Send")
	}
	model.updateQuestion("tab", "")
	if model.runFocus != 0 {
		t.Fatal("Tab did not return to the answer")
	}
	model.updateQuestion("esc", "")
	model.updateDetail("a", "a")
	if string(model.answer) != "q" || strings.Contains(model.footerText(), "q detach") {
		t.Fatal("answer draft or input-specific help was lost")
	}
}

func TestProjectGoalsLiteralWrapPreservesApprovedArgumentSpaces(t *testing.T) {
	value := `["tool","argument  with   spaces","long-path-tail"]`
	wrapped := goalLiteralWrap(value, 15)
	if strings.Join(wrapped, "") != value {
		t.Fatalf("literal wrapping changed approved bytes: %#v", wrapped)
	}
	for _, line := range wrapped {
		if ansi.StringWidth(line) > 15 {
			t.Fatalf("literal line exceeds width: %q", line)
		}
	}
	if got := strings.Join(goalLiteralWrap("safe\x1b]52;secret\a", 15), ""); strings.ContainsAny(got, "\x1b\a") {
		t.Fatal("literal wrapping allowed terminal control bytes")
	}
}

func TestProjectGoalsLateListCannotReplaceOpenedRun(t *testing.T) {
	state := projectGoalsStateFixture()
	model := newProjectGoalsModel(t.Context(), ProjectGoalsOptions{Theme: theme.Default()})
	model.states = []goal.State{state}
	model.selectedID = state.ID
	model.screen = projectGoalsRun
	model.loading = true
	model.Update(projectGoalsListMsg{states: nil})
	if len(model.states) != 1 || model.selectedID != state.ID || model.loading {
		t.Fatal("late dashboard list replaced the opened run")
	}
}

func TestProjectGoalsArrowScrollAndEditorResume(t *testing.T) {
	state := projectGoalsStateFixture()
	model := newProjectGoalsModel(t.Context(), ProjectGoalsOptions{Theme: theme.Default()})
	model.states = []goal.State{state}
	model.selectedID = state.ID
	model.screen = projectGoalsRun
	model.runTab = 1
	model.width, model.height = 50, 16
	model.viewerText = strings.Split(strings.Repeat("evidence line\n", 30), "\n")
	before := model.View().Content
	model.updateDetail("down", "")
	if model.View().Content == before {
		t.Fatal("ordinary Down did not scroll the evidence")
	}
	model.screen = projectGoalsSetup
	model.setupFocus = model.setupButtonIndex(projectGoalsFieldGoal)
	model.beginSetupEdit()
	model.editorManualScroll = true
	model.updateSetup("left", "")
	if model.editorManualScroll {
		t.Fatal("editing did not restore caret tracking after manual scrolling")
	}
}

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
	model.screen = projectGoalsSetup
	model.width = 100
	model.setupFocus = model.setupButtonIndex(projectGoalsFieldStart)
	if out := strings.Join(model.renderSetup(96), "\n"); !strings.Contains(out, "Title: Improve the parser") {
		t.Fatalf("review omitted title: %q", out)
	}
}

func TestDeriveGoalTitleTruncatesAtUTF8Boundary(t *testing.T) {
	title := deriveGoalTitle(strings.Repeat("界", 43))
	if !utf8.ValidString(title) {
		t.Fatalf("derived title is invalid UTF-8: %q", title)
	}
	if len(title) > 128 {
		t.Fatalf("derived title is %d bytes, want at most 128", len(title))
	}
	if want := 42; len([]rune(title)) != want {
		t.Fatalf("derived title has %d runes, want %d", len([]rune(title)), want)
	}
}

func TestProjectGoalsMultilineEditorsMoveByLine(t *testing.T) {
	model := newProjectGoalsModel(context.Background(), ProjectGoalsOptions{Theme: theme.Default()})
	model.screen = projectGoalsSetup
	model.setupFocus = int(projectGoalsFieldGoal)
	model.edit = []rune("one\nsecond\nthird")
	model.cursor = len([]rune("one\nse"))
	model.Update(projectGoalsKey("home"))
	if model.cursor != 4 {
		t.Fatalf("setup Home cursor = %d, want 4", model.cursor)
	}
	model.Update(projectGoalsKey("end"))
	if model.cursor != 10 {
		t.Fatalf("setup End cursor = %d, want 10", model.cursor)
	}
	model.Update(projectGoalsKey("up"))
	if model.cursor != 3 {
		t.Fatalf("setup Up cursor = %d, want 3", model.cursor)
	}
	model.Update(projectGoalsKey("down"))
	if model.cursor != 7 {
		t.Fatalf("setup Down cursor = %d, want 7", model.cursor)
	}

	model.screen = projectGoalsRun
	model.questionEditing = true
	model.runFocus = 0
	model.answer = []rune("one\nsecond\nthird")
	model.answerCursor = len([]rune("one\nse"))
	model.Update(projectGoalsKey("home"))
	if model.answerCursor != 4 {
		t.Fatalf("answer Home cursor = %d, want 4", model.answerCursor)
	}
	model.Update(projectGoalsKey("end"))
	if model.answerCursor != 10 {
		t.Fatalf("answer End cursor = %d, want 10", model.answerCursor)
	}
	model.Update(projectGoalsKey("up"))
	if model.answerCursor != 3 {
		t.Fatalf("answer Up cursor = %d, want 3", model.answerCursor)
	}
	model.Update(projectGoalsKey("down"))
	if model.answerCursor != 7 {
		t.Fatalf("answer Down cursor = %d, want 7", model.answerCursor)
	}
}

func TestProjectGoalsLongObjectiveTailVisibleWhenScrolled(t *testing.T) {
	prepared := projectGoalsPreparedFixture()
	prepared.Spec.Objective = strings.Repeat("objective ", 1200) + "approved-tail-marker"
	model := newProjectGoalsModel(context.Background(), ProjectGoalsOptions{Theme: theme.Default()})
	model.prepared = &prepared
	model.draft = prepared.Spec
	model.screen = projectGoalsSetup
	model.setupFocus = model.setupButtonIndex(projectGoalsFieldStart)
	model.width, model.height = 50, 16
	model.scroll = 100000
	model.editorManualScroll = true
	if out := model.View().Content; !strings.Contains(out, "approved-tail-marker") {
		t.Fatalf("scrolled review omitted the tail of the approved objective: %q", out)
	}
}

func TestProjectGoalsWizardFooterMatchesMultilineInput(t *testing.T) {
	model := newProjectGoalsModel(context.Background(), ProjectGoalsOptions{Theme: theme.Default()})
	model.width = 50
	model.screen = projectGoalsSetup
	model.setupFocus = int(projectGoalsFieldGoal)
	if got := model.footerText(); !strings.Contains(got, "enter newline") || ansi.StringWidth(got) > 46 {
		t.Fatalf("narrow multiline footer = %q width=%d", got, ansi.StringWidth(got))
	}
	model.setupFocus = int(projectGoalsFieldRepository)
	if got := model.footerText(); !strings.Contains(got, "tab/backtab") {
		t.Fatalf("narrow setup footer = %q", got)
	}
}

func TestProjectGoalsWizardEditorFollowsCaretInNarrowPane(t *testing.T) {
	model := newProjectGoalsModel(context.Background(), ProjectGoalsOptions{Theme: theme.Default()})
	model.width, model.height = 50, 16
	model.screen = projectGoalsSetup
	model.setupFocus = int(projectGoalsFieldGoal)
	model.edit = []rune(strings.Repeat("objective ", 1200) + "wizard-tail")
	model.cursor = len(model.edit)
	out := model.View().Content
	if !strings.Contains(out, "wizard-tail") || !strings.Contains(out, "▏") {
		t.Fatalf("wizard editor hid the tail or caret: %q", out)
	}
}

func TestProjectGoalsWizardEditorKeepsBeginningCaretVisible(t *testing.T) {
	model := newProjectGoalsModel(context.Background(), ProjectGoalsOptions{Theme: theme.Default()})
	model.width, model.height = 50, 16
	model.screen = projectGoalsSetup
	model.setupFocus = int(projectGoalsFieldGoal)
	model.edit = []rune("wizard-begin " + strings.Repeat("wizard-filler ", 400))
	model.cursor = 0
	if out := model.View().Content; !strings.Contains(out, "▏wizard-begin") {
		t.Fatalf("wizard editor hid the beginning caret: %q", out)
	}
}

func TestProjectGoalsWizardEditorKeepsMiddleCaretVisible(t *testing.T) {
	model := newProjectGoalsModel(context.Background(), ProjectGoalsOptions{Theme: theme.Default()})
	model.width, model.height = 50, 16
	model.screen = projectGoalsSetup
	model.setupFocus = int(projectGoalsFieldGoal)
	prefix := strings.Repeat("wizard-filler ", 150)
	model.edit = []rune(prefix + "wizard-middle" + strings.Repeat(" tail", 150))
	model.cursor = len([]rune(prefix + "wizard-middle"))
	if out := model.View().Content; !strings.Contains(out, "wizard-middle▏") {
		t.Fatalf("wizard editor hid the middle caret: %q", out)
	}
}

func TestProjectGoalsQuestionEditorFollowsAnswerCaretAndInspectsPrompt(t *testing.T) {
	model := newProjectGoalsModel(context.Background(), ProjectGoalsOptions{Theme: theme.Default()})
	model.width, model.height = 50, 16
	model.screen = projectGoalsRun
	model.questionEditing = true
	model.questionText = "question-start " + strings.Repeat("question ", 500) + "question-tail"
	state := projectGoalsStateFixture()
	state.Question = &goal.Question{Text: model.questionText}
	model.states = []goal.State{state}
	model.selectedID = state.ID
	model.answer = []rune(strings.Repeat("answer ", 500) + "answer-tail")
	model.answerCursor = len(model.answer)
	out := model.View().Content
	if !strings.Contains(out, "answer-tail") || !strings.Contains(out, "▏") {
		t.Fatalf("question editor hid the answer tail or caret: %q", out)
	}
	model.Update(projectGoalsKey("ctrl+up"))
	if !strings.Contains(string(model.answer), "answer-tail") {
		t.Fatal("manual prompt scrolling changed the answer")
	}
}

func TestProjectGoalsQuestionEditorKeepsBeginningCaretVisible(t *testing.T) {
	model := newProjectGoalsModel(context.Background(), ProjectGoalsOptions{Theme: theme.Default()})
	model.width, model.height = 50, 16
	model.screen = projectGoalsRun
	model.questionEditing = true
	state := projectGoalsStateFixture()
	state.Question = &goal.Question{Text: "question"}
	model.states = []goal.State{state}
	model.selectedID = state.ID
	model.questionText = "question"
	model.answer = []rune("answer-begin " + strings.Repeat("answer-filler ", 400))
	model.answerCursor = 0
	if out := model.View().Content; !strings.Contains(out, "▏answer-begin") {
		t.Fatalf("answer editor hid the beginning caret: %q", out)
	}
}

func TestProjectGoalsQuestionEditorKeepsMiddleCaretVisible(t *testing.T) {
	model := newProjectGoalsModel(context.Background(), ProjectGoalsOptions{Theme: theme.Default()})
	model.width, model.height = 50, 16
	model.screen = projectGoalsRun
	model.questionEditing = true
	state := projectGoalsStateFixture()
	state.Question = &goal.Question{Text: "question"}
	model.states = []goal.State{state}
	model.selectedID = state.ID
	model.questionText = "question"
	prefix := strings.Repeat("answer-filler ", 150)
	model.answer = []rune(prefix + "answer-middle" + strings.Repeat(" tail", 150))
	model.answerCursor = len([]rune(prefix + "answer-middle"))
	if out := model.View().Content; !strings.Contains(out, "answer-middle▏") {
		t.Fatalf("answer editor hid the middle caret: %q", out)
	}
}

func TestProjectGoalsCaretTracksFullWordWrapAndLiteralMarkers(t *testing.T) {
	for _, value := range []string{"12345 abcdefghi", "▏abcd efghijklm"} {
		if line := goalCursorLine([]rune(value), 8, 10); line != 1 {
			t.Fatalf("caret line for %q = %d, want wrapped line 1", value, line)
		}
	}
	if line := goalCursorLine([]rune("▏abcd efghijklm"), 0, 10); line != 0 {
		t.Fatalf("beginning caret before a literal marker = %d, want line 0", line)
	}
}

func TestProjectGoalsArtifactMessagePreservesLinesAndSanitizes(t *testing.T) {
	model := newProjectGoalsModel(context.Background(), ProjectGoalsOptions{Theme: theme.Default()})
	_, command := model.Update(projectGoalsArtifactMsg{kind: "patch", text: "first\nsecond\x1b[31m"})
	if command != nil || model.screen != projectGoalsRun {
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
	model.screen = projectGoalsSetup
	model.setupFocus = int(projectGoalsFieldGoal)
	model.edit = []rune(strings.Repeat("a", projectGoalsEditPasteLimit))
	model.cursor = len(model.edit)
	_, command := model.Update(tea.PasteMsg{Content: "x"})
	if command != nil || len(model.edit) != projectGoalsEditPasteLimit || model.errText == "" {
		t.Fatalf("oversize goal paste changed state: command=%v length=%d error=%q", command != nil, len(model.edit), model.errText)
	}

	model.screen = projectGoalsRun
	model.questionEditing = true
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
	if got := supervisorLabel(state); got != "stopped; owned process groups cleaned" {
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
	model.screen = projectGoalsSetup
	model.setupAdvanced = true
	model.setupFocus = int(projectGoalsFieldChecks)
	model.edit = []rune(`[["git",]]`)
	model.cursor = len(model.edit)
	_, command := model.Update(projectGoalsKey("tab"))
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
	model.screen = projectGoalsSetup
	model.setupFocus = model.setupButtonIndex(projectGoalsFieldStart)
	model.setupFocus = int(projectGoalsFieldGoal)
	model.beginSetupEdit()
	_, command := model.Update(projectGoalsKey("enter"))
	if command != nil || starts != 0 {
		t.Fatalf("goal Enter crossed approval boundary: command=%v starts=%d", command != nil, starts)
	}
	model.setupFocus = model.setupButtonIndex(projectGoalsFieldStart)
	model.beginSetupEdit()
	_, command = model.Update(projectGoalsKey("enter"))
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
	model.screen = projectGoalsRun
	model.Update(projectGoalsKey("c"))
	if !model.cancelConfirm {
		t.Fatalf("cancel did not open confirmation: %v", model.screen)
	}
	model.Update(projectGoalsKey("n"))
	if cancels != 0 || model.cancelConfirm {
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
	model.screen = projectGoalsRun
	model.Update(projectGoalsKey("a"))
	if !model.questionEditing {
		t.Fatalf("question action did not focus inline editor")
	}
	model.Update(projectGoalsKey("yes"))
	model.Update(projectGoalsKey("tab"))
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

func TestProjectGoalsStalePrepareDoesNotClearNewAction(t *testing.T) {
	model := newProjectGoalsModel(context.Background(), ProjectGoalsOptions{Theme: theme.Default()})
	model.prepareSeq = 2
	model.actionBusy = true
	prepared := projectGoalsPreparedFixture()
	model.Update(projectGoalsPreparedMsg{sequence: 1, prepared: prepared, digest: "stale"})
	if !model.actionBusy || model.prepared != nil {
		t.Fatalf("stale prepare changed current action: busy=%v prepared=%v", model.actionBusy, model.prepared != nil)
	}
}

func TestProjectGoalsStaleLoadAndArtifactDoNotChangeCurrentGoal(t *testing.T) {
	state := projectGoalsStateFixture()
	model := newProjectGoalsModel(context.Background(), ProjectGoalsOptions{Theme: theme.Default()})
	model.screen = projectGoalsRun
	model.states = []goal.State{state}
	model.selectedID = state.ID
	model.detailSeq = 2
	model.artifactSeq = 2
	model.actionBusy = true
	model.Update(projectGoalsLoadedMsg{goalID: "goal-other", sequence: 1, state: state})
	model.Update(projectGoalsArtifactMsg{goalID: "goal-other", sequence: 1, kind: "patch", text: "stale"})
	if !model.actionBusy || model.viewerKind != "" || model.viewerText != nil {
		t.Fatalf("stale detail response changed current goal: busy=%v viewer=%q", model.actionBusy, model.viewerKind)
	}
}

func TestProjectGoalsEscInvalidatesPendingPrepare(t *testing.T) {
	model := newProjectGoalsModel(context.Background(), ProjectGoalsOptions{Theme: theme.Default()})
	model.screen = projectGoalsSetup
	model.setupFocus = model.setupButtonIndex(projectGoalsFieldPrepare)
	model.prepareSeq = 1
	model.actionBusy = true
	model.Update(projectGoalsKey("esc"))
	model.Update(projectGoalsPreparedMsg{sequence: 1, prepared: projectGoalsPreparedFixture()})
	if model.prepared != nil || model.screen != projectGoalsDashboard {
		t.Fatalf("stale prepare resurrected setup: prepared=%v screen=%v", model.prepared != nil, model.screen)
	}
}

func TestProjectGoalsStaleActionDoesNotChangeCurrentRun(t *testing.T) {
	state := projectGoalsStateFixture()
	model := newProjectGoalsModel(context.Background(), ProjectGoalsOptions{Theme: theme.Default()})
	model.screen = projectGoalsRun
	model.states = []goal.State{state}
	model.selectedID = state.ID
	model.questionID = "question-1"
	model.actionSeq = 2
	model.actionBusy = true
	model.Update(projectGoalsActionMsg{goalID: state.ID, questionID: "question-1", sequence: 1, action: "stale"})
	if !model.actionBusy || model.footer == "stale" {
		t.Fatalf("stale action changed current run: busy=%v footer=%q", model.actionBusy, model.footer)
	}
}

func TestProjectGoalsCancelResponseIsInvalidatedByEscape(t *testing.T) {
	state := projectGoalsStateFixture()
	model := newProjectGoalsModel(context.Background(), ProjectGoalsOptions{
		Theme:  theme.Default(),
		Cancel: func(context.Context, string) error { return nil },
	})
	model.screen = projectGoalsRun
	model.states = []goal.State{state}
	model.selectedID = state.ID
	model.Update(projectGoalsKey("c"))
	_, command := model.Update(projectGoalsKey("y"))
	if command == nil || !model.actionBusy {
		t.Fatal("cancel did not become pending")
	}
	model.Update(projectGoalsKey("esc"))
	message := command()
	model.Update(message)
	if model.actionBusy || model.footer == "cancel requested" || model.cancelConfirm {
		t.Fatalf("escaped cancel response changed current run: busy=%v footer=%q confirm=%v", model.actionBusy, model.footer, model.cancelConfirm)
	}
}

func TestProjectGoalsOverviewInvalidatesPendingArtifact(t *testing.T) {
	state := projectGoalsStateFixture()
	model := newProjectGoalsModel(context.Background(), ProjectGoalsOptions{Theme: theme.Default()})
	model.screen = projectGoalsRun
	model.states = []goal.State{state}
	model.selectedID = state.ID
	model.runTab = 2
	model.artifactSeq = 1
	model.actionBusy = true
	model.Update(projectGoalsKey("1"))
	model.Update(projectGoalsArtifactMsg{goalID: state.ID, sequence: 1, kind: "check", text: "stale"})
	if model.runTab != 0 || model.viewerText != nil || model.actionBusy {
		t.Fatalf("stale artifact changed overview: tab=%d viewer=%q busy=%v", model.runTab, model.viewerText, model.actionBusy)
	}
}

func TestProjectGoalsPollingReconcilesChangedQuestion(t *testing.T) {
	state := projectGoalsStateFixture()
	state.Question = &goal.Question{ID: "question-1", Text: "first question"}
	model := newProjectGoalsModel(context.Background(), ProjectGoalsOptions{Theme: theme.Default()})
	model.screen = projectGoalsRun
	model.states = []goal.State{state}
	model.selectedID = state.ID
	model.questionID = "question-1"
	model.questionText = "first question"
	model.questionEditing = true
	model.answer = []rune("draft answer")
	model.answerCursor = len(model.answer)
	changed := state
	changed.Question = &goal.Question{ID: "question-2", Text: "new question"}
	model.replaceState(changed)
	if model.questionEditing || model.questionID != "question-2" || model.questionText != "new question" || len(model.answer) != 0 {
		t.Fatalf("changed question retained stale editor: editing=%v id=%q text=%q answer=%q", model.questionEditing, model.questionID, model.questionText, model.answer)
	}
	answered := changed
	answered.Question.Answer = "done"
	model.replaceState(answered)
	if model.questionID != "" || model.questionText != "" || len(model.answer) != 0 {
		t.Fatalf("answered question retained editor state: id=%q text=%q answer=%q", model.questionID, model.questionText, model.answer)
	}
}

func TestProjectGoalsPreparedApprovalKeepsFocusedButtonVisible(t *testing.T) {
	prepared := projectGoalsPreparedFixture()
	model := newProjectGoalsModel(context.Background(), ProjectGoalsOptions{Theme: theme.Default()})
	model.prepared = &prepared
	model.draft = prepared.Spec
	model.screen = projectGoalsSetup
	model.setupFocus = model.setupButtonIndex(projectGoalsFieldEdit)
	model.beginSetupEdit()
	model.width, model.height = 50, 16
	out := model.View().Content
	if !strings.Contains(out, "> [ Edit ]") {
		t.Fatalf("focused approval control was scrolled off screen: %q", out)
	}
}

func TestProjectGoalsChecksArtifactLoadsEveryReceipt(t *testing.T) {
	state := projectGoalsStateFixture()
	state.Checks = []goal.CheckResult{{OutputPath: "check-01.txt"}, {OutputPath: "check-02.txt"}}
	reads := 0
	model := newProjectGoalsModel(context.Background(), ProjectGoalsOptions{
		Theme: theme.Default(),
		ReadArtifact: func(_ context.Context, _ string, kind string, index int) (string, error) {
			reads++
			if kind != "check" {
				t.Fatalf("artifact kind=%q, want check", kind)
			}
			return "stdout:\nreceipt-" + strconv.Itoa(index+1) + "\nstderr:\n", nil
		},
	})
	model.screen = projectGoalsRun
	model.states = []goal.State{state}
	model.selectedID = state.ID
	command := model.readArtifact("check", 0)
	message, ok := command().(projectGoalsArtifactMsg)
	if !ok || reads != 2 || strings.Count(message.text, "stdout:") != 2 || !strings.Contains(message.text, "Check 1\nstdout:\nreceipt-1") || !strings.Contains(message.text, "Check 2\nstdout:\nreceipt-2") {
		t.Fatalf("checks artifact=%#v reads=%d", message, reads)
	}
}

func TestProjectGoalsFreshSetupDefaultsToBugFix(t *testing.T) {
	model := newProjectGoalsModel(context.Background(), ProjectGoalsOptions{Theme: theme.Default(), Repository: "/repo"})
	if model.presetID != "bugfix" || model.selectedPreset() == nil || model.selectedPreset().Name != "Bug fix" {
		t.Fatalf("fresh preset=%q selected=%#v", model.presetID, model.selectedPreset())
	}
}

func TestProjectGoalsCollapsedSetupShowsPresetCheckNote(t *testing.T) {
	model := newProjectGoalsModel(context.Background(), ProjectGoalsOptions{Theme: theme.Default(), Repository: t.TempDir()})
	model.screen = projectGoalsSetup
	model.width = 80
	if out := strings.Join(model.renderSetup(76), "\n"); !strings.Contains(out, "Only a whitespace check is suggested") {
		t.Fatalf("collapsed setup omitted the selected preset check note: %q", out)
	}
}

func TestProjectGoalsRunPinsIdentityWhileEvidenceScrolls(t *testing.T) {
	state := projectGoalsStateFixture()
	state.Result = &goal.Result{ReviewText: strings.Repeat("review evidence ", 500)}
	model := newProjectGoalsModel(context.Background(), ProjectGoalsOptions{Theme: theme.Default()})
	model.screen = projectGoalsRun
	model.runTab = 3
	model.states = []goal.State{state}
	model.selectedID = state.ID
	model.width, model.height = 50, 16
	model.scroll = 10000
	model.editorManualScroll = true
	out := model.View().Content
	for _, want := range []string{"Goal ·", "Goal: " + state.ID, "Status:", "Details · JSON"} {
		if !strings.Contains(out, want) {
			t.Fatalf("pinned run header omitted %q: %q", want, out)
		}
	}
}

func TestProjectGoalsInlineActionsShowFocus(t *testing.T) {
	state := projectGoalsStateFixture()
	state.Question = &goal.Question{ID: "question-1", Text: "Need a decision"}
	model := newProjectGoalsModel(context.Background(), ProjectGoalsOptions{Theme: theme.Default()})
	model.screen = projectGoalsRun
	model.states = []goal.State{state}
	model.selectedID = state.ID
	model.questionEditing = true
	model.questionID = state.Question.ID
	model.questionText = state.Question.Text
	model.runFocus = 1
	if out := model.View().Content; !strings.Contains(out, "> [ Send answer ]") {
		t.Fatalf("send button lacks focus marker: %q", out)
	}
	model.questionEditing = false
	model.cancelConfirm = true
	model.runFocus = 1
	if out := model.View().Content; !strings.Contains(out, "> [ Request cancellation ]") {
		t.Fatalf("cancel button lacks focus marker: %q", out)
	}
}

func TestProjectGoalsSingleLineEditorKeepsCaretAndTailVisible(t *testing.T) {
	model := newProjectGoalsModel(context.Background(), ProjectGoalsOptions{Theme: theme.Default()})
	model.screen = projectGoalsSetup
	model.setupFocus = int(projectGoalsFieldRepository)
	model.edit = []rune(strings.Repeat("/very/long/path/", 20) + "tail-marker")
	model.cursor = len(model.edit)
	model.width, model.height = 50, 16
	out := model.View().Content
	if !strings.Contains(out, "tail-marker") || !strings.Contains(out, "▏") {
		t.Fatalf("single-line editor hid caret or tail: %q", out)
	}
}

func TestProjectGoalsPasteOnlyEditsActiveInput(t *testing.T) {
	model := newProjectGoalsModel(context.Background(), ProjectGoalsOptions{Theme: theme.Default()})
	model.screen = projectGoalsSetup
	model.setupFocus = model.setupButtonIndex(projectGoalsFieldPrepare)
	model.edit = []rune("unchanged")
	model.cursor = len(model.edit)
	model.Update(tea.PasteMsg{Content: "ignored"})
	if string(model.edit) != "unchanged" {
		t.Fatalf("button paste changed setup edit: %q", model.edit)
	}
	model.screen = projectGoalsRun
	model.answer = []rune("unchanged")
	model.answerCursor = len(model.answer)
	model.questionEditing = false
	model.Update(tea.PasteMsg{Content: "ignored"})
	if string(model.answer) != "unchanged" {
		t.Fatalf("non-question paste changed answer: %q", model.answer)
	}
}

func TestProjectGoalsBackgroundPollingDoesNotBlockAnswerTyping(t *testing.T) {
	state := projectGoalsStateFixture()
	state.Question = &goal.Question{ID: "question-1", Text: "Need a decision"}
	model := newProjectGoalsModel(context.Background(), ProjectGoalsOptions{Theme: theme.Default()})
	model.screen = projectGoalsRun
	model.states = []goal.State{state}
	model.selectedID = state.ID
	model.questionEditing = true
	model.questionID = state.Question.ID
	model.questionText = state.Question.Text
	model.detailLoading = true
	model.Update(projectGoalsKey("draft"))
	if string(model.answer) != "draft" {
		t.Fatalf("background load blocked answer input: %q", model.answer)
	}
}
