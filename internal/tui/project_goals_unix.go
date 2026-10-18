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
	"github.com/charmbracelet/x/ansi"

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
	projectGoalsEditPasteLimit    = 32 * 1024
	projectGoalsAnswerPasteLimit  = 8 * 1024
	projectGoalsArtifactLineLimit = 4096
	projectGoalsSanitizeChunk     = 256
)

type projectGoalsScreen uint8

const (
	projectGoalsDashboard projectGoalsScreen = iota
	projectGoalsSetup
	projectGoalsRun
)

type projectGoalsSetupField uint8

const (
	projectGoalsFieldRepository projectGoalsSetupField = iota
	projectGoalsFieldPreset
	projectGoalsFieldGoal
	projectGoalsFieldAdvanced
	projectGoalsFieldTitle
	projectGoalsFieldScope
	projectGoalsFieldAcceptance
	projectGoalsFieldChecks
	projectGoalsFieldModel
	projectGoalsFieldDuration
	projectGoalsFieldPrepare
	projectGoalsFieldEdit
	projectGoalsFieldStart
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
	detailLoading      bool
	actionBusy         bool
	loadSeq            uint64
	footer             string
	errText            string
	width              int
	height             int
	scroll             int
	editorManualScroll bool

	draft    goal.Spec
	prepared *goal.Prepared
	edit     []rune
	cursor   int

	questionText string
	questionID   string
	answer       []rune
	answerCursor int
	viewerKind   string
	viewerText   []string
	viewerScroll int

	setupAdvanced   bool
	setupFocus      int
	presetID        string
	titleExplicit   bool
	runTab          int
	runFocus        int
	prepareDigest   string
	prepareSeq      uint64
	actionSeq       uint64
	detailSeq       uint64
	artifactSeq     uint64
	questionEditing bool
	cancelConfirm   bool
}

type projectGoalsListMsg struct {
	sequence uint64
	states   []goal.State
	err      error
}

type projectGoalsPreparedMsg struct {
	prepared goal.Prepared
	digest   string
	sequence uint64
	err      error
}

type projectGoalsStartedMsg struct {
	state goal.State
	err   error
}

type projectGoalsLoadedMsg struct {
	goalID   string
	sequence uint64
	state    goal.State
	err      error
}

type projectGoalsActionMsg struct {
	goalID     string
	questionID string
	sequence   uint64
	action     string
	err        error
}

type projectGoalsArtifactMsg struct {
	goalID   string
	sequence uint64
	kind     string
	text     string
	err      error
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
		ctx:      ctx,
		th:       opts.Theme,
		styles:   opts.Theme.Styles(),
		presetID: "bugfix",
		opts:     opts,
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
	model.applyPreset("bugfix", false)
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
		if model.isRunScreen() && model.selectedID != "" && model.opts.Load != nil && !model.actionBusy && !model.detailLoading {
			return model, tea.Batch(model.loadSelected(), model.projectGoalsTick())
		}
		return model, model.projectGoalsTick()
	case projectGoalsListMsg:
		if message.sequence != model.loadSeq {
			return model, nil
		}
		model.loading = false
		if model.screen != projectGoalsDashboard {
			return model, nil
		}
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
		if (message.sequence != 0 && message.sequence != model.prepareSeq) || (message.sequence == 0 && message.digest != "" && message.digest != model.prepareDigest) {
			return model, nil
		}
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
		model.screen = projectGoalsSetup
		model.scroll = 0
		model.setupFocus = model.setupButtonIndex(projectGoalsFieldEdit)
		model.beginSetupEdit()
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
		model.screen = projectGoalsRun
		model.scroll = 0
		model.detailLoading = false
		model.runFocus = 0
		model.runTab = 0
		model.questionEditing = false
		model.cancelConfirm = false
		model.errText = ""
		return model, nil
	case projectGoalsLoadedMsg:
		if (message.sequence != 0 && message.sequence != model.detailSeq) || (message.goalID != "" && message.goalID != model.selectedID) {
			return model, nil
		}
		model.detailLoading = false
		if message.err != nil {
			model.errText = termsafe.SafeLineMax(message.err.Error(), 180)
			return model, nil
		}
		model.replaceState(message.state)
		model.errText = ""
		return model, nil
	case projectGoalsActionMsg:
		if (message.sequence != 0 && message.sequence != model.actionSeq) || (message.goalID != "" && message.goalID != model.selectedID) {
			return model, nil
		}
		model.actionBusy = false
		if message.questionID != "" && message.questionID != model.questionID {
			return model, nil
		}
		if message.err != nil {
			model.errText = termsafe.SafeLineMax(message.err.Error(), 180)
			return model, nil
		}
		model.footer = message.action
		model.errText = ""
		model.screen = projectGoalsRun
		model.questionEditing = false
		model.cancelConfirm = false
		model.editorManualScroll = false
		model.clearQuestionEditor()
		model.detailLoading = false
		return model, model.loadSelected()
	case projectGoalsArtifactMsg:
		if (message.sequence != 0 && message.sequence != model.artifactSeq) || (message.goalID != "" && message.goalID != model.selectedID) {
			return model, nil
		}
		model.actionBusy = false
		if message.err != nil {
			model.errText = termsafe.SafeLineMax(message.err.Error(), 180)
			return model, nil
		}
		model.viewerKind = message.kind
		model.viewerText = safeGoalArtifactLines(message.text)
		model.viewerScroll = 0
		model.screen = projectGoalsRun
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
	case projectGoalsSetup:
		return model.updateWizard(name, key.Text)
	case projectGoalsRun:
		return model.updateDetail(name, key.Text)
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
	case projectGoalsSetup:
		if !setupFieldText(model.setupField()) {
			return model, nil
		}
		if len(string(model.edit))+len(content) > projectGoalsEditPasteLimit {
			model.errText = "pasted goal text exceeds 32 KiB"
			return model, nil
		}
		model.insertEdit(content)
		if err := model.commitSetupEdit(); err != nil {
			model.errText = termsafe.SafeLineMax(err.Error(), 180)
			return model, nil
		}
		model.errText = ""
		model.editorManualScroll = false
	case projectGoalsRun:
		if !model.questionEditing || model.runFocus != 0 {
			return model, nil
		}
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
		model.startSetup()
		return model, nil
	case "up", "k":
		model.moveSelection(-1)
	case "down", "j":
		model.moveSelection(1)
	case "enter":
		if state, ok := model.selectedState(); ok {
			model.selectedID = state.ID
			model.screen = projectGoalsRun
			model.scroll = 0
			model.detailLoading = false
			return model, model.loadSelected()
		}
		model.startSetup()
	case "r":
		return model, model.loadList()
	case "c":
		if state, ok := model.selectedState(); ok && !state.Phase.Terminal() {
			model.selectedID = state.ID
			model.screen = projectGoalsRun
			model.scroll = 0
		}
	}
	return model, nil
}

func (model *projectGoalsModel) updateWizard(name, text string) (tea.Model, tea.Cmd) {
	return model.updateSetup(name, text)
}

func (model *projectGoalsModel) updateSetup(name, text string) (tea.Model, tea.Cmd) {
	field := model.setupField()
	if name == "ctrl+c" || (name == "q" && !setupFieldText(field)) {
		return model, tea.Quit
	}
	if model.actionBusy && model.prepared != nil {
		return model, nil
	}
	if name == "esc" {
		if model.prepared != nil {
			model.invalidatePrepared()
			model.screen = projectGoalsSetup
			model.setupFocus = model.setupButtonIndex(projectGoalsFieldPrepare)
			model.beginSetupEdit()
			return model, nil
		}
		if model.actionBusy {
			model.invalidatePrepared()
		}
		model.screen = projectGoalsDashboard
		model.scroll = 0
		return model, nil
	}
	if name == "pgup" || name == "pageup" || name == "pgdown" || name == "pagedown" {
		if name == "pgup" || name == "pageup" {
			model.scroll -= 8
		} else {
			model.scroll += 8
		}
		model.editorManualScroll = true
		model.clampScroll()
		return model, nil
	}
	if name == "tab" || name == "shift+tab" || name == "backtab" {
		if err := model.commitSetupEdit(); err != nil {
			model.errText = termsafe.SafeLineMax(err.Error(), 180)
			return model, nil
		}
		delta := 1
		if name != "tab" {
			delta = -1
		}
		model.moveSetupFocus(delta)
		return model, nil
	}
	if field == projectGoalsFieldPreset {
		switch name {
		case "left", "up":
			model.selectPreset(-1)
			return model, nil
		case "right", "down", "space":
			model.selectPreset(1)
			return model, nil
		}
	}
	if field == projectGoalsFieldAdvanced {
		if name == "enter" || name == "space" {
			model.setupAdvanced = !model.setupAdvanced
			model.invalidatePrepared()
			model.clampSetupFocus()
			model.beginSetupEdit()
		}
		return model, nil
	}
	if field == projectGoalsFieldPrepare || field == projectGoalsFieldStart || field == projectGoalsFieldEdit {
		if name == "enter" || name == "space" || (name == "s" && field == projectGoalsFieldStart) {
			return model.activateSetupButton(field)
		}
		return model, nil
	}
	if name == "ctrl+u" {
		model.edit = nil
		model.cursor = 0
		_ = model.commitSetupEdit()
		return model, nil
	}
	if name == "home" || name == "end" || name == "up" || name == "down" {
		model.cursor = moveGoalCursor(model.edit, model.cursor, name)
		return model, nil
	}
	if name == "left" {
		if model.cursor > 0 {
			model.cursor--
		}
		return model, nil
	}
	if name == "right" {
		if model.cursor < len(model.edit) {
			model.cursor++
		}
		return model, nil
	}
	if name == "backspace" {
		if model.cursor > 0 {
			model.edit = append(model.edit[:model.cursor-1], model.edit[model.cursor:]...)
			model.cursor--
			_ = model.commitSetupEdit()
		}
		return model, nil
	}
	if name == "enter" && setupFieldMultiline(field) {
		model.insertEdit("\n")
		_ = model.commitSetupEdit()
		return model, nil
	}
	if name == "enter" || (name == "space" && !setupFieldText(field)) {
		return model, nil
	}
	if text != "" && !strings.HasPrefix(name, "ctrl+") {
		model.insertEdit(text)
		_ = model.commitSetupEdit()
	}
	return model, nil
}

func setupFieldMultiline(field projectGoalsSetupField) bool {
	return field == projectGoalsFieldGoal || field == projectGoalsFieldAcceptance
}

func setupFieldText(field projectGoalsSetupField) bool {
	return field == projectGoalsFieldRepository || field == projectGoalsFieldGoal || field == projectGoalsFieldTitle || field == projectGoalsFieldScope || field == projectGoalsFieldAcceptance || field == projectGoalsFieldChecks || field == projectGoalsFieldModel || field == projectGoalsFieldDuration
}

func (model projectGoalsModel) setupFields() []projectGoalsSetupField {
	fields := []projectGoalsSetupField{
		projectGoalsFieldRepository,
		projectGoalsFieldPreset,
		projectGoalsFieldGoal,
		projectGoalsFieldAdvanced,
	}
	if model.setupAdvanced {
		fields = append(fields,
			projectGoalsFieldTitle,
			projectGoalsFieldScope,
			projectGoalsFieldAcceptance,
			projectGoalsFieldChecks,
			projectGoalsFieldModel,
			projectGoalsFieldDuration,
		)
	}
	if model.prepared == nil {
		fields = append(fields, projectGoalsFieldPrepare)
	} else {
		fields = append(fields, projectGoalsFieldEdit, projectGoalsFieldStart)
	}
	return fields
}

func (model projectGoalsModel) setupField() projectGoalsSetupField {
	fields := model.setupFields()
	if model.setupFocus < 0 || model.setupFocus >= len(fields) {
		return projectGoalsFieldRepository
	}
	return fields[model.setupFocus]
}

func (model projectGoalsModel) setupButtonIndex(field projectGoalsSetupField) int {
	for index, candidate := range model.setupFields() {
		if candidate == field {
			return index
		}
	}
	return len(model.setupFields()) - 1
}

func (model *projectGoalsModel) clampSetupFocus() {
	fields := model.setupFields()
	if len(fields) == 0 {
		model.setupFocus = 0
		return
	}
	if model.setupFocus < 0 {
		model.setupFocus = len(fields) - 1
	}
	if model.setupFocus >= len(fields) {
		model.setupFocus = len(fields) - 1
	}
}

func (model *projectGoalsModel) moveSetupFocus(delta int) {
	fields := model.setupFields()
	if len(fields) == 0 {
		return
	}
	model.setupFocus += delta
	for model.setupFocus < 0 {
		model.setupFocus += len(fields)
	}
	model.setupFocus %= len(fields)
	model.beginSetupEdit()
	model.errText = ""
}

func (model *projectGoalsModel) beginSetupEdit() {
	model.clampSetupFocus()
	field := model.setupField()
	value := model.setupFieldValue(field)
	model.edit = []rune(value)
	model.cursor = len(model.edit)
	model.editorManualScroll = false
}

func (model projectGoalsModel) setupFieldValue(field projectGoalsSetupField) string {
	switch field {
	case projectGoalsFieldRepository:
		return model.draft.Repository
	case projectGoalsFieldGoal:
		return model.draft.Objective
	case projectGoalsFieldTitle:
		return model.draft.Title
	case projectGoalsFieldScope:
		return strings.Join(model.draft.AllowedPaths, ", ")
	case projectGoalsFieldAcceptance:
		return strings.Join(model.draft.Acceptance, "\n")
	case projectGoalsFieldChecks:
		encoded, _ := json.Marshal(model.draft.Checks)
		return string(encoded)
	case projectGoalsFieldModel:
		return model.draft.Model
	case projectGoalsFieldDuration:
		return strconv.Itoa(model.draft.LimitMinutes)
	}
	return ""
}

func (model *projectGoalsModel) commitSetupEdit() error {
	field := model.setupField()
	if field == projectGoalsFieldPreset || field == projectGoalsFieldAdvanced || field >= projectGoalsFieldPrepare {
		return nil
	}
	value := strings.TrimSpace(string(model.edit))
	switch field {
	case projectGoalsFieldRepository:
		if value != model.draft.Repository {
			model.draft.Repository = value
			model.invalidatePrepared()
			if model.presetID != "custom" {
				model.applyPreset(model.presetID, false)
			}
		}
	case projectGoalsFieldGoal:
		if value != model.draft.Objective {
			model.draft.Objective = value
			model.invalidatePrepared()
			if !model.titleExplicit {
				model.draft.Title = deriveGoalTitle(value)
			}
		}
	case projectGoalsFieldTitle:
		if value != model.draft.Title {
			model.draft.Title = value
			model.titleExplicit = true
			model.invalidatePrepared()
		}
	case projectGoalsFieldScope:
		paths := splitGoalList(value)
		if !sameStringSlice(paths, model.draft.AllowedPaths) {
			model.draft.AllowedPaths = paths
			model.presetID = "custom"
			model.invalidatePrepared()
		}
	case projectGoalsFieldAcceptance:
		criteria := splitGoalLines(value)
		if !sameStringSlice(criteria, model.draft.Acceptance) {
			model.draft.Acceptance = criteria
			model.presetID = "custom"
			model.invalidatePrepared()
		}
	case projectGoalsFieldChecks:
		var checks [][]string
		if err := json.Unmarshal([]byte(value), &checks); err != nil {
			return fmt.Errorf("checks must be a JSON array of argument arrays: %w", err)
		}
		if !sameStringMatrix(checks, model.draft.Checks) {
			model.draft.Checks = checks
			model.presetID = "custom"
			model.invalidatePrepared()
		}
	case projectGoalsFieldModel:
		if value != model.draft.Model {
			model.draft.Model = value
			model.invalidatePrepared()
		}
	case projectGoalsFieldDuration:
		minutes, err := strconv.Atoi(value)
		if err != nil {
			return errors.New("duration must be a number from 1 to 30 minutes")
		}
		if minutes != model.draft.LimitMinutes {
			model.draft.LimitMinutes = minutes
			model.invalidatePrepared()
		}
	}
	return nil
}

func (model *projectGoalsModel) invalidatePrepared() {
	model.prepareSeq++
	model.prepared = nil
	model.prepareDigest = ""
	model.draft.BaseRevision = ""
	model.actionBusy = false
	model.screen = projectGoalsSetup
}

func (model *projectGoalsModel) selectPreset(delta int) {
	presets := projectGoalPresets(model.draft.Repository)
	if len(presets) == 0 {
		return
	}
	current := 0
	for index, preset := range presets {
		if preset.ID == model.presetID {
			current = index
			break
		}
	}
	current = (current + delta + len(presets)) % len(presets)
	model.applyPreset(presets[current].ID, true)
}

func (model *projectGoalsModel) applyPreset(id string, invalidate bool) {
	for _, preset := range projectGoalPresets(model.draft.Repository) {
		if preset.ID != id {
			continue
		}
		model.presetID = id
		if id != "custom" {
			model.draft.AllowedPaths = append([]string(nil), preset.AllowedPaths...)
			model.draft.Acceptance = append([]string(nil), preset.Acceptance...)
			model.draft.Checks = cloneStringMatrix(preset.Checks)
		}
		if invalidate {
			model.invalidatePrepared()
		}
		model.beginSetupEdit()
		return
	}
}

func (model *projectGoalsModel) activateSetupButton(field projectGoalsSetupField) (tea.Model, tea.Cmd) {
	switch field {
	case projectGoalsFieldPrepare:
		if model.actionBusy {
			return model, nil
		}
		if err := model.commitSetupEdit(); err != nil {
			model.errText = termsafe.SafeLineMax(err.Error(), 180)
			return model, nil
		}
		return model, model.prepareGoal()
	case projectGoalsFieldEdit:
		model.invalidatePrepared()
		model.screen = projectGoalsSetup
		model.setupFocus = model.setupButtonIndex(projectGoalsFieldPrepare)
		model.beginSetupEdit()
	case projectGoalsFieldStart:
		if model.actionBusy {
			return model, nil
		}
		if model.prepared == nil {
			model.errText = "approval is not ready; prepare the goal first"
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
	return model, nil
}

func (model *projectGoalsModel) updateDetail(name, text string) (tea.Model, tea.Cmd) {
	if model.cancelConfirm {
		return model.updateCancel(name)
	}
	if model.questionEditing {
		return model.updateQuestion(name, text)
	}
	switch name {
	case "q", "ctrl+c":
		return model, tea.Quit
	case "esc":
		model.detailSeq++
		model.detailLoading = false
		model.actionSeq++
		model.actionBusy = false
		if model.runTab != 0 {
			model.artifactSeq++
			model.runTab = 0
			model.viewerText = nil
		} else {
			model.artifactSeq++
			model.screen = projectGoalsDashboard
		}
		model.scroll = 0
	case "up", "k":
		model.scroll--
	case "down", "j":
		model.scroll++
	case "pgup", "pageup":
		model.scroll -= 8
		model.editorManualScroll = true
	case "pgdown", "pagedown":
		model.scroll += 8
		model.editorManualScroll = true
	case "a":
		if model.actionBusy {
			return model, nil
		}
		if state, ok := model.selectedState(); ok && state.Question != nil && state.Question.Answer == "" {
			model.beginQuestionEdit(state.Question)
		}
	case "c":
		if !model.actionBusy {
			if state, ok := model.selectedState(); ok && !state.Phase.Terminal() {
				model.scroll = 0
				model.runFocus = 0
				model.cancelConfirm = true
			}
		}
	case "d":
		model.runTab = 1
		model.viewerText = nil
		model.scroll = 0
		model.editorManualScroll = false
		return model, model.readArtifact("patch", 0)
	case "v":
		model.runTab = 2
		model.viewerText = nil
		model.scroll = 0
		model.editorManualScroll = false
		return model, model.readArtifact("check", 0)
	case "e":
		model.runTab = 3
		model.viewerText = nil
		model.scroll = 0
		model.editorManualScroll = false
		return model, model.readArtifact("json", 0)
	case "1":
		model.artifactSeq++
		model.actionSeq++
		model.actionBusy = false
		model.runTab = 0
		model.viewerText = nil
		model.scroll = 0
		model.editorManualScroll = false
	case "2":
		model.runTab = 1
		model.viewerText = nil
		model.scroll = 0
		model.editorManualScroll = false
		return model, model.readArtifact("patch", 0)
	case "3":
		model.runTab = 2
		model.viewerText = nil
		model.scroll = 0
		model.editorManualScroll = false
		return model, model.readArtifact("check", 0)
	case "4":
		model.runTab = 3
		model.viewerText = nil
		model.scroll = 0
		model.editorManualScroll = false
		return model, model.readArtifact("json", 0)
	case "tab":
		if model.actionBusy {
			return model, nil
		}
		if state, ok := model.selectedState(); ok && state.Question != nil && state.Question.Answer == "" {
			model.beginQuestionEdit(state.Question)
		}
	}
	model.clampScroll()
	return model, nil
}

func (model *projectGoalsModel) updateQuestion(name, text string) (tea.Model, tea.Cmd) {
	if name == "ctrl+c" {
		return model, tea.Quit
	}
	if name == "esc" {
		model.questionEditing = false
		model.editorManualScroll = false
		return model, nil
	}
	if model.actionBusy {
		return model, nil
	}
	if name == "tab" || name == "shift+tab" || name == "backtab" {
		model.runFocus = (model.runFocus + 1) % 2
		return model, nil
	}
	if model.runFocus == 1 && (name == "enter" || name == "space") {
		return model.sendAnswer()
	}
	if model.runFocus != 0 {
		return model, nil
	}
	if name == "home" || name == "end" || name == "up" || name == "down" {
		model.answerCursor = moveGoalCursor(model.answer, model.answerCursor, name)
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
		model.insertAnswer("\n")
		model.editorManualScroll = false
		return model, nil
	}
	if text != "" && !strings.HasPrefix(name, "ctrl+") {
		model.insertAnswer(text)
		model.editorManualScroll = false
	}
	return model, nil
}

func (model *projectGoalsModel) sendAnswer() (tea.Model, tea.Cmd) {
	if model.actionBusy {
		return model, nil
	}
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
	model.actionSeq++
	sequence := model.actionSeq
	goalID, questionID := model.selectedID, model.questionID
	return model, func() tea.Msg {
		return projectGoalsActionMsg{goalID: goalID, questionID: questionID, sequence: sequence, action: "answer sent", err: model.opts.Respond(model.ctx, goalID, questionID, answer)}
	}
}

func (model *projectGoalsModel) updateCancel(name string) (tea.Model, tea.Cmd) {
	if name == "tab" || name == "shift+tab" || name == "backtab" {
		model.runFocus = (model.runFocus + 1) % 2
		return model, nil
	}
	if name == "enter" || name == "space" {
		if model.runFocus == 0 {
			name = "n"
		} else {
			name = "y"
		}
	}
	switch name {
	case "q", "ctrl+c":
		return model, tea.Quit
	case "esc", "n":
		if model.actionBusy {
			model.actionSeq++
			model.actionBusy = false
		}
		model.cancelConfirm = false
	case "y":
		if model.actionBusy {
			return model, nil
		}
		if model.opts.Cancel == nil {
			model.errText = "cancel is unavailable"
			model.screen = projectGoalsRun
			return model, nil
		}
		model.actionBusy = true
		model.actionSeq++
		sequence := model.actionSeq
		goalID := model.selectedID
		return model, func() tea.Msg {
			return projectGoalsActionMsg{goalID: goalID, sequence: sequence, action: "cancel requested", err: model.opts.Cancel(model.ctx, goalID)}
		}
	}
	return model, nil
}

func (model *projectGoalsModel) startSetup() {
	model.invalidatePrepared()
	model.screen = projectGoalsSetup
	model.presetID = "bugfix"
	model.setupAdvanced = false
	model.setupFocus = 0
	model.titleExplicit = false
	model.errText = ""
	model.scroll = 0
	model.editorManualScroll = false
	model.applyPreset("bugfix", false)
}

func deriveGoalTitle(objective string) string {
	line := strings.TrimSpace(strings.SplitN(objective, "\n", 2)[0])
	if line == "" {
		return "Repository goal"
	}
	if len(line) > 128 {
		cut := 128
		for cut > 0 && !utf8.RuneStart(line[cut]) {
			cut--
		}
		line = line[:cut]
	}
	return line
}

func sameStringSlice(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func sameStringMatrix(left, right [][]string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if !sameStringSlice(left[index], right[index]) {
			return false
		}
	}
	return true
}

func cloneStringMatrix(value [][]string) [][]string {
	clone := make([][]string, len(value))
	for index := range value {
		clone[index] = append([]string(nil), value[index]...)
	}
	return clone
}

func (model projectGoalsModel) isRunScreen() bool {
	return model.screen == projectGoalsRun
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
	encoded, _ := json.Marshal(spec)
	digest := goal.Digest(encoded)
	model.prepareSeq++
	sequence := model.prepareSeq
	model.prepareDigest = digest
	return func() tea.Msg {
		prepared, err := model.opts.Prepare(model.ctx, spec)
		return projectGoalsPreparedMsg{prepared: prepared, digest: digest, sequence: sequence, err: err}
	}
}

func (model *projectGoalsModel) loadSelected() tea.Cmd {
	if model.selectedID == "" || model.opts.Load == nil || model.detailLoading {
		return nil
	}
	model.detailLoading = true
	goalID := model.selectedID
	model.detailSeq++
	sequence := model.detailSeq
	return func() tea.Msg {
		state, err := model.opts.Load(model.ctx, goalID)
		return projectGoalsLoadedMsg{goalID: goalID, sequence: sequence, state: state, err: err}
	}
}

func (model *projectGoalsModel) readArtifact(kind string, index int) tea.Cmd {
	if model.selectedID == "" || model.opts.ReadArtifact == nil {
		model.errText = "artifact viewing is unavailable"
		return nil
	}
	model.actionSeq++
	model.actionBusy = true
	goalID := model.selectedID
	model.artifactSeq++
	sequence := model.artifactSeq
	checkCount := 0
	if kind == "check" {
		if state, ok := model.selectedState(); ok {
			checkCount = len(state.Checks)
		}
	}
	return func() tea.Msg {
		if kind == "check" {
			if checkCount == 0 {
				return projectGoalsArtifactMsg{goalID: goalID, sequence: sequence, kind: kind, text: "No check output receipts yet."}
			}
			var combined strings.Builder
			for checkIndex := 0; checkIndex < checkCount; checkIndex++ {
				text, err := model.opts.ReadArtifact(model.ctx, goalID, kind, checkIndex)
				if err != nil {
					return projectGoalsArtifactMsg{goalID: goalID, sequence: sequence, kind: kind, err: err}
				}
				fmt.Fprintf(&combined, "Check %d\n%s\n", checkIndex+1, text)
			}
			return projectGoalsArtifactMsg{goalID: goalID, sequence: sequence, kind: kind, text: combined.String()}
		}
		text, err := model.opts.ReadArtifact(model.ctx, goalID, kind, index)
		return projectGoalsArtifactMsg{goalID: goalID, sequence: sequence, kind: kind, text: text, err: err}
	}
}

func (model *projectGoalsModel) replaceState(state goal.State) {
	for index := range model.states {
		if model.states[index].ID == state.ID {
			model.states[index] = state
			if state.ID == model.selectedID {
				model.reconcileQuestion(state)
			}
			return
		}
	}
	model.states = append(model.states, state)
	if state.ID == model.selectedID {
		model.reconcileQuestion(state)
	}
}

func (model *projectGoalsModel) clearQuestionEditor() {
	model.questionText = ""
	model.questionID = ""
	model.answer = nil
	model.answerCursor = 0
	model.questionEditing = false
	model.runFocus = 0
}

func (model *projectGoalsModel) beginQuestionEdit(question *goal.Question) {
	if model.questionID != question.ID {
		model.clearQuestionEditor()
	}
	model.questionID = question.ID
	model.questionText = question.Text
	model.scroll = 0
	model.runFocus = 0
	model.questionEditing = true
	model.editorManualScroll = false
}

func (model *projectGoalsModel) reconcileQuestion(state goal.State) {
	question := state.Question
	if question == nil || question.Answer != "" {
		model.clearQuestionEditor()
		return
	}
	if model.questionID != "" && model.questionID != question.ID {
		model.clearQuestionEditor()
	}
	model.questionID = question.ID
	model.questionText = question.Text
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
	var runBody []string
	lines = append(lines, model.styles.Brand.Render("●")+" "+model.styles.Header.Render("Projects · Goals"))
	switch model.screen {
	case projectGoalsDashboard:
		lines = append(lines, model.renderDashboard(contentWidth)...)
	case projectGoalsSetup:
		lines = append(lines, model.renderSetup(contentWidth)...)
	case projectGoalsRun:
		runLines := model.renderRunWorkspace(contentWidth)
		pinned := goalMin(6, len(runLines))
		lines = append(lines, runLines[:pinned]...)
		runBody = runLines[pinned:]
	}
	footer := model.footerText()
	available := goalMax(height-1, 1)
	if model.isRunScreen() {
		bodyAvailable := goalMax(available-len(lines), 0)
		if bodyAvailable > 0 {
			scroll := model.scroll
			if !model.editorManualScroll {
				cursorLine := model.editorCursorLine(contentWidth) - len(lines)
				maxStart := goalMax(len(runBody)-bodyAvailable, 0)
				if cursorLine < scroll {
					scroll = cursorLine
				}
				if cursorLine >= scroll+bodyAvailable {
					scroll = cursorLine - bodyAvailable + 1
				}
				scroll = goalMin(goalMax(scroll, 0), maxStart)
			}
			start := goalMin(goalMax(scroll, 0), goalMax(len(runBody)-bodyAvailable, 0))
			body := runBody[start:]
			if len(body) > bodyAvailable {
				body = body[:bodyAvailable]
			}
			lines = append(lines, body...)
		}
	} else {
		scroll := model.scroll
		if model.screen == projectGoalsSetup && !model.editorManualScroll {
			scroll = model.editorScroll(lines, contentWidth, available)
		}
		if len(lines) > available {
			start := goalMin(scroll, len(lines)-available)
			lines = lines[start:]
			if len(lines) > available {
				lines = lines[:available]
			}
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
		lines = append(lines, model.styles.Fg.Render("No goals yet."), model.styles.Fg.Render("> [ New goal ]"), model.styles.Meta.Render("Enter or n opens the setup form."))
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

func (model projectGoalsModel) renderSetup(width int) []string {
	lines := []string{
		model.styles.Header.Render("Start goal"),
		model.styles.Meta.Render("Enter adds a line in Goal/Acceptance · Tab/Shift+Tab moves · Enter/Space activates buttons"),
		"",
	}
	fields := model.setupFields()
	for index, field := range fields {
		focused := index == model.setupFocus
		marker := "  "
		if focused {
			marker = "> "
		}
		switch field {
		case projectGoalsFieldRepository:
			lines = append(lines, marker+"Repository: "+model.setupValueForRender(field, width-14))
		case projectGoalsFieldPreset:
			preset := model.selectedPreset()
			name, hint := "Custom", "preserve explicit scope, checks and acceptance"
			if preset != nil {
				name, hint = preset.Name, preset.Hint
			}
			lines = append(lines, marker+"Preset: ["+name+"]")
			lines = append(lines, model.styles.Meta.Render("  "+safeGoalField(hint, width-4)))
			if !model.setupAdvanced {
				lines = append(lines, model.styles.Meta.Render("  Scope: "+safeGoalField(strings.Join(model.draft.AllowedPaths, ", "), width-10)))
			}
			if preset != nil && preset.CheckNote != "" {
				for _, line := range goalWrap("Checks: "+preset.CheckNote, width-4) {
					lines = append(lines, model.styles.Meta.Render("  "+line))
				}
			}
		case projectGoalsFieldGoal:
			lines = append(lines, marker+"Goal:")
			value := model.setupFieldValue(field)
			if focused {
				value = goalCursorText(model.edit, model.cursor)
			}
			for _, line := range goalWrap(value, width-4) {
				lines = append(lines, "    "+line)
			}
			if value == "" {
				lines = append(lines, model.styles.Dim.Render("    describe the bounded change"))
			}
		case projectGoalsFieldAdvanced:
			label := "Advanced: collapsed"
			if model.setupAdvanced {
				label = "Advanced: expanded"
			}
			lines = append(lines, marker+"[ "+label+" ]")
		case projectGoalsFieldTitle, projectGoalsFieldScope, projectGoalsFieldAcceptance, projectGoalsFieldChecks, projectGoalsFieldModel, projectGoalsFieldDuration:
			label := setupFieldLabel(field)
			value := model.setupValueForRender(field, width-lipgloss.Width(label)-4)
			if field == projectGoalsFieldAcceptance {
				value = model.setupFieldValue(field)
				if focused {
					value = goalCursorText(model.edit, model.cursor)
				}
				lines = append(lines, marker+label+":")
				for _, line := range goalWrap(value, width-4) {
					lines = append(lines, "    "+line)
				}
			} else {
				lines = append(lines, marker+label+": "+value)
			}
		case projectGoalsFieldPrepare:
			label := "[ Prepare goal ]"
			if model.actionBusy {
				label = "[ Preparing… ]"
			}
			lines = append(lines, marker+label)
		case projectGoalsFieldEdit, projectGoalsFieldStart:
			if index == model.setupButtonIndex(projectGoalsFieldEdit) {
				lines = append(lines, model.styles.Header.Render("Approval"))
				prepared := model.prepared
				if prepared != nil {
					lines = appendGoalField(lines, "Repository", prepared.Spec.Repository, width)
					lines = appendGoalField(lines, "Title", prepared.Spec.Title, width)
					lines = appendGoalField(lines, "Base revision", prepared.Spec.BaseRevision, width)
					lines = appendGoalField(lines, "Scope", strings.Join(prepared.Spec.AllowedPaths, ", "), width)
					lines = appendGoalField(lines, "Goal", prepared.Spec.Objective, width)
					lines = append(lines, model.styles.Header.Render("Acceptance"))
					for _, criterion := range prepared.Spec.Acceptance {
						for index, line := range goalWrap(criterion, width-4) {
							prefix := "    "
							if index == 0 {
								prefix = "  - "
							}
							lines = append(lines, prefix+line)
						}
					}
					lines = appendGoalField(lines, "Commands", formatGoalChecks(prepared.Spec.Checks), width)
					lines = appendGoalField(lines, "Model", prepared.Spec.Model, width)
					lines = appendGoalField(lines, "Duration", fmt.Sprintf("%d minutes", prepared.Spec.LimitMinutes), width)
					for _, line := range goalWrap("Execution: isolated private worktree; source checkout remains unchanged; Start is the approval boundary.", width) {
						lines = append(lines, model.styles.Meta.Render(line))
					}
					if prepared.SourceDirty {
						for _, line := range goalWrap("! source checkout has uncommitted changes; only approved scope may change", width) {
							lines = append(lines, model.styles.Warn.Render(line))
						}
					}
					for _, warning := range prepared.Warnings {
						for _, line := range goalWrap("! "+warning, width) {
							lines = append(lines, model.styles.Warn.Render(line))
						}
					}
				}
			}
			label := "[ Edit ]"
			if field == projectGoalsFieldStart {
				label = "[ Start ]"
			}
			lines = append(lines, marker+label)
		}
	}
	if model.errText != "" {
		lines = append(lines, "", model.styles.Danger.Render("! "+model.errText))
	}
	return lines
}

func (model projectGoalsModel) setupValueForRender(field projectGoalsSetupField, width int) string {
	value := model.setupFieldValue(field)
	if field == model.setupField() && field != projectGoalsFieldPreset && field < projectGoalsFieldPrepare {
		return goalEditorViewport(model.edit, model.cursor, width)
	}
	if width < 1 {
		width = 1
	}
	return safeGoalField(value, width)
}

func setupFieldLabel(field projectGoalsSetupField) string {
	switch field {
	case projectGoalsFieldTitle:
		return "Title"
	case projectGoalsFieldScope:
		return "Allowed scope"
	case projectGoalsFieldAcceptance:
		return "Acceptance"
	case projectGoalsFieldChecks:
		return "Checks"
	case projectGoalsFieldModel:
		return "OpenAI model"
	case projectGoalsFieldDuration:
		return "Duration (minutes)"
	default:
		return ""
	}
}

func formatGoalChecks(checks [][]string) string {
	parts := make([]string, len(checks))
	for index, check := range checks {
		encoded, err := json.Marshal(check)
		if err != nil {
			parts[index] = "[]"
		} else {
			parts[index] = string(encoded)
		}
	}
	return strings.Join(parts, "; ")
}

func (model projectGoalsModel) selectedPreset() *GoalPreset {
	for _, preset := range projectGoalPresets(model.draft.Repository) {
		if preset.ID == model.presetID {
			selected := preset
			return &selected
		}
	}
	return nil
}

func (model projectGoalsModel) renderRunWorkspace(width int) []string {
	state, ok := model.selectedState()
	if !ok {
		return []string{model.styles.Meta.Render("No goal selected."), ""}
	}
	title := safeGoalField(state.Spec.Title, width)
	lines := []string{model.styles.Header.Render("Goal · " + title)}
	lines = append(lines, safeGoalField("Goal: "+state.ID, width))
	lines = append(lines, model.styles.Meta.Render("1 Overview · 2 Diff · 3 Checks · 4 JSON"))
	lines = append(lines, safeGoalField(fmt.Sprintf("Status: %s · %s", goalPhaseLabel(state.Phase), state.Summary), width))
	lines = append(lines, "")
	lines = append(lines, model.styles.Header.Render(runTabLabel(model.runTab)))
	switch model.runTab {
	case 1:
		lines = append(lines, model.styles.Meta.Render("Diff evidence"))
		for _, line := range model.viewerText {
			lines = append(lines, goalLiteralWrap(line, width)...)
		}
	case 2:
		lines = append(lines, model.styles.Meta.Render("Verification receipts"))
		if len(state.Checks) == 0 {
			lines = append(lines, model.styles.Dim.Render("No check receipts yet."))
		}
		for index, check := range state.Checks {
			lines = append(lines, goalWrap(fmt.Sprintf("Check %d · %s · stdout: %s · exit %d", index+1, boolLabel(check.Passed), check.OutputPath, check.ExitCode), width)...)
			for _, line := range goalLiteralWrap("argv: "+formatGoalChecks([][]string{check.Argv}), width) {
				lines = append(lines, model.styles.Meta.Render(line))
			}
		}
		if len(model.viewerText) > 0 {
			lines = append(lines, model.styles.Header.Render("Check output"))
			for _, line := range model.viewerText {
				lines = append(lines, goalLiteralWrap(line, width)...)
			}
		}
	case 3:
		encoded, _ := json.MarshalIndent(state, "", "  ")
		lines = append(lines, goalLiteralWrap(string(encoded), width)...)
	default:
		lines = append(lines, model.styles.Header.Render("Progress"))
		lines = appendGoalField(lines, "Status", goalPhaseLabel(state.Phase), width)
		lines = appendGoalField(lines, "Summary", state.Summary, width)
		lines = appendGoalField(lines, "Repository", state.Spec.Repository, width)
		lines = appendGoalField(lines, "Worktree", state.Worktree, width)
		lines = appendGoalField(lines, "Supervisor", supervisorLabel(state), width)
		lines = appendGoalField(lines, "Objective", state.Spec.Objective, width)
		if len(state.Roles) > 0 {
			lines = append(lines, model.styles.Header.Render("Roles"))
			for _, role := range state.Roles {
				lines = append(lines, safeGoalField(fmt.Sprintf("%s · %s · %s", role.Name, role.Provider, role.Model), width))
			}
		}
	}
	if state.Question != nil && state.Question.Answer == "" {
		lines = append(lines, "", model.styles.Warn.Render("Cameron decision needed"))
		lines = append(lines, goalWrap(state.Question.Text, width)...)
		lines = append(lines, model.styles.Header.Render("Answer"))
		answerLines := goalWrap(goalCursorText(model.answer, model.answerCursor), width-4)
		for index, line := range answerLines {
			prefix := "    "
			if index == 0 && model.runFocus == 0 {
				prefix = "> "
			}
			lines = append(lines, prefix+line)
		}
		if model.questionEditing {
			marker := "  "
			if model.runFocus == 1 {
				marker = "> "
			}
			lines = append(lines, model.styles.Fg.Render(marker+"[ Send answer ]"))
		}
	}
	if model.cancelConfirm {
		lines = append(lines, "", model.styles.Warn.Render("Cancel goal? The default is to keep it running."))
		keepMarker, cancelMarker := "  ", "  "
		if model.runFocus == 0 {
			keepMarker = "> "
		} else {
			cancelMarker = "> "
		}
		lines = append(lines, keepMarker+"[ Keep running ]  "+cancelMarker+"[ Request cancellation ]")
	}
	if model.errText != "" {
		lines = append(lines, "", model.styles.Danger.Render("! "+model.errText))
	}
	return lines
}

func runTabLabel(tab int) string {
	switch tab {
	case 1:
		return "Evidence · Diff"
	case 2:
		return "Evidence · Checks"
	case 3:
		return "Details · JSON"
	default:
		return "Overview"
	}
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
	case projectGoalsSetup:
		return 2 + model.setupEditorCursorLine(width)
	case projectGoalsRun:
		if model.questionEditing {
			state, ok := model.selectedState()
			if !ok || state.Question == nil {
				return 0
			}
			if model.runFocus == 1 {
				buttonModel := model
				buttonModel.errText = ""
				return len(buttonModel.renderRunWorkspace(width))
			}
			base := model
			base.questionEditing = false
			baseState := state
			baseState.Question = nil
			base.states = []goal.State{baseState}
			base.selected = 0
			answerStart := 1 + len(base.renderRunWorkspace(width)) + 2 + len(goalWrap(state.Question.Text, width)) + 1
			return answerStart + goalCursorLine(model.answer, model.answerCursor, goalMax(width-4, 1))
		}
		if model.cancelConfirm {
			base := model
			base.errText = ""
			return 1 + len(base.renderRunWorkspace(width)) - 1
		}
		return 0
	default:
		return 0
	}
}

func (model projectGoalsModel) setupEditorCursorLine(width int) int {
	line := 3
	for index, field := range model.setupFields() {
		if index == model.setupFocus {
			if field == projectGoalsFieldGoal || field == projectGoalsFieldAcceptance {
				return line + goalCursorLine(model.edit, model.cursor, goalMax(width-4, 1))
			}
			if field == projectGoalsFieldEdit {
				return line + model.approvalLineCount(width)
			}
			return line
		}
		switch field {
		case projectGoalsFieldRepository:
			line++
		case projectGoalsFieldPreset:
			line += 2
			if !model.setupAdvanced {
				line++
			}
			line += model.presetCheckNoteLineCount(width)
		case projectGoalsFieldGoal:
			line += 1 + goalMax(len(goalWrap(model.draft.Objective, goalMax(width-4, 1))), 1)
		case projectGoalsFieldAdvanced:
			line++
		case projectGoalsFieldEdit:
			line += model.approvalLineCount(width) + 1
		case projectGoalsFieldAcceptance:
			line += 1 + goalMax(len(goalWrap(strings.Join(model.draft.Acceptance, "\n"), goalMax(width-4, 1))), 1)
		default:
			line++
		}
	}
	return line
}

func (model projectGoalsModel) approvalLineCount(width int) int {
	if model.prepared == nil {
		return 0
	}
	prepared := model.prepared
	count := 1
	for _, field := range []struct {
		label string
		value string
	}{
		{label: "Repository", value: prepared.Spec.Repository},
		{label: "Title", value: prepared.Spec.Title},
		{label: "Base revision", value: prepared.Spec.BaseRevision},
		{label: "Scope", value: strings.Join(prepared.Spec.AllowedPaths, ", ")},
		{label: "Goal", value: prepared.Spec.Objective},
	} {
		count += goalFieldLineCount(field.label, field.value, width)
	}
	count++
	for _, criterion := range prepared.Spec.Acceptance {
		count += goalMax(len(goalWrap(criterion, width-4)), 1)
	}
	count += goalFieldLineCount("Commands", formatGoalChecks(prepared.Spec.Checks), width)
	count += goalFieldLineCount("Model", prepared.Spec.Model, width)
	count += goalFieldLineCount("Duration", fmt.Sprintf("%d minutes", prepared.Spec.LimitMinutes), width)
	count += goalMax(len(goalWrap("Execution: isolated private worktree; source checkout remains unchanged; Start is the approval boundary.", width)), 1)
	if prepared.SourceDirty {
		count += goalMax(len(goalWrap("! source checkout has uncommitted changes; only approved scope may change", width)), 1)
	}
	for _, warning := range prepared.Warnings {
		count += goalMax(len(goalWrap("! "+warning, width)), 1)
	}
	return count
}

func (model projectGoalsModel) presetCheckNoteLineCount(width int) int {
	preset := model.selectedPreset()
	if preset == nil || preset.CheckNote == "" {
		return 0
	}
	return goalMax(len(goalWrap("Checks: "+preset.CheckNote, goalMax(width-4, 1))), 1)
}

func goalFieldLineCount(label, value string, width int) int {
	available := width - lipgloss.Width(label+": ")
	if available < 1 {
		available = 1
	}
	return goalMax(len(goalLiteralWrap(value, available)), 1)
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

func (model projectGoalsModel) footerText() string {
	compact := model.width > 0 && model.width < 70
	switch model.screen {
	case projectGoalsDashboard:
		if compact {
			return "j/k move · enter open/new · n new · c cancel · q quit"
		}
		return "↑↓/jk select · enter details/new · n new · c cancel · r refresh · q detach"
	case projectGoalsSetup:
		if compact {
			return "tab/backtab · enter newline/button · esc back"
		}
		return "Tab/Shift+Tab focus · Enter newline · Enter/Space buttons · Ctrl+C detach"
	case projectGoalsRun:
		if model.questionEditing {
			return "Enter newline · Tab Send · Esc back"
		}
		if model.cancelConfirm {
			return "Tab choice · Enter confirm · Esc keep"
		}
		if compact {
			return "1-4 tabs · a answer · c cancel · q detach"
		}
		return "1 overview · 2 diff · 3 checks · 4 JSON · a answer · c cancel · q detach"
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

func appendGoalField(lines []string, label, value string, width int) []string {
	prefix := label + ": "
	available := width - lipgloss.Width(prefix)
	if available < 1 {
		available = 1
	}
	wrapped := goalLiteralWrap(value, available)
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

func moveGoalCursor(value []rune, cursor int, direction string) int {
	if cursor < 0 {
		cursor = 0
	}
	if cursor > len(value) {
		cursor = len(value)
	}
	lineStart := cursor
	for lineStart > 0 && value[lineStart-1] != '\n' {
		lineStart--
	}
	lineEnd := cursor
	for lineEnd < len(value) && value[lineEnd] != '\n' {
		lineEnd++
	}
	switch direction {
	case "home":
		return lineStart
	case "end":
		return lineEnd
	case "up":
		if lineStart == 0 {
			return cursor
		}
		previousEnd := lineStart - 1
		previousStart := previousEnd
		for previousStart > 0 && value[previousStart-1] != '\n' {
			previousStart--
		}
		return previousStart + goalMin(cursor-lineStart, previousEnd-previousStart)
	case "down":
		if lineEnd == len(value) {
			return cursor
		}
		nextStart := lineEnd + 1
		nextEnd := nextStart
		for nextEnd < len(value) && value[nextEnd] != '\n' {
			nextEnd++
		}
		return nextStart + goalMin(cursor-lineStart, nextEnd-nextStart)
	default:
		return cursor
	}
}

func goalEditorViewport(value []rune, cursor, width int) string {
	if width < 1 {
		return ""
	}
	text := []rune(goalCursorText(value, cursor))
	if lipgloss.Width(string(text)) <= width {
		return safeGoalField(string(text), width)
	}
	marker := cursor
	start := marker - width/2
	if start < 0 {
		start = 0
	}
	if start+width > len(text) {
		start = len(text) - width
	}
	if start > marker {
		start = marker
	}
	if start < 0 {
		start = 0
	}
	return safeGoalField(string(text[start:]), width)
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

func goalLiteralWrap(value string, width int) []string {
	var lines []string
	for _, line := range strings.Split(value, "\n") {
		wrapped := ansi.Hardwrap(safeGoalText(line), goalMax(width, 1), true)
		lines = append(lines, strings.Split(wrapped, "\n")...)
	}
	return lines
}

func goalWrap(value string, width int) []string {
	if width < 1 {
		width = 1
	}
	var result []string
	for _, raw := range strings.Split(value, "\n") {
		raw = safeGoalText(raw)
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
