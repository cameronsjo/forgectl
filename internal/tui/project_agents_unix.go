//go:build unix

package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/cameronsjo/forgectl/internal/keymap"
	"github.com/cameronsjo/forgectl/internal/surface/activity"
	"github.com/cameronsjo/forgectl/internal/surface/projectview"
	"github.com/cameronsjo/forgectl/internal/termsafe"
	"github.com/cameronsjo/forgectl/internal/theme"
)

const (
	projectAgentsDefaultPoll = 5 * time.Second
	projectAgentsDefaultView = 30 * time.Minute
	projectAgentsFreshFor    = 2 * time.Minute
	projectAgentsMinView     = 5 * time.Minute
	projectAgentsMaxView     = 60 * time.Minute
	projectAgentsMinWidth    = 70
	projectAgentsNameWidth   = 22
)

type ProjectAgentsOptions struct {
	Theme        theme.Theme
	Load         func(context.Context) (projectview.AgentsSnapshot, error)
	Focus        func(context.Context, string, string) error
	OpenEvidence func(context.Context, string, string) error
	ASCII        bool
	Poll         time.Duration
}

type projectAgentsModel struct {
	ctx          context.Context
	theme        theme.Theme
	styles       theme.Styles
	load         func(context.Context) (projectview.AgentsSnapshot, error)
	focus        func(context.Context, string, string) error
	openEvidence func(context.Context, string, string) error
	ascii        bool
	poll         time.Duration
	window       time.Duration
	now          func() time.Time

	snapshot     projectview.AgentsSnapshot
	hasSnapshot  bool
	loading      bool
	loadSequence uint64
	sourceError  string
	actionBusy   bool
	selected     string
	offset       int
	footer       string
	width        int
	height       int
}

type projectAgentsLoadMsg struct {
	sequence uint64
	snapshot projectview.AgentsSnapshot
	err      error
}

type projectAgentsTickMsg struct{}

type projectAgentsActionMsg struct {
	err error
}

func RunProjectAgents(ctx context.Context, opts ProjectAgentsOptions) error {
	if opts.Load == nil {
		return fmt.Errorf("agents view: load function is required")
	}
	if opts.Poll <= 0 {
		opts.Poll = projectAgentsDefaultPoll
	}
	final, err := tea.NewProgram(newProjectAgentsModel(ctx, opts), keymap.ProgramOptions(ctx)...).Run()
	if err != nil {
		return err
	}
	if _, ok := final.(*projectAgentsModel); !ok {
		return nil
	}
	return nil
}

func newProjectAgentsModel(ctx context.Context, opts ProjectAgentsOptions) *projectAgentsModel {
	poll := opts.Poll
	if poll <= 0 {
		poll = projectAgentsDefaultPoll
	}
	return &projectAgentsModel{
		ctx:          ctx,
		theme:        opts.Theme,
		styles:       opts.Theme.Styles(),
		load:         opts.Load,
		focus:        opts.Focus,
		openEvidence: opts.OpenEvidence,
		ascii:        opts.ASCII,
		poll:         poll,
		window:       projectAgentsDefaultView,
		now:          time.Now,
	}
}

func (model *projectAgentsModel) Init() tea.Cmd {
	load := model.startLoad()
	return tea.Batch(load, model.tick())
}

func (model *projectAgentsModel) startLoad() tea.Cmd {
	if model.loading || model.load == nil {
		return nil
	}
	model.loading = true
	model.loadSequence++
	sequence := model.loadSequence
	load := model.load
	ctx := model.ctx
	return func() tea.Msg {
		snapshot, err := load(ctx)
		return projectAgentsLoadMsg{sequence: sequence, snapshot: snapshot, err: err}
	}
}

func (model projectAgentsModel) tick() tea.Cmd {
	return tea.Tick(model.poll, func(time.Time) tea.Msg { return projectAgentsTickMsg{} })
}

func (model *projectAgentsModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch message := msg.(type) {
	case tea.WindowSizeMsg:
		model.width, model.height = message.Width, message.Height
		model.ensureVisible()
		return model, nil
	case tea.BackgroundColorMsg:
		if model.theme.Mode() == theme.ModeAuto {
			model.theme = model.theme.WithDark(message.IsDark())
			model.styles = model.theme.Styles()
		}
		return model, nil
	case projectAgentsLoadMsg:
		if message.sequence != model.loadSequence {
			return model, nil
		}
		model.loading = false
		if message.err != nil {
			model.sourceError = termsafe.SafeLineMax(message.err.Error(), 160)
			model.footer = model.styles.Danger.Render("! source unavailable: " + model.sourceError)
			return model, nil
		}
		model.snapshot = message.snapshot
		model.hasSnapshot = true
		model.sourceError = ""
		model.reconcileSelection()
		model.footer = ""
		return model, nil
	case projectAgentsTickMsg:
		return model, tea.Batch(model.startLoad(), model.tick())
	case projectAgentsActionMsg:
		model.actionBusy = false
		if message.err != nil {
			model.footer = model.styles.Danger.Render("! " + termsafe.SafeLineMax(message.err.Error(), 160))
		} else {
			model.footer = model.styles.Muted.Render("opened " + termsafe.SafeLineMax(model.selected, 64))
		}
		return model, nil
	case tea.KeyPressMsg:
		return model.updateKey(message)
	default:
		return model, nil
	}
}

func (model *projectAgentsModel) updateKey(key tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch key.String() {
	case "q", "ctrl+c":
		return model, tea.Quit
	case "up", "k":
		model.moveSelection(-1)
	case "down", "j":
		model.moveSelection(1)
	case "enter":
		return model, model.startAction(false)
	case "e":
		return model, model.startAction(true)
	case "+", "=":
		model.window = maxProjectAgentsWindow(model.window + 5*time.Minute)
	case "-", "_":
		model.window = maxProjectAgentsWindow(model.window - 5*time.Minute)
	}
	return model, nil
}

func (model *projectAgentsModel) startAction(evidence bool) tea.Cmd {
	if model.actionBusy {
		return nil
	}
	row, ok := model.selectedRow()
	if !ok {
		return nil
	}
	callback := model.focus
	label := "opened "
	if evidence {
		callback = model.openEvidence
		label = "evidence "
	}
	if callback == nil {
		model.footer = model.styles.Warn.Render("! " + label + "action unavailable")
		return nil
	}
	model.actionBusy = true
	ctx := model.ctx
	threadID, runID := row.ThreadID, row.RunID
	return func() tea.Msg {
		return projectAgentsActionMsg{err: callback(ctx, threadID, runID)}
	}
}

func (model *projectAgentsModel) reconcileSelection() {
	rows := model.rows()
	for _, row := range rows {
		if row.ThreadID == model.selected {
			model.ensureVisible()
			return
		}
	}
	if len(rows) == 0 {
		model.selected = ""
		model.offset = 0
		return
	}
	model.selected = rows[0].ThreadID
	model.offset = 0
}

func (model *projectAgentsModel) moveSelection(delta int) {
	rows := model.rows()
	if len(rows) == 0 {
		return
	}
	index := 0
	for rowIndex, row := range rows {
		if row.ThreadID == model.selected {
			index = rowIndex
			break
		}
	}
	index += delta
	if index < 0 {
		index = 0
	}
	if index >= len(rows) {
		index = len(rows) - 1
	}
	model.selected = rows[index].ThreadID
	model.ensureVisible()
}

func (model projectAgentsModel) selectedRow() (projectview.AgentRow, bool) {
	for _, row := range model.rows() {
		if row.ThreadID == model.selected {
			return row, true
		}
	}
	return projectview.AgentRow{}, false
}

func (model projectAgentsModel) rows() []projectview.AgentRow {
	rows := model.snapshot.Rows
	if len(rows) > projectview.MaxAgentRows {
		rows = rows[:projectview.MaxAgentRows]
	}
	return rows
}

func (model *projectAgentsModel) ensureVisible() {
	rows := model.rows()
	if len(rows) == 0 {
		model.offset = 0
		return
	}
	index := 0
	for rowIndex, row := range rows {
		if row.ThreadID == model.selected {
			index = rowIndex
			break
		}
	}
	room := model.rowRoom()
	if index < model.offset {
		model.offset = index
	}
	if index >= model.offset+room {
		model.offset = index - room + 1
	}
	maxOffset := max(len(rows)-room, 0)
	if model.offset > maxOffset {
		model.offset = maxOffset
	}
	if model.offset < 0 {
		model.offset = 0
	}
}

func (model projectAgentsModel) rowRoom() int {
	if model.height <= 0 {
		return len(model.rows())
	}
	if model.width > 0 && model.width < projectAgentsMinWidth {
		return max(model.height-2, 1)
	}
	return max((model.height-3)/2, 1)
}

func maxProjectAgentsWindow(value time.Duration) time.Duration {
	if value < projectAgentsMinView {
		return projectAgentsMinView
	}
	if value > projectAgentsMaxView {
		return projectAgentsMaxView
	}
	return value
}

func (model projectAgentsModel) View() tea.View {
	now := time.Now()
	if model.now != nil {
		now = model.now()
	}
	content := renderProjectAgents(model.snapshot, model.width, model.height, model.selected, model.window, now, model.ascii, model.theme, model.sourceError, model.loading, model.footer)
	view := tea.NewView(content)
	view.AltScreen = true
	view.WindowTitle = "forgectl Agents"
	return view
}

func RenderProjectAgents(snapshot projectview.AgentsSnapshot, width, height int, selection string, window time.Duration, now time.Time, ascii bool, th theme.Theme) string {
	return renderProjectAgents(snapshot, width, height, selection, window, now, ascii, th, "", false, "")
}

func renderProjectAgents(snapshot projectview.AgentsSnapshot, width, height int, selection string, window time.Duration, now time.Time, ascii bool, th theme.Theme, sourceError string, loading bool, footer string) string {
	if width <= 0 {
		width = 80
	}
	if height <= 0 {
		height = 24
	}
	if window <= 0 {
		window = projectAgentsDefaultView
	}
	if now.IsZero() {
		now = snapshot.ObservedAt
	}
	if now.IsZero() {
		now = time.Now()
	}
	styles := th.Styles()
	separator := projectAgentsSeparator(ascii)
	rows := snapshot.Rows
	if len(rows) > projectview.MaxAgentRows {
		rows = rows[:projectview.MaxAgentRows]
	}
	active := countProjectAgentsActive(rows, now, sourceError)
	windowLabel := projectAgentsWindowLabel(window)
	mark := "●"
	if ascii {
		mark = "*"
	}
	header := styles.Brand.Render(mark) + " " + styles.Header.Render("Agents")
	header += styles.Muted.Render(fmt.Sprintf("  %d active%sLast %s", active, separator, windowLabel))
	var notes []string
	if snapshot.Partial {
		notes = append(notes, "partial")
	}
	if snapshot.DroppedThreads > 0 {
		notes = append(notes, fmt.Sprintf("dropped %d threads", snapshot.DroppedThreads))
	}
	if snapshot.DroppedSpans > 0 {
		notes = append(notes, fmt.Sprintf("dropped %d spans", snapshot.DroppedSpans))
	}
	if snapshot.Reason != "" {
		notes = append(notes, "reason: "+safeProjectAgents(snapshot.Reason, 80))
	}
	if len(notes) > 0 {
		header += styles.Warn.Render(separator + strings.Join(notes, separator))
	}
	if sourceError != "" {
		header += styles.Danger.Render(separator + "source unavailable")
	} else if loading {
		loadingLabel := "loading…"
		if ascii {
			loadingLabel = "loading..."
		}
		header += styles.Muted.Render(separator + loadingLabel)
	}
	if snapshot.ProjectName != "" {
		header += styles.Dim.Render(separator + safeProjectAgents(snapshot.ProjectName, 48))
	}
	lines := []string{fitProjectAgentsLine(header, width)}
	narrow := width < projectAgentsMinWidth
	if len(rows) == 0 {
		lines = append(lines, fitProjectAgentsLine(styles.Muted.Render("No agent threads observed"), width))
	} else if narrow {
		room := max(height-len(lines)-1, 1)
		start, end := projectAgentsWindowRows(rows, selection, room)
		for _, row := range rows[start:end] {
			lines = append(lines, renderProjectAgentsListRow(row, row.ThreadID == selection, width, ascii, styles, now, sourceError != ""))
		}
	} else {
		timelineStart := now.Add(-window)
		barWidth := max(width-projectAgentsNameWidth-3, 16)
		lines = append(lines, renderProjectAgentsAxis(timelineStart, now, barWidth, ascii, styles))
		room := max((height-len(lines)-1)/2, 1)
		start, end := projectAgentsWindowRows(rows, selection, room)
		for _, row := range rows[start:end] {
			lines = append(lines, strings.Split(renderProjectAgentsRow(row, row.ThreadID == selection, timelineStart, now, now, width, barWidth, ascii, styles, sourceError != ""), "\n")...)
		}
	}
	if footer == "" {
		footerText := "↑↓/jk move · enter focus · e evidence · +/- window · q quit"
		if ascii {
			footerText = "up/down j/k move - enter focus - e evidence - +/- window - q quit"
		}
		footer = styles.Muted.Render(footerText)
	}
	lines = append(lines, fitProjectAgentsLine(footer, width))
	if height > 0 && len(lines) > height {
		lines = lines[:height]
	}
	for len(lines) < height {
		lines = append(lines, "")
	}
	return strings.Join(lines, "\n")
}

func projectAgentsWindowRows(rows []projectview.AgentRow, selection string, room int) (int, int) {
	if room < 1 {
		room = 1
	}
	if len(rows) <= room {
		return 0, len(rows)
	}
	index := 0
	for rowIndex, row := range rows {
		if row.ThreadID == selection {
			index = rowIndex
			break
		}
	}
	start := index
	if start+room > len(rows) {
		start = len(rows) - room
	}
	return start, minProjectAgentsInt(start+room, len(rows))
}

func renderProjectAgentsAxis(start, end time.Time, width int, ascii bool, styles theme.Styles) string {
	left := start.Format("15:04")
	middle := start.Add(end.Sub(start) / 2).Format("15:04")
	right := end.Format("15:04")
	line := strings.Repeat("-", width)
	if !ascii {
		line = strings.Repeat("─", width)
	}
	line = replaceProjectAgentsAt(line, 0, left)
	line = replaceProjectAgentsAt(line, max((width-ansi.StringWidth(middle))/2, 0), middle)
	line = replaceProjectAgentsAt(line, max(width-ansi.StringWidth(right), 0), right)
	return styles.Dim.Render(strings.Repeat(" ", projectAgentsNameWidth+1) + fitProjectAgentsText(line, width))
}

func renderProjectAgentsRow(row projectview.AgentRow, selected bool, start, end, now time.Time, totalWidth, barWidth int, ascii bool, styles theme.Styles, sourceError bool) string {
	name := safeProjectAgents(row.Name, 64)
	if name == "" {
		name = "(unnamed thread)"
	}
	name = fitProjectAgentsText(name, projectAgentsNameWidth)
	lead := "  "
	if selected {
		lead = styles.Selected.Render("> ")
	}
	bar := projectAgentsBar(row, start, end, barWidth, ascii, styles, now, sourceError)
	first := lead + fitProjectAgentsText(name, projectAgentsNameWidth) + " " + bar
	subtitle := projectAgentsSubtitle(row)
	state := projectAgentsState(row, now, sourceError)
	coverage := projectAgentsCoverage(row)
	labels := projectAgentsActivityLabels(row, now, sourceError)
	leftColumn := strings.Repeat(" ", 2) + fitProjectAgentsText(subtitle, projectAgentsNameWidth)
	rightColumn := state + projectAgentsSeparator(ascii) + coverage + projectAgentsSeparator(ascii) + labels
	second := leftColumn + " " + rightColumn
	return fitProjectAgentsLine(first, totalWidth) + "\n" + fitProjectAgentsLine(second, totalWidth)
}

func renderProjectAgentsListRow(row projectview.AgentRow, selected bool, width int, ascii bool, styles theme.Styles, now time.Time, sourceError bool) string {
	name := safeProjectAgents(row.Name, 48)
	subtitle := projectAgentsSubtitle(row)
	state := projectAgentsState(row, now, sourceError)
	text := name + projectAgentsSeparator(ascii) + subtitle + projectAgentsSeparator(ascii) + state
	if selected {
		text = "> " + text
	} else {
		text = "  " + text
	}
	if selected {
		return fitProjectAgentsLine(styles.Selected.Render(text), width)
	}
	return fitProjectAgentsLine(styles.Fg.Render(text), width)
}

func projectAgentsBar(row projectview.AgentRow, start, end time.Time, width int, ascii bool, styles theme.Styles, now time.Time, sourceError bool) string {
	if width < 1 {
		return ""
	}
	base := " "
	if ascii {
		base = "."
	}
	cells := make([]string, width)
	for cellIndex := range cells {
		cells[cellIndex] = base
	}
	rowFresh := projectAgentsRowFresh(row, now, sourceError)
	for index, item := range row.Spans {
		if index >= activity.MaxSpans {
			break
		}
		span := item.Span
		capAt, hasCap := projectAgentsSpanCap(row, item, end, rowFresh)
		left, right, point, visible := projectAgentsSpanGeometry(span, start, end, width, capAt, hasCap)
		if !visible {
			continue
		}
		displayState := span.State
		if !rowFresh && displayState == activity.Running {
			displayState = activity.Unknown
		}
		style := projectAgentsSpanStyle(displayState, styles)
		glyph := projectAgentsSpanGlyph(displayState, ascii)
		if point {
			cells[left] = style.Render(glyph)
			continue
		}
		for index := left; index <= right && index < width; index++ {
			cells[index] = style.Render(glyph)
		}
	}
	projectAgentsBarLabels(cells, row, rowFresh, start, end, width, styles)
	for index, point := range row.Points {
		if index >= activity.MaxSpans {
			break
		}
		if point.At.IsZero() {
			cells[0] = styles.Dim.Render(projectAgentsPointGlyph(ascii))
			continue
		}
		capAt, hasCap := projectAgentsPointCap(row, end, rowFresh)
		if hasCap && point.At.After(capAt) {
			continue
		}
		if point.At.Before(start) || point.At.After(end) {
			continue
		}
		position := projectAgentsTimePosition(point.At, start, end, width)
		cells[position] = projectAgentsSpanStyle(point.State, styles).Render(projectAgentsPointGlyph(ascii))
	}
	return strings.Join(cells, "")
}

func projectAgentsBarLabels(cells []string, row projectview.AgentRow, rowFresh bool, start, end time.Time, width int, styles theme.Styles) {
	if width < 40 {
		return
	}
	used := make([]bool, width)
	for index, item := range row.Spans {
		if index >= activity.MaxSpans {
			break
		}
		span := item.Span
		capAt, hasCap := projectAgentsSpanCap(row, item, end, rowFresh)
		left, right, point, visible := projectAgentsSpanGeometry(span, start, end, width, capAt, hasCap)
		if !visible || point {
			continue
		}
		label := projectAgentsSpanIdentifier(span)
		if label == "" || right-left+1 < ansi.StringWidth(label)+2 {
			continue
		}
		labelStart := left + (right-left+1-ansi.StringWidth(label))/2
		labelEnd := labelStart + ansi.StringWidth(label)
		if labelStart <= left || labelEnd >= right+1 {
			continue
		}
		occupied := false
		for position := labelStart; position < labelEnd; position++ {
			if used[position] {
				occupied = true
				break
			}
		}
		if occupied {
			continue
		}
		displayState := span.State
		if !rowFresh && displayState == activity.Running {
			displayState = activity.Unknown
		}
		style := projectAgentsSpanStyle(displayState, styles)
		for offset, character := range []rune(label) {
			position := labelStart + offset
			cells[position] = style.Render(string(character))
			used[position] = true
		}
	}
}

func projectAgentsSpanGeometry(span activity.Span, start, end time.Time, width int, capAt time.Time, hasCap bool) (int, int, bool, bool) {
	if width < 1 || !end.After(start) {
		return 0, 0, false, false
	}
	if span.StartedAt.IsZero() {
		return 0, 0, true, true
	}
	var spanEnd time.Time
	hasEnd := false
	if span.EndedAt != nil {
		spanEnd, hasEnd = *span.EndedAt, true
	} else if hasCap {
		spanEnd, hasEnd = capAt, true
	} else if span.State == activity.Running {
		spanEnd, hasEnd = end, true
	}
	if span.StartedAt.After(end) || (hasEnd && spanEnd.Before(start)) {
		return 0, 0, false, false
	}
	if hasEnd {
		if spanEnd.After(end) {
			spanEnd = end
		}
		if hasCap && spanEnd.After(capAt) {
			spanEnd = capAt
		}
	}
	if !hasEnd {
		position := projectAgentsTimePosition(span.StartedAt, start, end, width)
		return position, position, true, true
	}
	if spanEnd.Before(span.StartedAt) {
		position := projectAgentsTimePosition(span.StartedAt, start, end, width)
		return position, position, true, true
	}
	left := projectAgentsTimePosition(maxProjectAgentsTime(span.StartedAt, start), start, end, width)
	right := projectAgentsTimePosition(minProjectAgentsTime(spanEnd, end), start, end, width)
	if right < left {
		return left, left, true, true
	}
	return left, right, false, true
}

func projectAgentsSpanCap(row projectview.AgentRow, item projectview.AgentSpan, end time.Time, fresh bool) (time.Time, bool) {
	if fresh && item.Span.State == activity.Running {
		return end, true
	}
	if item.ObservedThrough != nil {
		return *item.ObservedThrough, true
	}
	if !row.ObservedAt.IsZero() {
		return row.ObservedAt, true
	}
	return time.Time{}, false
}

func projectAgentsPointCap(row projectview.AgentRow, end time.Time, fresh bool) (time.Time, bool) {
	if fresh {
		return end, true
	}
	if !row.ObservedAt.IsZero() {
		return row.ObservedAt, true
	}
	return time.Time{}, false
}

func projectAgentsPointGlyph(ascii bool) string {
	if ascii {
		return "o"
	}
	return "●"
}

func projectAgentsTimePosition(value, start, end time.Time, width int) int {
	seconds := value.Sub(start).Seconds()
	ratio := seconds / end.Sub(start).Seconds()
	position := int(ratio * float64(width-1))
	if position < 0 {
		return 0
	}
	if position >= width {
		return width - 1
	}
	return position
}

func projectAgentsSpanStyle(state activity.State, styles theme.Styles) lipgloss.Style {
	switch state {
	case activity.Running:
		return styles.Active
	case activity.Finished:
		return styles.OK
	case activity.Failed:
		return styles.Danger
	case activity.Incomplete:
		return styles.Warn
	default:
		return styles.Dim
	}
}

func projectAgentsSpanGlyph(state activity.State, ascii bool) string {
	if state == activity.Unknown {
		return "?"
	}
	if ascii {
		return "="
	}
	return "━"
}

func projectAgentsSpanIdentifier(span activity.Span) string {
	identifier := safeProjectAgents(span.ID, 16)
	if identifier == "" {
		identifier = safeProjectAgents(span.TurnID, 16)
	}
	return capProjectAgentsText(identifier, 16)
}

func projectAgentsSubtitle(row projectview.AgentRow) string {
	harness := safeProjectAgents(row.Harness, 24)
	model := safeProjectAgents(row.Model, 40)
	if model != "" {
		return capProjectAgentsText(harness+" / "+model, 68)
	}
	if harness != "" {
		return harness + " / model unknown"
	}
	return "harness unknown / model unknown"
}

func projectAgentsActivityLabels(row projectview.AgentRow, now time.Time, sourceError bool) string {
	labels := make([]string, 0, 4)
	seen := make(map[string]bool)
	for _, item := range row.Spans {
		state := item.Span.State
		if state == activity.Running && !projectAgentsRowFresh(row, now, sourceError) {
			state = activity.Unknown
		}
		label := projectAgentsSourceName(item.Source) + "/" + string(state)
		if label == "" {
			label = "native activity"
		}
		if seen[label] {
			continue
		}
		seen[label] = true
		labels = append(labels, safeProjectAgents(label, 40))
		if len(labels) == 4 {
			break
		}
	}
	if len(labels) == 0 {
		for _, point := range row.Points {
			if len(labels) == 4 {
				break
			}
			labels = append(labels, safeProjectAgents(projectAgentsSourceName(point.Source)+"/point/"+string(point.State), 40))
		}
	}
	if row.Partial {
		labels = append(labels, "partial")
	}
	if row.Reason != "" {
		labels = append(labels, "reason/"+safeProjectAgents(row.Reason, 60))
	}
	if len(labels) == 0 {
		return "native activity unavailable"
	}
	return "native activity: " + strings.Join(labels, ", ")
}

func projectAgentsSourceName(source activity.Source) string {
	switch source {
	case activity.SourceCodex:
		return "codex"
	case activity.SourcePi:
		return "pi"
	case activity.SourceHermes:
		return "hermes"
	default:
		return safeProjectAgents(string(source), 24)
	}
}

func projectAgentsSeparator(ascii bool) string {
	if ascii {
		return " - "
	}
	return " · "
}

func projectAgentsState(row projectview.AgentRow, now time.Time, sourceError bool) string {
	if strings.EqualFold(row.State, "active") && !projectAgentsRowFresh(row, now, sourceError) {
		return "unknown"
	}
	state := strings.ToLower(safeProjectAgents(row.State, 24))
	if state == "" {
		return "unknown"
	}
	return state
}

func projectAgentsRowFresh(row projectview.AgentRow, now time.Time, sourceError bool) bool {
	return !sourceError && strings.EqualFold(row.State, "active") && projectAgentsFresh(row.ObservedAt, now)
}

func projectAgentsCoverage(row projectview.AgentRow) string {
	if row.Coverage == "" {
		return "coverage unknown"
	}
	return safeProjectAgents(string(row.Coverage), 24)
}

func countProjectAgentsActive(rows []projectview.AgentRow, now time.Time, sourceError string) int {
	if sourceError != "" {
		return 0
	}
	count := 0
	for _, row := range rows {
		if strings.EqualFold(row.State, "active") && projectAgentsFresh(row.ObservedAt, now) {
			count++
		}
	}
	return count
}

func projectAgentsFresh(observed, now time.Time) bool {
	if observed.IsZero() || now.IsZero() || observed.After(now.Add(time.Second)) {
		return false
	}
	return now.Sub(observed) <= projectAgentsFreshFor
}

func projectAgentsWindowLabel(window time.Duration) string {
	minutes := int(window / time.Minute)
	if minutes <= 0 {
		minutes = 30
	}
	return fmt.Sprintf("%dm", minutes)
}

func safeProjectAgents(value string, limit int) string {
	return termsafe.SafeLineMax(strings.TrimSpace(value), limit)
}

func fitProjectAgentsText(value string, width int) string {
	if width <= 0 {
		return ""
	}
	if ansi.StringWidth(value) > width {
		value = ansi.Truncate(value, width, "")
	}
	return value + strings.Repeat(" ", width-ansi.StringWidth(value))
}

func capProjectAgentsText(value string, width int) string {
	if width <= 0 {
		return ""
	}
	if ansi.StringWidth(value) <= width {
		return value
	}
	return ansi.Truncate(value, width, "")
}

func fitProjectAgentsLine(value string, width int) string {
	if width <= 0 {
		return ""
	}
	if ansi.StringWidth(value) <= width {
		return value
	}
	return ansi.Truncate(value, width, "")
}

func replaceProjectAgentsAt(value string, position int, replacement string) string {
	if position < 0 {
		position = 0
	}
	runes := []rune(value)
	for index, character := range []rune(replacement) {
		at := position + index
		if at >= 0 && at < len(runes) {
			runes[at] = character
		}
	}
	return string(runes)
}

func maxProjectAgentsTime(left, right time.Time) time.Time {
	if left.After(right) {
		return left
	}
	return right
}

func minProjectAgentsTime(left, right time.Time) time.Time {
	if left.Before(right) {
		return left
	}
	return right
}

func minProjectAgentsInt(left, right int) int {
	if left < right {
		return left
	}
	return right
}
