package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/cameronsjo/forgectl/internal/meta"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// HubHeader is the hub's one-line status (forgectl#730 item 1). Every field
// is read locally by the caller — no network — and each carries its own
// availability, so a source that failed or timed out drops out of the line
// instead of rendering as a confident zero.
type HubHeader struct {
	// Project and Branch come from the cwd's git checkout. Either may be
	// empty; an empty Project omits both.
	Project string
	Branch  string

	HasTmux      bool
	TmuxSessions int

	HasReviews     bool
	ReviewsRunning int
	ReviewsQueued  int

	HasDoctor    bool
	DoctorResult string
	DoctorAge    time.Duration
}

// hubHeaderValueMax caps each free-text header value (project, branch, doctor
// result) in rendered runes. A branch name is chosen by whoever pushed it, so
// its length is not ours to trust on a one-line header.
const hubHeaderValueMax = 32

// Line assembles the header, omitting every unavailable field. It returns ""
// when nothing is available, and the caller then shows the plain title.
func (h HubHeader) Line() string {
	var parts []string
	if project := capSafe(h.Project, hubHeaderValueMax); project != "" {
		if branch := capSafe(h.Branch, hubHeaderValueMax); branch != "" {
			project += " @ " + branch
		}
		parts = append(parts, project)
	}
	if h.HasTmux && h.TmuxSessions >= 0 {
		parts = append(parts, fmt.Sprintf("%d tmux", h.TmuxSessions))
	}
	if h.HasReviews && h.ReviewsRunning >= 0 && h.ReviewsQueued >= 0 {
		parts = append(parts, reviewsPhrase(h.ReviewsRunning, h.ReviewsQueued))
	}
	if h.HasDoctor {
		if result := capSafe(h.DoctorResult, hubHeaderValueMax); result != "" {
			field := "doctor " + result
			if age := ageLabel(h.DoctorAge); age != "" {
				field += " (" + age + ")"
			}
			parts = append(parts, field)
		}
	}
	return strings.Join(parts, " · ")
}

// reviewsPhrase renders the lifecycle store's counts in the mockup's words.
func reviewsPhrase(running, queued int) string {
	plural := func(n int) string {
		if n == 1 {
			return ""
		}
		return "s"
	}
	switch {
	case running > 0 && queued > 0:
		return fmt.Sprintf("%d review%s running, %d queued", running, plural(running), queued)
	case running > 0:
		return fmt.Sprintf("%d review%s running", running, plural(running))
	case queued > 0:
		return fmt.Sprintf("%d review%s queued", queued, plural(queued))
	default:
		return "no reviews"
	}
}

// ageLabel is a coarse age: minutes under an hour, hours under two days, then
// days. A negative age (a clock that moved backwards) renders nothing rather
// than a nonsense value.
func ageLabel(d time.Duration) string {
	switch {
	case d < 0:
		return ""
	case d < time.Minute:
		return "<1m"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	default:
		return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
	}
}

// capSafe renders s as one inert terminal line of at most maxRunes runes,
// ending in "…" when it was cut. It escapes rune by rune (termsafe.SafeLine
// over each), so a cut never splits an escape into text that reads as
// something else.
func capSafe(s string, maxRunes int) string {
	s = strings.TrimSpace(s)
	var out strings.Builder
	used := 0
	for _, r := range s {
		piece := termsafe.SafeLine(string(r))
		n := utf8.RuneCountInString(piece)
		if used+n > maxRunes {
			out.WriteString("…")
			return out.String()
		}
		out.WriteString(piece)
		used += n
	}
	return out.String()
}

// --- inline argument picker (forgectl#730 item 4) ---

// ArgSource lists picker candidates for one command path. It must stay local:
// the hub makes no network calls, and a picker opening is no reason to start.
// It is also bounded: a source still running after pickerSourceBudget is
// abandoned and the picker opens without candidates.
type ArgSource func(ctx context.Context) []string

// ArgvBuilder turns a picker choice into the argv that runs. The caller
// supplies one that knows the live command tree (RunOptions.BuildArgv): it
// must refuse a value that would dispatch to any command other than the one
// the row names — a typed "drain" under `pr <ref>` is `forgectl pr drain`,
// not a ref. PickerArgv is the tree-blind default and the validation every
// builder starts from.
type ArgvBuilder func(prefix []string, arg string, optional bool) ([]string, error)

// pickerSourceBudget bounds an ArgSource, so a slow walk cannot stall the
// keypress that opened the picker.
const pickerSourceBudget = 300 * time.Millisecond

const (
	// pickerArgMaxRunes caps one typed or picked argument. A ref, a URL, or a
	// project query fits with room to spare.
	pickerArgMaxRunes = 256
	// pickerInputMaxRunes bounds the edit buffer itself, so a runaway paste
	// cannot grow it without limit before validation refuses it.
	pickerInputMaxRunes = 1024
	// pickerCandidateMax caps how many candidates a source may contribute.
	pickerCandidateMax = 500
	// pickerVisibleRows is how many picker rows show at once.
	pickerVisibleRows = 5
	// pickerCandidateDisplayMax caps a candidate's rendered width.
	pickerCandidateDisplayMax = 60
	// pickerInputDisplayMax caps the edit line's rendered width, so a long
	// value never wraps the box past the lines applySize reserved for it.
	pickerInputDisplayMax = 60
)

// tailSafe renders the end of input — the part being typed — as one inert
// line of at most maxRunes runes, led by "…" when the head was dropped.
func tailSafe(input []rune, maxRunes int) string {
	pieces := make([]string, len(input))
	total := 0
	for i, r := range input {
		pieces[i] = termsafe.SafeLine(string(r))
		total += utf8.RuneCountInString(pieces[i])
	}
	if total <= maxRunes {
		return strings.Join(pieces, "")
	}
	start, used := len(pieces), 0
	for start > 0 && used+utf8.RuneCountInString(pieces[start-1]) <= maxRunes-1 {
		start--
		used += utf8.RuneCountInString(pieces[start])
	}
	return "…" + strings.Join(pieces[start:], "")
}

// pickerSpec reads a cobra Use line and reports whether its command takes the
// one positional argument the picker can supply. placeholder is that
// argument's own text ("<ref>"); optional is true for a bracketed one, which
// the picker lets the operator leave empty.
//
// It is deliberately conservative. Two or more required positionals, a
// variadic one (required or optional), or any bare word outside a <> or []
// group means the picker cannot build a correct argv, and the row keeps the
// older behavior of printing the invocation to finish by hand. A group whose
// text starts with "-" is a flag placeholder, not a positional, and is
// skipped.
//
// The variadic rule is what keeps the picker off `launch [harness args…]`
// (forgectl#730 review): a launch argument is a harness argv, and the
// launcher classifies its first word — `update` becomes `claude update`,
// `agents` opens the agents posture — so no single typed value is a safe
// "plain argument" there. The picker is for one positional; a list of them
// is finished by hand.
func pickerSpec(use string) (placeholder string, optional bool, ok bool) {
	_, rest, _ := strings.Cut(use, " ")
	groups, clean := useGroups(rest)
	if !clean {
		return "", false, false
	}
	var positional []string
	for _, g := range groups {
		if strings.HasPrefix(strings.TrimSpace(g[1:len(g)-1]), "-") {
			continue
		}
		positional = append(positional, g)
	}
	if len(positional) == 0 {
		return "", false, false
	}
	required := 0
	for _, g := range positional {
		if isVariadic(g) {
			return "", false, false
		}
		if g[0] == '<' {
			required++
		}
	}
	switch {
	case required == 0:
		return positional[0], true, true
	case required == 1 && positional[0][0] == '<':
		return positional[0], false, true
	default:
		return "", false, false
	}
}

// PickerSpec is pickerSpec for callers outside this package: the hub's argv
// builder and its tests use it to find every command the picker can open on.
func PickerSpec(use string) (placeholder string, optional bool, ok bool) {
	return pickerSpec(use)
}

// moduleNeedsArg reports whether a module row's own command requires exactly
// one argument the picker can supply (pr <ref>). Enter on such a row opens the
// picker rather than the drill-down list. A row that opted out of the picker
// (NoPicker) drills into its leaves instead, where its own invocation prints.
func moduleNeedsArg(e HubEntry) bool {
	if e.NoPicker {
		return false
	}
	_, optional, ok := pickerSpec(e.Use)
	return ok && !optional
}

func isVariadic(group string) bool {
	return strings.Contains(group, "...") || strings.Contains(group, "…")
}

// useGroups splits the placeholder part of a Use line into its top-level <>
// and [] groups. clean is false when any non-space text sits outside a group
// or a bracket is unbalanced.
func useGroups(s string) (groups []string, clean bool) {
	depth, start := 0, -1
	for i, r := range s {
		switch r {
		case '<', '[':
			if depth == 0 {
				start = i
			}
			depth++
		case '>', ']':
			depth--
			if depth < 0 {
				return nil, false
			}
			if depth == 0 {
				groups = append(groups, s[start:i+1])
			}
		default:
			if depth == 0 && r != ' ' {
				return nil, false
			}
		}
	}
	return groups, depth == 0
}

// validatePickerArg refuses an argument the picker will not hand to the
// command. The rules exist because the value becomes one argv element:
//
//   - a leading "-" would be parsed as a flag, not the positional the row asked
//     for (`--agent=…` typed into the pr picker would pick the review agent);
//   - a control or bidi character would reach the echo line and the command;
//   - an invisible or blank-rendering rune (U+200B, U+FEFF, a tag character,
//     a variation selector, a Hangul filler, U+2800) would make the argument
//     differ from the ref or name it looks like (#916, #948);
//   - a blank or oversized value is never a real ref, URL, or query.
func validatePickerArg(arg string, optional bool) error {
	if arg == "" {
		if optional {
			return nil
		}
		return errors.New("type a value or pick one")
	}
	if !utf8.ValidString(arg) {
		return errors.New("the value is not valid UTF-8")
	}
	if strings.TrimSpace(arg) == "" {
		return errors.New("the value is blank")
	}
	if utf8.RuneCountInString(arg) > pickerArgMaxRunes {
		return fmt.Errorf("the value is longer than %d characters", pickerArgMaxRunes)
	}
	if strings.HasPrefix(arg, "-") {
		return errors.New("the value can't start with - (it would be read as a flag)")
	}
	for _, r := range arg {
		if termsafe.IsUnsafeTerminalRune(r) {
			return errors.New("the value holds a control character")
		}
		if termsafe.IsInvisibleRune(r) {
			return errors.New("the value holds an invisible character")
		}
	}
	return nil
}

// PickerArgv builds the argv a picker choice runs: prefix, then arg as exactly
// one element. arg is never split, trimmed, or interpreted — there is no shell
// anywhere between the picker and the command. An empty optional arg runs
// prefix alone.
func PickerArgv(prefix []string, arg string, optional bool) ([]string, error) {
	if err := validatePickerArg(arg, optional); err != nil {
		return nil, err
	}
	argv := make([]string, len(prefix), len(prefix)+1)
	copy(argv, prefix)
	if arg != "" {
		argv = append(argv, arg)
	}
	return argv, nil
}

// DisplayArgv renders argv for the "$ forgectl …" line so it shows exactly
// the elements that will run. A token that is plain is shown as is; one that
// holds a terminal-unsafe rune is shown Go-quoted, with the rune visibly
// escaped; any other token that a shell would split or expand is
// single-quoted. The line is display only — nothing ever parses it back.
func DisplayArgv(argv []string) string {
	out := make([]string, 0, len(argv))
	for _, tok := range argv {
		out = append(out, displayToken(tok))
	}
	return strings.Join(out, " ")
}

func displayToken(tok string) string {
	if tok == "" {
		return "''"
	}
	if !utf8.ValidString(tok) {
		return termsafe.QuoteText(tok)
	}
	for _, r := range tok {
		if termsafe.IsUnsafeTerminalRune(r) {
			return termsafe.QuoteText(tok)
		}
	}
	plain := tok[0] != '#' && tok[0] != '~' && tok[0] != '='
	for _, r := range tok {
		if !isPlainShellRune(r) {
			plain = false
			break
		}
	}
	if plain {
		return tok
	}
	return termsafe.SafeLine("'" + strings.ReplaceAll(tok, "'", `'\''`) + "'")
}

func isPlainShellRune(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true
	}
	return strings.ContainsRune("-_./:@#%+=,", r)
}

// dollarLine is the "$ forgectl …" text for argv.
func dollarLine(argv []string) string {
	return "$ " + meta.AppName + " " + DisplayArgv(argv)
}

type pickerRowKind int

const (
	pickerRowLiteral pickerRowKind = iota
	pickerRowCandidate
	pickerRowBrowse
)

type pickerRow struct {
	kind  pickerRowKind
	value string
}

// argPicker is the in-place argument prompt. It edits its own rune buffer
// rather than wrapping a text input so every character it draws goes through
// termsafe, and so the value it hands to PickerArgv is exactly what was typed.
type argPicker struct {
	prefix      []string
	placeholder string
	optional    bool
	input       []rune
	candidates  []string
	cursor      int
	// browse, when set, adds a last row that drills into the entry's
	// subcommands (pr's own row opens the picker, not its leaves).
	browse  *HubEntry
	errText string
	// build turns the current choice into argv; never nil.
	build ArgvBuilder
}

func newArgPicker(ctx context.Context, prefix []string, placeholder string, optional bool, source ArgSource, browse *HubEntry, build ArgvBuilder) *argPicker {
	if build == nil {
		build = PickerArgv
	}
	p := &argPicker{
		prefix:      append([]string(nil), prefix...),
		placeholder: placeholder,
		optional:    optional,
		browse:      browse,
		build:       build,
	}
	for _, c := range boundedSource(ctx, source) {
		if len(p.candidates) == pickerCandidateMax {
			break
		}
		// A candidate runs through the same builder a typed value does, so a
		// project that happens to be named like a subcommand is never offered.
		if _, err := build(p.prefix, c, false); err != nil {
			continue
		}
		p.candidates = append(p.candidates, c)
	}
	return p
}

// boundedSource runs source under pickerSourceBudget and returns nil when it
// does not answer in time; the abandoned call finishes on its own.
func boundedSource(ctx context.Context, source ArgSource) []string {
	if source == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, pickerSourceBudget)
	defer cancel()
	done := make(chan []string, 1)
	go func() { done <- source(ctx) }()
	select {
	case got := <-done:
		return got
	case <-ctx.Done():
		return nil
	}
}

// rows lists what the cursor moves over: the literal typed text first (free
// text is always accepted), then candidates matching it, then the browse row.
func (p *argPicker) rows() []pickerRow {
	var rows []pickerRow
	typed := string(p.input)
	if typed != "" {
		rows = append(rows, pickerRow{kind: pickerRowLiteral, value: typed})
	}
	needle := strings.ToLower(typed)
	for _, c := range p.candidates {
		if c == typed {
			continue
		}
		if needle == "" || strings.Contains(strings.ToLower(c), needle) {
			rows = append(rows, pickerRow{kind: pickerRowCandidate, value: c})
		}
	}
	if p.browse != nil {
		rows = append(rows, pickerRow{kind: pickerRowBrowse})
	}
	return rows
}

func (p *argPicker) current() (pickerRow, bool) {
	rows := p.rows()
	if len(rows) == 0 {
		return pickerRow{}, false
	}
	if p.cursor >= len(rows) {
		p.cursor = len(rows) - 1
	}
	if p.cursor < 0 {
		p.cursor = 0
	}
	return rows[p.cursor], true
}

func (p *argPicker) typeText(s string) {
	for _, r := range s {
		if len(p.input) >= pickerInputMaxRunes {
			break
		}
		p.input = append(p.input, r)
	}
	p.cursor = 0
	p.errText = ""
}

// pending is the argv the current row would run, or nil with the reason it
// cannot run yet.
func (p *argPicker) pending() ([]string, error) {
	row, ok := p.current()
	if !ok {
		return p.build(p.prefix, "", p.optional)
	}
	if row.kind == pickerRowBrowse {
		return nil, errors.New("browse")
	}
	return p.build(p.prefix, row.value, p.optional)
}

// updatePicker handles every message while the picker is open: the picker
// owns the keyboard until enter runs a choice or esc closes it.
func (m model) updatePicker(msg tea.Msg) (tea.Model, tea.Cmd) {
	p := m.picker
	switch t := msg.(type) {
	case tea.PasteMsg:
		p.typeText(t.Content)
		return m, nil
	case tea.KeyPressMsg:
		switch t.String() {
		case "esc":
			m.closePicker()
			return m, nil
		case "enter":
			return m.submitPicker()
		case "up", "ctrl+p":
			if p.cursor > 0 {
				p.cursor--
			}
			return m, nil
		case "down", "ctrl+n":
			if p.cursor < len(p.rows())-1 {
				p.cursor++
			}
			return m, nil
		case "tab":
			if row, ok := p.current(); ok && row.kind == pickerRowCandidate {
				p.input = []rune(row.value)
				p.cursor = 0
				p.errText = ""
			}
			return m, nil
		case "backspace":
			if len(p.input) > 0 {
				p.input = p.input[:len(p.input)-1]
			}
			p.cursor = 0
			p.errText = ""
			return m, nil
		case "ctrl+u":
			p.input = nil
			p.cursor = 0
			p.errText = ""
			return m, nil
		}
		if t.Text != "" && t.Mod&(tea.ModCtrl|tea.ModAlt) == 0 {
			p.typeText(t.Text)
		}
		return m, nil
	}
	return m, nil
}

func (m model) submitPicker() (tea.Model, tea.Cmd) {
	p := m.picker
	if row, ok := p.current(); ok && row.kind == pickerRowBrowse {
		entry := *p.browse
		m.closePicker()
		m.enterLeaves(entry)
		return m, nil
	}
	argv, err := p.pending()
	if err != nil {
		p.errText = err.Error()
		return m, nil
	}
	m.action = Action{Kind: ActionRunVerb, Argv: argv}
	return m, tea.Quit
}

// openPicker opens the argument picker in place for a command whose argv
// before the argument is prefix. It reports false when use does not describe
// a single argument the picker can supply, or when the row opted out of the
// picker (noPicker: its argument is another CLI's subcommand).
func (m *model) openPicker(prefix []string, use string, noPicker bool, browse *HubEntry) bool {
	if noPicker {
		return false
	}
	placeholder, optional, ok := pickerSpec(use)
	if !ok {
		return false
	}
	var source ArgSource
	if m.argSources != nil {
		source = m.argSources[strings.Join(prefix, " ")]
	}
	m.picker = newArgPicker(m.ctx, prefix, placeholder, optional, source, browse, m.buildArgv)
	m.applySize()
	return true
}

func (m *model) closePicker() {
	m.picker = nil
	m.applySize()
}

// pickerLines is how many terminal lines pickerView draws, for applySize.
func pickerLines() int {
	// Border (2) + title + input + rows + hint/error.
	return 2 + 1 + 1 + pickerVisibleRows + 1
}

func (m model) pickerView() string {
	p := m.picker
	s := m.styles
	title := s.Accent.Render(strings.Join(p.prefix, " ") + " " + p.placeholder)
	input := s.Fg.Render("› "+tailSafe(p.input, pickerInputDisplayMax)) + s.Accent.Render("▏")

	p.current() // clamps p.cursor to the rows the input now matches
	rows := p.rows()
	start := 0
	if p.cursor >= pickerVisibleRows {
		start = p.cursor - pickerVisibleRows + 1
	}
	lines := []string{title, input}
	for i := start; i < len(rows) && i < start+pickerVisibleRows; i++ {
		lines = append(lines, m.pickerRowView(rows[i], i == p.cursor))
	}
	for len(lines) < 2+pickerVisibleRows {
		lines = append(lines, "")
	}
	switch {
	case p.errText != "":
		lines = append(lines, s.Danger.Render(termsafe.SafeLineMax("✗ "+p.errText, statusMaxRunes)))
	case len(p.candidates) == 0 && p.optional:
		lines = append(lines, s.Muted.Render("(type a value, or enter to run without one)"))
	case len(p.candidates) == 0:
		lines = append(lines, s.Muted.Render("(type a value for "+p.placeholder+")"))
	default:
		lines = append(lines, s.Muted.Render("(type to filter, or type any value)"))
	}

	width := m.width - 2
	if width > 76 {
		width = 76
	}
	if width < 20 {
		width = 20
	}
	return lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Width(width).Render(strings.Join(lines, "\n"))
}

func (m model) pickerRowView(row pickerRow, selected bool) string {
	s := m.styles
	var text string
	switch row.kind {
	case pickerRowLiteral:
		text = `use "` + capSafe(row.value, pickerCandidateDisplayMax) + `"`
	case pickerRowCandidate:
		text = capSafe(row.value, pickerCandidateDisplayMax)
	case pickerRowBrowse:
		text = "browse " + m.picker.browse.Name + " subcommands…"
	}
	if selected {
		return s.Accent.Render("> ") + s.Selected.Render(text)
	}
	return "  " + s.Fg.Render(text)
}

// pickerDollar is the bottom line while the picker is open: the exact argv
// the current row runs, or the placeholder form while nothing valid is chosen.
func (m model) pickerDollar() string {
	p := m.picker
	if row, ok := p.current(); ok && row.kind == pickerRowBrowse {
		return m.styles.Muted.Render("$ " + meta.AppName + " " + strings.Join(p.prefix, " ") + " <subcommand>")
	}
	if argv, err := p.pending(); err == nil {
		return m.styles.Fg.Render(dollarLine(argv))
	}
	return m.styles.Muted.Render("$ " + meta.AppName + " " + strings.Join(p.prefix, " ") + " " + p.placeholder)
}
