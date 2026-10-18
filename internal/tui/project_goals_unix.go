//go:build unix

package tui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/cameronsjo/forgectl/internal/surface/goal"
	"github.com/cameronsjo/forgectl/internal/termsafe"
	"github.com/cameronsjo/forgectl/internal/theme"
)

// ProjectGoalsOptions supplies the durable goal operations used by the
// Projects goals screen. Callbacks own persistence and process lifecycle; the
// screen only collects, displays, and requests those operations.
type ProjectGoalsOptions struct {
	Theme        theme.Theme
	Input        io.Reader
	Output       io.Writer
	Root         string
	Repository   string
	Model        string
	List         func(context.Context) ([]goal.State, error)
	Prepare      func(context.Context, goal.Spec) (goal.Prepared, error)
	Start        func(context.Context, goal.Prepared) (goal.State, error)
	Load         func(context.Context, string) (goal.State, error)
	Respond      func(context.Context, string, string, string) error
	Cancel       func(context.Context, string) error
	ReadArtifact func(context.Context, string, string, int) (string, error)
}

var ErrProjectGoalsNeedsTerminal = errors.New("project goals requires a terminal")

const (
	projectGoalsPoll              = 2 * time.Second
	projectGoalsWizardSteps       = 8
	projectGoalsEditPasteLimit    = 32 * 1024
	projectGoalsAnswerPasteLimit  = 8 * 1024
	projectGoalsArtifactLineLimit = 4096
	projectGoalsSanitizeChunk     = 256
)

type projectGoalsScreen uint8

const (
	projectGoalsDashboard projectGoalsScreen = iota
	projectGoalsWizard
	projectGoalsReview
	projectGoalsDetail
	projectGoalsQuestion
	projectGoalsCancel
	projectGoalsArtifact
)

type projectGoalsModel struct {
	ctx    context.Context
	th     theme.Theme
	styles theme.Styles
	opts   ProjectGoalsOptions
	screen projectGoalsScreen

	states             []goal.State
	selected           int
	selectedID         string
	loading            bool
	actionBusy         bool
	loadSeq            uint64
	footer             string
	errText            string
	width              int
	height             int
	scroll             int
	editorManualScroll bool

	draft      goal.Spec
	prepared   *goal.Prepared
	wizardStep int
	edit       []rune
	cursor     int

	questionText string
	questionID   string
	answer       []rune
	answerCursor int
	viewerKind   string
	viewerText   []string
	viewerScroll int
}

type projectGoalsListMsg struct {
	sequence uint64
	states   []goal.State
	err      error
}

type projectGoalsPreparedMsg struct {
	prepared goal.Prepared
	err      error
}

type projectGoalsStartedMsg struct {
	state goal.State
	err   error
}

type projectGoalsLoadedMsg struct {
	state goal.State
	err   error
}

type projectGoalsActionMsg struct {
	action string
	err    error
}

type projectGoalsArtifactMsg struct {
	kind string
	text string
	err  error
}

type projectGoalsTickMsg struct{}

// RunProjectGoals starts the interactive Projects goals screen.
func RunProjectGoals(ctx context.Context, opts ProjectGoalsOptions) error {
	if ctx == nil {
		ctx = context.Background()
	}
	model := newProjectGoalsModel(ctx, opts)
	programOptions := []tea.ProgramOption{tea.WithContext(ctx), tea.WithFilter(projectGoalsSuspendFilter)}
	if opts.Input != nil {
		programOptions = append(programOptions, tea.WithInput(opts.Input))
	}
	if opts.Output != nil {
		programOptions = append(programOptions, tea.WithOutput(opts.Output))
	}
	_, err := tea.NewProgram(model, programOptions...).Run()
	return err
}

func projectGoalsSuspendFilter(_ tea.Model, msg tea.Msg) tea.Msg {
	if key, ok := msg.(tea.KeyPressMsg); ok && key.String() == "ctrl+z" {
		return tea.SuspendMsg{}
	}
	return msg
}

func newProjectGoalsModel(ctx context.Context, opts ProjectGoalsOptions) *projectGoalsModel {
	if ctx == nil {
		ctx = context.Background()
	}
	repository := opts.Repository
	if repository == "" {
		if cwd, err := os.Getwd(); err == nil {
			repository, _ = filepath.Abs(cwd)
		}
	}
	model := &projectGoalsModel{
		ctx:    ctx,
		th:     opts.Theme,
		styles: opts.Theme.Styles(),
		opts:   opts,
		draft: goal.Spec{
			Version:      goal.Version,
			Title:        "Repository goal",
			Repository:   repository,
			AllowedPaths: []string{"."},
			Acceptance:   []string{"The requested objective is implemented and independently reviewed."},
			Checks:       [][]string{{"git", "diff", "--check"}},
			Model:        opts.Model,
			LimitMinutes: 10,
		},
	}
	model.beginWizardEdit()
	return model
}

func (model *projectGoalsModel) Init() tea.Cmd {
	return tea.Batch(model.loadList(), model.projectGoalsTick())
}

func (model *projectGoalsModel) loadList() tea.Cmd {
	if model.loading || model.opts.List == nil {
		return nil
	}
	model.loading = true
	model.loadSeq++
	sequence := model.loadSeq
	return func() tea.Msg {
		states, err := model.opts.List(model.ctx)
		return projectGoalsListMsg{sequence: sequence, states: states, err: err}
	}
}

func (model projectGoalsModel) projectGoalsTick() tea.Cmd {
	return tea.Tick(projectGoalsPoll, func(time.Time) tea.Msg { return projectGoalsTickMsg{} })
}

func (model *projectGoalsModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch message := msg.(type) {
	case tea.WindowSizeMsg:
		model.width, model.height = message.Width, message.Height
		model.clampScroll()
		return model, nil
	case projectGoalsTickMsg:
		if model.screen == projectGoalsDashboard {
			return model, tea.Batch(model.loadList(), model.projectGoalsTick())
		}
		if model.screen == projectGoalsDetail && model.selectedID != "" && model.opts.Load != nil && !model.actionBusy {
			return model, tea.Batch(model.loadSelected(), model.projectGoalsTick())
		}
		return model, model.projectGoalsTick()
	case projectGoalsListMsg:
		if message.sequence != model.loadSeq {
			return model, nil
		}
		model.loading = false
		if message.err != nil {
			model.errText = termsafe.SafeLineMax(message.err.Error(), 180)
			model.footer = ""
			return model, nil
		}
		model.errText = ""
		model.states = append([]goal.State(nil), message.states...)
		model.reconcileSelection()
		return model, nil
	case projectGoalsPreparedMsg:
		model.actionBusy = false
		if message.err != nil {
			model.errText = termsafe.SafeLineMax(message.err.Error(), 180)
			model.prepared = nil
			return model, nil
		}
		prepared := message.prepared
		model.prepared = &prepared
		model.draft = prepared.Spec
		model.errText = ""
		model.screen = projectGoalsReview
		model.scroll = 0
		return model, nil
	case projectGoalsStartedMsg:
		model.actionBusy = false
		if message.err != nil {
			model.errText = termsafe.SafeLineMax(message.err.Error(), 180)
			return model, nil
		}
		model.states = append(model.states, message.state)
		model.selectedID = message.state.ID
		model.reconcileSelection()
		model.screen = projectGoalsDetail
		model.scroll = 0
		model.errText = ""
		return model, nil
	case projectGoalsLoadedMsg:
		model.actionBusy = false
		if message.err != nil {
			model.errText = termsafe.SafeLineMax(message.err.Error(), 180)
			return model, nil
		}
		model.replaceState(message.state)
		model.errText = ""
		return model, nil
	case projectGoalsActionMsg:
		model.actionBusy = false
		if message.err != nil {
			model.errText = termsafe.SafeLineMax(message.err.Error(), 180)
			return model, nil
		}
		model.footer = message.action
		model.errText = ""
		if model.screen == projectGoalsQuestion || model.screen == projectGoalsCancel {
			model.screen = projectGoalsDetail
			model.editorManualScroll = false
		}
		return model, model.loadSelected()
	case projectGoalsArtifactMsg:
		model.actionBusy = false
		if message.err != nil {
			model.errText = termsafe.SafeLineMax(message.err.Error(), 180)
			return model, nil
		}
		model.viewerKind = message.kind
		model.viewerText = safeGoalArtifactLines(message.text)
		model.viewerScroll = 0
		model.screen = projectGoalsArtifact
		model.errText = ""
		return model, nil
	case tea.PasteMsg:
		return model.updatePaste(message.Content)
	case tea.KeyPressMsg:
		return model.updateKey(message)
	}
	return model, nil
}

func (model *projectGoalsModel) updateKey(key tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	name := projectGoalsKeyName(key)
	switch model.screen {
	case projectGoalsDashboard:
		return model.updateDashboard(name)
	case projectGoalsWizard:
		return model.updateWizard(name, key.Text)
	case projectGoalsReview:
		return model.updateReview(name)
	case projectGoalsDetail:
		return model.updateDetail(name)
	case projectGoalsQuestion:
		return model.updateQuestion(name, key.Text)
	case projectGoalsCancel:
		return model.updateCancel(name)
	case projectGoalsArtifact:
		return model.updateArtifact(name)
	default:
		return model, nil
	}
}

func (model *projectGoalsModel) updatePaste(content string) (tea.Model, tea.Cmd) {
	if !utf8.ValidString(content) {
		model.errText = "pasted text is not valid UTF-8"
		return model, nil
	}
	switch model.screen {
	case projectGoalsWizard:
		if len(string(model.edit))+len(content) > projectGoalsEditPasteLimit {
			model.errText = "pasted goal text exceeds 32 KiB"
			return model, nil
		}
		model.insertEdit(content)
		model.errText = ""
		model.editorManualScroll = false
	case projectGoalsQuestion:
		if len(string(model.answer))+len(content) > projectGoalsAnswerPasteLimit {
			model.errText = "pasted answer exceeds 8 KiB"
			return model, nil
		}
		model.insertAnswer(content)
		model.errText = ""
		model.editorManualScroll = false
	}
	return model, nil
}

func (model *projectGoalsModel) updateDashboard(name string) (tea.Model, tea.Cmd) {
	switch name {
	case "q", "ctrl+c":
		return model, tea.Quit
	case "n", "s":
		model.startWizard()
		return model, nil
	case "up", "k":
		model.moveSelection(-1)
	case "down", "j":
		model.moveSelection(1)
	case "enter":
		if state, ok := model.selectedState(); ok {
			model.selectedID = state.ID
			model.screen = projectGoalsDetail
			model.scroll = 0
			return model, model.loadSelected()
		}
	case "r":
		return model, model.loadList()
	case "c":
		if state, ok := model.selectedState(); ok && !state.Phase.Terminal() {
			model.selectedID = state.ID
			model.screen = projectGoalsCancel
			model.scroll = 0
		}
	}
	return model, nil
}

func (model *projectGoalsModel) updateWizard(name, text string) (tea.Model, tea.Cmd) {
	if name == "ctrl+c" {
		return model, tea.Quit
	}
	if name == "esc" {
		if model.wizardStep == 0 {
			model.screen = projectGoalsDashboard
		} else {
			model.wizardStep--
			model.beginWizardEdit()
		}
		return model, nil
	}
	if name == "ctrl+u" {
		model.edit = nil
		model.cursor = 0
		model.editorManualScroll = false
		return model, nil
	}
	if name == "ctrl+up" || name == "ctrl+down" {
		if name == "ctrl+up" {
			model.scroll--
		} else {
			model.scroll++
		}
		model.editorManualScroll = true
		model.clampScroll()
		return model, nil
	}
	if name == "left" {
		if model.cursor > 0 {
			model.cursor--
		}
		model.editorManualScroll = false
		return model, nil
	}
	if name == "right" {
		if model.cursor < len(model.edit) {
			model.cursor++
		}
		model.editorManualScroll = false
		return model, nil
	}
	if name == "ctrl+j" || (name == "enter" && (model.wizardStep == 2 || model.wizardStep == 4)) {
		model.insertEdit("\n")
		model.editorManualScroll = false
		return model, nil
	}
	if name == "backspace" {
		if model.cursor > 0 {
			model.edit = append(model.edit[:model.cursor-1], model.edit[model.cursor:]...)
			model.cursor--
		}
		model.editorManualScroll = false
		return model, nil
	}
	if name == "enter" || name == "ctrl+s" {
		if err := model.commitWizardStep(); err != nil {
			model.errText = termsafe.SafeLineMax(err.Error(), 180)
			return model, nil
		}
		model.errText = ""
		if model.wizardStep == projectGoalsWizardSteps-1 {
			return model, model.prepareGoal()
		}
		model.wizardStep++
		model.beginWizardEdit()
		model.scroll = 0
		return model, nil
	}
	if text != "" && !strings.HasPrefix(name, "ctrl+") {
		model.insertEdit(text)
		model.editorManualScroll = false
	}
	return model, nil
}

func (model *projectGoalsModel) updateReview(name string) (tea.Model, tea.Cmd) {
	switch name {
	case "q", "ctrl+c":
		return model, tea.Quit
	case "esc":
		model.screen = projectGoalsWizard
		model.wizardStep = projectGoalsWizardSteps - 1
		model.beginWizardEdit()
	case "up", "k":
		model.scroll--
	case "down", "j":
		model.scroll++
	case "s":
		if model.prepared == nil {
			model.errText = "review is not ready; press esc to edit the goal"
			return model, nil
		}
		model.actionBusy = true
		prepared := *model.prepared
		return model, func() tea.Msg {
			if model.opts.Start == nil {
				return projectGoalsStartedMsg{err: errors.New("goal start is unavailable")}
			}
			state, err := model.opts.Start(model.ctx, prepared)
			return projectGoalsStartedMsg{state: state, err: err}
		}
	}
	model.clampScroll()
	return model, nil
}

func (model *projectGoalsModel) updateDetail(name string) (tea.Model, tea.Cmd) {
	switch name {
	case "q", "ctrl+c":
		return model, tea.Quit
	case "esc":
		model.screen = projectGoalsDashboard
		model.scroll = 0
	case "up", "k":
		model.scroll--
	case "down", "j":
		model.scroll++
	case "a":
		if state, ok := model.selectedState(); ok && state.Question != nil && state.Question.Answer == "" {
			model.questionID = state.Question.ID
			model.questionText = state.Question.Text
			model.answer = nil
			model.answerCursor = 0
			model.screen = projectGoalsQuestion
			model.scroll = 0
			model.editorManualScroll = false
		}
	case "c":
		if state, ok := model.selectedState(); ok && !state.Phase.Terminal() {
			model.screen = projectGoalsCancel
			model.scroll = 0
		}
	case "d":
		return model, model.readArtifact("patch", 0)
	case "v":
		return model, model.readArtifact("check", 0)
	case "e":
		return model, model.readArtifact("json", 0)
	}
	model.clampScroll()
	return model, nil
}

func (model *projectGoalsModel) updateQuestion(name, text string) (tea.Model, tea.Cmd) {
	if name == "ctrl+c" {
		return model, tea.Quit
	}
	if name == "esc" {
		model.screen = projectGoalsDetail
		model.editorManualScroll = false
		return model, nil
	}
	if name == "ctrl+up" || name == "ctrl+down" {
		if name == "ctrl+up" {
			model.scroll--
		} else {
			model.scroll++
		}
		model.editorManualScroll = true
		model.clampScroll()
		return model, nil
	}
	if name == "backspace" {
		if model.answerCursor > 0 {
			model.answer = append(model.answer[:model.answerCursor-1], model.answer[model.answerCursor:]...)
			model.answerCursor--
		}
		model.editorManualScroll = false
		return model, nil
	}
	if name == "left" {
		if model.answerCursor > 0 {
			model.answerCursor--
		}
		model.editorManualScroll = false
		return model, nil
	}
	if name == "right" {
		if model.answerCursor < len(model.answer) {
			model.answerCursor++
		}
		model.editorManualScroll = false
		return model, nil
	}
	if name == "enter" {
		answer := strings.TrimSpace(string(model.answer))
		if answer == "" {
			model.errText = "answer cannot be empty"
			return model, nil
		}
		if model.opts.Respond == nil {
			model.errText = "answering questions is unavailable"
			return model, nil
		}
		model.actionBusy = true
		goalID, questionID := model.selectedID, model.questionID
		return model, func() tea.Msg {
			return projectGoalsActionMsg{action: "answer sent", err: model.opts.Respond(model.ctx, goalID, questionID, answer)}
		}
	}
	if text != "" && !strings.HasPrefix(name, "ctrl+") {
		model.insertAnswer(text)
		model.editorManualScroll = false
	}
	return model, nil
}

func (model *projectGoalsModel) updateCancel(name string) (tea.Model, tea.Cmd) {
	switch name {
	case "q", "ctrl+c":
		return model, tea.Quit
	case "esc", "n":
		model.screen = projectGoalsDetail
	case "y":
		if model.opts.Cancel == nil {
			model.errText = "cancel is unavailable"
			model.screen = projectGoalsDetail
			return model, nil
		}
		model.actionBusy = true
		goalID := model.selectedID
		return model, func() tea.Msg {
			return projectGoalsActionMsg{action: "cancel requested", err: model.opts.Cancel(model.ctx, goalID)}
		}
	}
	return model, nil
}

func (model *projectGoalsModel) updateArtifact(name string) (tea.Model, tea.Cmd) {
	switch name {
	case "q", "ctrl+c", "esc":
		model.screen = projectGoalsDetail
		model.viewerText = nil
		model.viewerScroll = 0
	case "up", "k":
		model.viewerScroll--
	case "down", "j":
		model.viewerScroll++
	}
	model.clampViewerScroll()
	return model, nil
}

func (model *projectGoalsModel) startWizard() {
	model.screen = projectGoalsWizard
	model.wizardStep = 0
	model.prepared = nil
	model.errText = ""
	model.scroll = 0
	model.editorManualScroll = false
	model.beginWizardEdit()
}

func (model *projectGoalsModel) beginWizardEdit() {
	var value string
	switch model.wizardStep {
	case 0:
		value = model.draft.Repository
	case 1:
		value = model.draft.Title
	case 2:
		value = model.draft.Objective
	case 3:
		value = strings.Join(model.draft.AllowedPaths, ", ")
	case 4:
		value = strings.Join(model.draft.Acceptance, "\n")
	case 5:
		// termsafe:allow-raw-json bounded form input rendered inside the TUI
		encoded, _ := json.Marshal(model.draft.Checks)
		value = string(encoded)
	case 6:
		value = model.draft.Model
	case 7:
		value = strconv.Itoa(model.draft.LimitMinutes)
	}
	model.edit = []rune(value)
	model.cursor = len(model.edit)
	model.editorManualScroll = false
}

func (model *projectGoalsModel) commitWizardStep() error {
	value := strings.TrimSpace(string(model.edit))
	switch model.wizardStep {
	case 0:
		model.draft.Repository = value
	case 1:
		model.draft.Title = value
	case 2:
		model.draft.Objective = value
	case 3:
		model.draft.AllowedPaths = splitGoalList(value)
	case 4:
		model.draft.Acceptance = splitGoalLines(value)
	case 5:
		var checks [][]string
		if err := json.Unmarshal([]byte(value), &checks); err != nil {
			return fmt.Errorf("checks must be a JSON array of argument arrays: %w", err)
		}
		model.draft.Checks = checks
	case 6:
		model.draft.Model = value
	case 7:
		minutes, err := strconv.Atoi(value)
		if err != nil {
			return errors.New("duration must be a number from 1 to 30 minutes")
		}
		model.draft.LimitMinutes = minutes
	}
	return nil
}

func (model *projectGoalsModel) prepareGoal() tea.Cmd {
	if model.opts.Prepare == nil {
		model.errText = "goal preparation is unavailable"
		return nil
	}
	model.draft.Version = goal.Version
	if err := model.draft.Validate(); err != nil {
		model.errText = termsafe.SafeLineMax(err.Error(), 180)
		return nil
	}
	model.actionBusy = true
	spec := model.draft
	return func() tea.Msg {
		prepared, err := model.opts.Prepare(model.ctx, spec)
		return projectGoalsPreparedMsg{prepared: prepared, err: err}
	}
}

func (model *projectGoalsModel) loadSelected() tea.Cmd {
	if model.selectedID == "" || model.opts.Load == nil || model.actionBusy {
		return nil
	}
	model.actionBusy = true
	goalID := model.selectedID
	return func() tea.Msg {
		state, err := model.opts.Load(model.ctx, goalID)
		return projectGoalsLoadedMsg{state: state, err: err}
	}
}

func (model *projectGoalsModel) readArtifact(kind string, index int) tea.Cmd {
	if model.selectedID == "" || model.opts.ReadArtifact == nil {
		model.errText = "artifact viewing is unavailable"
		return nil
	}
	model.actionBusy = true
	goalID := model.selectedID
	return func() tea.Msg {
		text, err := model.opts.ReadArtifact(model.ctx, goalID, kind, index)
		return projectGoalsArtifactMsg{kind: kind, text: text, err: err}
	}
}

func (model *projectGoalsModel) replaceState(state goal.State) {
	for index := range model.states {
		if model.states[index].ID == state.ID {
			model.states[index] = state
			return
		}
	}
	model.states = append(model.states, state)
}

func (model *projectGoalsModel) reconcileSelection() {
	if model.selectedID != "" {
		for index := range model.states {
			if model.states[index].ID == model.selectedID {
				model.selected = index
				return
			}
		}
	}
	if len(model.states) == 0 {
		model.selected = 0
		model.selectedID = ""
		return
	}
	if model.selected >= len(model.states) {
		model.selected = len(model.states) - 1
	}
	model.selectedID = model.states[model.selected].ID
}

func (model *projectGoalsModel) moveSelection(delta int) {
	if len(model.states) == 0 {
		return
	}
	model.selected += delta
	if model.selected < 0 {
		model.selected = 0
	}
	if model.selected >= len(model.states) {
		model.selected = len(model.states) - 1
	}
	model.selectedID = model.states[model.selected].ID
}

func (model projectGoalsModel) selectedState() (goal.State, bool) {
	if model.selectedID != "" {
		for _, state := range model.states {
			if state.ID == model.selectedID {
				return state, true
			}
		}
	}
	if model.selected >= 0 && model.selected < len(model.states) {
		return model.states[model.selected], true
	}
	return goal.State{}, false
}

func (model *projectGoalsModel) insertEdit(text string) {
	if !utf8.ValidString(text) {
		return
	}
	runes := []rune(text)
	model.edit = append(model.edit[:model.cursor], append(runes, model.edit[model.cursor:]...)...)
	model.cursor += len(runes)
}

func (model *projectGoalsModel) insertAnswer(text string) {
	if !utf8.ValidString(text) {
		return
	}
	runes := []rune(text)
	model.answer = append(model.answer[:model.answerCursor], append(runes, model.answer[model.answerCursor:]...)...)
	model.answerCursor += len(runes)
}

func (model projectGoalsModel) View() tea.View {
	content := model.render()
	view := tea.NewView(content)
	view.AltScreen = true
	view.WindowTitle = "forgectl Projects Goals"
	return view
}

func (model projectGoalsModel) render() string {
	width, height := model.width, model.height
	if width <= 0 {
		width = 80
	}
	if height <= 0 {
		height = 24
	}
	contentWidth := width
	if width >= 40 {
		contentWidth -= 4
	}
	if contentWidth < 10 {
		contentWidth = 10
	}
	var lines []string
	lines = append(lines, model.styles.Brand.Render("●")+" "+model.styles.Header.Render("Projects · Goals"))
	switch model.screen {
	case projectGoalsDashboard:
		lines = append(lines, model.renderDashboard(contentWidth)...)
	case projectGoalsWizard:
		lines = append(lines, model.renderWizard(contentWidth)...)
	case projectGoalsReview:
		lines = append(lines, model.renderReview(contentWidth)...)
	case projectGoalsDetail:
		lines = append(lines, model.renderDetail(contentWidth)...)
	case projectGoalsQuestion:
		lines = append(lines, model.renderQuestion(contentWidth)...)
	case projectGoalsCancel:
		lines = append(lines, model.renderCancel(contentWidth)...)
	case projectGoalsArtifact:
		lines = append(lines, model.renderArtifact(contentWidth)...)
	}
	footer := model.footerText()
	available := goalMax(height-1, 1)
	scroll := model.scroll
	if (model.screen == projectGoalsWizard || model.screen == projectGoalsQuestion) && !model.editorManualScroll {
		scroll = model.editorScroll(lines, contentWidth, available)
	}
	if len(lines) > available {
		start := goalMin(scroll, len(lines)-available)
		lines = lines[start:]
		if len(lines) > available {
			lines = lines[:available]
		}
	}
	for len(lines) < available {
		lines = append(lines, "")
	}
	lines = append(lines, model.styles.Meta.Render(footer))
	for index, line := range lines {
		lines[index] = strings.Repeat(" ", goalMax((width-contentWidth)/2, 0)) + fitProjectAgentsLine(line, contentWidth)
	}
	return strings.Join(lines, "\n")
}

func (model projectGoalsModel) renderDashboard(width int) []string {
	lines := []string{model.styles.Meta.Render("Start a bounded native Codex goal; leaving this screen detaches.")}
	if model.opts.Root != "" {
		lines = appendGoalField(lines, "Registry", model.opts.Root, width)
	}
	lines = append(lines, "")
	if model.errText != "" {
		lines = append(lines, model.styles.Danger.Render("! "+model.errText), "")
	}
	if model.loading && len(model.states) == 0 {
		lines = append(lines, model.styles.Meta.Render("Loading goals…"))
		return lines
	}
	if len(model.states) == 0 {
		lines = append(lines, model.styles.Fg.Render("No goals yet."), model.styles.Meta.Render("Press n to start a repository goal."))
		return lines
	}
	for index, state := range model.states {
		marker := "  "
		if index == model.selected {
			marker = "> "
		}
		label := goalPhaseLabel(state.Phase)
		line := fmt.Sprintf("%s%-12s %-18s %s", marker, label, safeGoalField(state.ID, 18), safeGoalField(state.Spec.Title, goalMax(width-42, 1)))
		lines = append(lines, line)
		if index == model.selected {
			lines = append(lines, model.styles.Meta.Render("   "+safeGoalField(state.Summary, width-6)))
		}
	}
	return lines
}

func (model projectGoalsModel) renderWizard(width int) []string {
	labels := []string{"Repository", "Title", "Objective", "Allowed paths", "Acceptance", "Checks (JSON arrays)", "OpenAI model", "Duration (minutes)"}
	hints := []string{"absolute local checkout", "short name", "what should change; enter or ctrl+j adds a line; ctrl+s advances", "comma-separated literals; . means whole repository", "one criterion per line; enter or ctrl+j adds a line; ctrl+s advances", "example: [[\"git\",\"diff\",\"--check\"]]", "exact model ID", "1–30; not a spending cap"}
	lines := []string{model.styles.Header.Render(fmt.Sprintf("Start goal · step %d/%d · %s", model.wizardStep+1, projectGoalsWizardSteps, labels[model.wizardStep])), model.styles.Meta.Render(hints[model.wizardStep]), ""}
	for _, wrapped := range goalWrap(goalCursorText(model.edit, model.cursor), width-2) {
		lines = append(lines, model.styles.Fg.Render(wrapped))
	}
	if len(model.edit) == 0 {
		lines = append(lines, model.styles.Dim.Render("type a value"))
	}
	if model.errText != "" {
		lines = append(lines, "", model.styles.Danger.Render("! "+model.errText))
	}
	return lines
}

func (model projectGoalsModel) renderReview(width int) []string {
	lines := []string{model.styles.Header.Render("Review before start"), model.styles.Meta.Render("No model runs until you press s to start."), ""}
	prepared := model.prepared
	spec := model.draft
	if prepared != nil {
		spec = prepared.Spec
	}
	lines = appendGoalField(lines, "Repository", spec.Repository, width)
	lines = appendGoalField(lines, "Title", spec.Title, width)
	lines = appendGoalField(lines, "Base revision", spec.BaseRevision, width)
	lines = appendGoalField(lines, "Source branch", preparedString(prepared, func(value goal.Prepared) string { return value.SourceBranch }), width)
	lines = appendGoalField(lines, "Model", spec.Model, width)
	lines = appendGoalField(lines, "Duration", fmt.Sprintf("%d minutes", spec.LimitMinutes), width)
	lines = appendGoalField(lines, "Allowed", strings.Join(spec.AllowedPaths, ", "), width)
	if prepared != nil {
		lines = appendGoalField(lines, "Source dirty", strconv.FormatBool(prepared.SourceDirty), width)
		for _, warning := range prepared.Warnings {
			for index, line := range goalWrap("warning: "+warning, width) {
				lines = append(lines, model.styles.Warn.Render(line))
				if index == 0 {
					continue
				}
			}
		}
	}
	lines = append(lines, "", model.styles.Header.Render("Objective"))
	lines = append(lines, goalWrap(spec.Objective, width)...)
	lines = append(lines, model.styles.Header.Render("Acceptance"))
	for _, criterion := range spec.Acceptance {
		for index, line := range goalWrap(criterion, width-2) {
			prefix := "  "
			if index == 0 {
				prefix = "- "
			}
			lines = append(lines, prefix+line)
		}
	}
	lines = append(lines, model.styles.Header.Render("Checks"))
	for _, check := range spec.Checks {
		// termsafe:allow-raw-json bounded check display rendered inside the TUI
		encoded, _ := json.Marshal(check)
		lines = append(lines, goalWrap(string(encoded), width)...)
	}
	if model.errText != "" {
		lines = append(lines, "", model.styles.Danger.Render("! "+model.errText))
	}
	return lines
}

func (model projectGoalsModel) renderDetail(width int) []string {
	state, ok := model.selectedState()
	if !ok {
		return []string{model.styles.Meta.Render("No goal selected."), ""}
	}
	lines := []string{model.styles.Header.Render(safeGoalField(state.Spec.Title, width))}
	lines = appendGoalField(lines, "Status", goalPhaseLabel(state.Phase), width)
	lines = appendGoalField(lines, "Goal", state.ID, width)
	lines = appendGoalField(lines, "Summary", state.Summary, width)
	lines = appendGoalField(lines, "Repository", state.Spec.Repository, width)
	lines = appendGoalField(lines, "Worktree", state.Worktree, width)
	lines = appendGoalField(lines, "Branch", state.Branch, width)
	lines = appendGoalField(lines, "Base", state.Spec.BaseRevision, width)
	lines = appendGoalField(lines, "Supervisor", supervisorLabel(state), width)
	lines = append(lines, "")
	if state.Question != nil && state.Question.Answer == "" {
		lines = append(lines, model.styles.Warn.Render("Cameron decision needed"))
		for index, line := range goalWrap(state.Question.Text, width) {
			lines = append(lines, model.styles.Fg.Render(line))
			if index == 0 {
				continue
			}
		}
		lines = append(lines, "")
	}
	if state.Result != nil {
		result := state.Result
		lines = append(lines, model.styles.Header.Render("Result"))
		lines = appendGoalField(lines, "Review", boolLabel(result.ReviewPassed), width)
		lines = appendGoalField(lines, "Checks", boolLabel(result.ChecksPassed), width)
		lines = appendGoalField(lines, "Source unchanged", boolLabel(result.SourceUnchanged), width)
		lines = appendGoalField(lines, "Patch", result.PatchPath, width)
		lines = appendGoalField(lines, "Patch SHA", result.PatchSHA256, width)
		lines = append(lines, "")
	}
	if len(state.Checks) > 0 {
		lines = append(lines, model.styles.Header.Render("Checks"))
		for index, check := range state.Checks {
			lines = appendGoalField(lines, fmt.Sprintf("%d. %s · exit %d", index+1, boolLabel(check.Passed), check.ExitCode), check.OutputPath, width)
		}
	}
	if state.Error != "" {
		for _, line := range goalWrap("error: "+state.Error, width) {
			lines = append(lines, model.styles.Danger.Render(line))
		}
	}
	return lines
}

func (model projectGoalsModel) renderQuestion(width int) []string {
	lines := []string{model.styles.Header.Render("Question for Cameron"), model.styles.Meta.Render("This answer clarifies the approved goal; it cannot grant permissions."), ""}
	lines = append(lines, goalWrap(model.questionText, width)...)
	lines = append(lines, "", model.styles.Header.Render("Answer"))
	for _, line := range goalWrap(goalCursorText(model.answer, model.answerCursor), width) {
		lines = append(lines, model.styles.Fg.Render(line))
	}
	if model.errText != "" {
		lines = append(lines, "", model.styles.Danger.Render("! "+model.errText))
	}
	return lines
}

func (model projectGoalsModel) editorScroll(lines []string, width, available int) int {
	maxStart := goalMax(len(lines)-available, 0)
	start := model.scroll
	cursorLine := model.editorCursorLine(width)
	if cursorLine < start {
		start = cursorLine
	}
	if cursorLine >= start+available {
		start = cursorLine - available + 1
	}
	return goalMin(goalMax(start, 0), maxStart)
}

func (model projectGoalsModel) editorCursorLine(width int) int {
	switch model.screen {
	case projectGoalsWizard:
		return 1 + 3 + goalCursorLine(model.edit, model.cursor, goalMax(width-2, 1))
	case projectGoalsQuestion:
		questionLines := goalWrap(model.questionText, width)
		return 1 + 3 + len(questionLines) + 2 + goalCursorLine(model.answer, model.answerCursor, width)
	default:
		return 0
	}
}

func goalCursorLine(value []rune, cursor, width int) int {
	if cursor < 0 {
		cursor = 0
	}
	if cursor > len(value) {
		cursor = len(value)
	}
	remaining := strings.Count(string(value[:cursor]), "▏")
	lines := goalWrap(goalCursorText(value, cursor), width)
	for index, line := range lines {
		markers := strings.Count(line, "▏")
		if remaining < markers {
			return index
		}
		remaining -= markers
	}
	return goalMax(len(lines)-1, 0)
}

func (model projectGoalsModel) renderCancel(width int) []string {
	return []string{model.styles.Header.Render("Cancel goal?"), model.styles.Warn.Render("Cancellation asks the owning supervisor to stop its children."), "", goalReviewLine("Goal", model.selectedID), model.styles.Fg.Render("Press y to request cancellation; n or esc keeps it running."), ""}
}

func (model projectGoalsModel) renderArtifact(width int) []string {
	lines := []string{model.styles.Header.Render("Artifact · " + model.viewerKind), model.styles.Meta.Render("Esc returns; ↑/↓ or j/k scroll."), ""}
	start := goalMin(model.viewerScroll, goalMax(len(model.viewerText)-1, 0))
	for _, line := range model.viewerText[start:] {
		lines = append(lines, safeGoalField(line, width))
	}
	return lines
}

func (model projectGoalsModel) footerText() string {
	compact := model.width > 0 && model.width < 70
	switch model.screen {
	case projectGoalsDashboard:
		if compact {
			return "j/k move · enter open · n start · c cancel · q quit"
		}
		return "↑↓/jk select · enter details · n start · c cancel · r refresh · q detach"
	case projectGoalsWizard:
		if compact {
			if model.wizardStep == 2 || model.wizardStep == 4 {
				return "^c quit · esc back · ctrl+s next · enter nl"
			}
			return "^c quit · esc back · ctrl+s/enter next"
		}
		if model.wizardStep == 2 || model.wizardStep == 4 {
			return "ctrl+s next · enter newline · esc back · ctrl+c cancel"
		}
		return "enter next · esc back · ctrl+c cancel"
	case projectGoalsReview:
		if compact {
			return "s start · esc edit · j/k scroll · q quit"
		}
		return "s start · esc edit · ↑↓/jk scroll · q detach"
	case projectGoalsDetail:
		if compact {
			return "a answer · c cancel · d/v/e view · esc back · q quit"
		}
		return "a answer · c cancel · d patch · v checks · e JSON · esc list · q detach"
	case projectGoalsQuestion:
		if compact {
			return "^c quit · esc back · ^↑/↓ scroll · enter send"
		}
		return "enter send answer · ctrl+↑/↓ scroll · esc back · ctrl+c cancel"
	case projectGoalsCancel:
		if compact {
			return "y cancel · n keep · q quit"
		}
		return "y request cancel · n/esc keep running · q detach"
	case projectGoalsArtifact:
		if compact {
			return "j/k scroll · esc back · q quit"
		}
		return "↑↓/jk scroll · esc back · q detach"
	default:
		return "q detach"
	}
}

func (model *projectGoalsModel) clampScroll() {
	if model.scroll < 0 {
		model.scroll = 0
	}
	if model.scroll > 10000 {
		model.scroll = 10000
	}
}

func (model *projectGoalsModel) clampViewerScroll() {
	if model.viewerScroll < 0 {
		model.viewerScroll = 0
	}
	maxScroll := goalMax(len(model.viewerText)-1, 0)
	if model.viewerScroll > maxScroll {
		model.viewerScroll = maxScroll
	}
}

func projectGoalsKeyName(key tea.KeyPressMsg) string {
	name := key.String()
	if name != "" {
		return name
	}
	if key.Text != "" {
		return key.Text
	}
	switch key.Code {
	case tea.KeyBackspace:
		return "backspace"
	case tea.KeyEnter:
		return "enter"
	case tea.KeyEscape:
		return "esc"
	case tea.KeyLeft:
		return "left"
	case tea.KeyRight:
		return "right"
	case tea.KeyUp:
		return "up"
	case tea.KeyDown:
		return "down"
	}
	return ""
}

func splitGoalList(value string) []string {
	return splitGoalLines(strings.ReplaceAll(value, ",", "\n"))
}

func splitGoalLines(value string) []string {
	parts := strings.Split(value, "\n")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}

func goalPhaseLabel(phase goal.Phase) string { return string(phase) }

func supervisorLabel(state goal.State) string {
	if state.CleanupComplete {
		return "stopped; cleanup confirmed"
	}
	if state.CancelRequested {
		return "canceling"
	}
	if state.SupervisorActive {
		return "active"
	}
	if state.Phase == goal.PhaseInterrupted || state.Phase == goal.PhaseUnknown {
		return "unknown; inspect receipts; no replay"
	}
	return "inactive"
}

func boolLabel(value bool) string {
	if value {
		return "pass"
	}
	return "not passed"
}

func goalReviewLine(label, value string) string { return label + ": " + safeGoalField(value, 512) }

func appendGoalField(lines []string, label, value string, width int) []string {
	prefix := label + ": "
	available := width - lipgloss.Width(prefix)
	if available < 1 {
		available = 1
	}
	wrapped := goalWrap(value, available)
	if len(wrapped) == 0 {
		return append(lines, prefix)
	}
	lines = append(lines, prefix+wrapped[0])
	for _, line := range wrapped[1:] {
		lines = append(lines, "  "+line)
	}
	return lines
}

func goalCursorText(value []rune, cursor int) string {
	if cursor < 0 {
		cursor = 0
	}
	if cursor > len(value) {
		cursor = len(value)
	}
	return string(value[:cursor]) + "▏" + string(value[cursor:])
}

func safeGoalField(value string, width int) string {
	return fitProjectAgentsLine(termsafe.SafeLineMax(value, projectGoalsArtifactLineLimit), width)
}

func safeGoalArtifactLines(value string) []string {
	raw := strings.Split(value, "\n")
	lines := make([]string, len(raw))
	for index, line := range raw {
		lines[index] = safeGoalField(line, projectGoalsArtifactLineLimit)
	}
	return lines
}

func safeGoalText(value string) string {
	runes := []rune(value)
	var safe strings.Builder
	for start := 0; start < len(runes); start += projectGoalsSanitizeChunk {
		end := start + projectGoalsSanitizeChunk
		if end > len(runes) {
			end = len(runes)
		}
		safe.WriteString(termsafe.SafeLineMax(string(runes[start:end]), projectGoalsArtifactLineLimit))
	}
	return safe.String()
}

func preparedString(prepared *goal.Prepared, fn func(goal.Prepared) string) string {
	if prepared == nil {
		return "pending"
	}
	return fn(*prepared)
}

func goalWrap(value string, width int) []string {
	value = safeGoalText(value)
	if width < 1 {
		width = 1
	}
	var result []string
	for _, raw := range strings.Split(value, "\n") {
		words := strings.Fields(raw)
		if len(words) == 0 {
			result = append(result, "")
			continue
		}
		line := ""
		for _, word := range words {
			for len(word) > 0 {
				segment := word
				if lipgloss.Width(segment) > width {
					runes := []rune(segment)
					cut := 0
					for cut < len(runes) && lipgloss.Width(string(runes[:cut+1])) <= width {
						cut++
					}
					if cut == 0 {
						cut = 1
					}
					segment, word = string(runes[:cut]), string(runes[cut:])
				} else {
					word = ""
				}
				if line == "" {
					line = segment
				} else if lipgloss.Width(line)+1+lipgloss.Width(segment) <= width {
					line += " " + segment
				} else {
					result = append(result, line)
					line = segment
				}
				if len(word) > 0 && line != "" {
					result = append(result, line)
					line = ""
				}
			}
		}
		result = append(result, line)
	}
	return result
}

func goalMin(left, right int) int {
	if left < right {
		return left
	}
	return right
}

func goalMax(left, right int) int {
	if left > right {
		return left
	}
	return right
}
