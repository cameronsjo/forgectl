// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package runview

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// A Lens teaches forgectl to read one app's log as a run (ADR-0014): how a
// line splits into an event name, a step, a time and fields, and which lines
// start, close or fail a step or end the run. It is a TOML file a person (or
// an agent) writes once per app. Patterns are Go RE2, so a pattern cannot
// backtrack without bound however a log line is shaped.
type Lens struct {
	Name, About string

	format string
	keys   LogKeys // json: the keys holding the event, step and time
	// json: the keys a line may carry its own action and exit in, for a
	// translator (or an app) that writes the event vocabulary itself.
	actionKey, exitKey string
	pattern            *regexp.Regexp // text: splits a line; nil reads the line whole
	layout             string         // a Go time layout; "" reads RFC 3339 or epoch seconds

	steps []StepDef
	rules []lensRule
}

// lensRule is one [[rule]]: when match matches the event name (or field,
// when set), the event takes action. Its named groups fill the event: step
// sets the step, exit the run's exit, any other a field.
//
// say, when set, rewrites the event's name into plain words: {name} takes a
// named group of match, or a field of the event, and the line as read is kept
// in the @line field.
type lensRule struct {
	action Action
	field  string
	match  *regexp.Regexp
	step   string
	say    string
	exit   *int // end: the exit to record when match has no exit group
}

// Lens formats.
const (
	LensJSON = "json"
	LensText = "text"
)

// ActionIgnore drops a line a lens rule matches, as noise. It never reaches
// the fold.
const ActionIgnore Action = "ignore"

// ActionNote marks a line a rule only rewrites (say): it reaches the timeline
// in plain words and changes no step.
const ActionNote Action = "note"

// Fields a lens adds to the events it reads. A line's own key that starts
// with "@" is dropped, so a log cannot set its own action.
const (
	LensActionField = "@action"
	LensRuleField   = "@rule"
	// LensLineField keeps an event's name as read when a rule's say
	// rewrote it.
	LensLineField = "@line"
	lensExitField = "@exit"
)

// Lens caps: a lens is small and hand-written.
const (
	maxLensBytes = 64 << 10
	maxLensRules = 200
	maxLensSteps = 200
	maxSayRefs   = 32 // {name}s in one say: a line's words, not a template engine
)

// lensFile is the TOML shape. Every key is listed, so an unknown one (a
// typo) is an error rather than a rule that silently never fires.
type lensFile struct {
	About      string `toml:"about"`
	Format     string `toml:"format"`
	TimeLayout string `toml:"time_layout"`
	JSON       *struct {
		Event  string `toml:"event"`
		Step   string `toml:"step"`
		Time   string `toml:"time"`
		Action string `toml:"action"`
		Exit   string `toml:"exit"`
	} `toml:"json"`
	Text *struct {
		Pattern string `toml:"pattern"`
	} `toml:"text"`
	Steps []struct {
		ID    string   `toml:"id"`
		Note  string   `toml:"note"`
		After []string `toml:"after"`
	} `toml:"step"`
	Rules []struct {
		Action string `toml:"action"`
		Match  string `toml:"match"`
		Field  string `toml:"field"`
		Step   string `toml:"step"`
		Say    string `toml:"say"`
		Exit   *int   `toml:"exit"`
	} `toml:"rule"`
}

// lensActions are the actions a rule may name.
var lensActions = []Action{ActionStart, ActionClose, ActionFail, ActionSkip, ActionEnd, ActionNote, ActionIgnore}

// sayRef is a {name} in a rule's say.
var sayRef = regexp.MustCompile(`\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// LoadLens reads the lens file at path, named by its stem.
func LoadLens(path string) (*Lens, error) {
	f, err := openLensFile(path)
	if err != nil {
		return nil, cleanErr(err)
	}
	defer f.Close() //nolint:errcheck // read-only
	st, err := f.Stat()
	if err != nil {
		return nil, cleanErr(err)
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("lens %s: %w: not a regular file", clean(path), ErrRefused)
	}
	b, err := io.ReadAll(io.LimitReader(f, maxLensBytes+1))
	if err != nil {
		return nil, cleanErr(err)
	}
	return ParseLens(strings.TrimSuffix(filepath.Base(path), ".toml"), b)
}

// ParseLens reads a lens from its TOML text. name is what the lens is
// called (its file's stem). Every error names the place in the file to fix.
func ParseLens(name string, b []byte) (*Lens, error) {
	if len(b) > maxLensBytes {
		return nil, fmt.Errorf("lens %s: larger than %d KiB", clean(name), maxLensBytes>>10)
	}
	var f lensFile
	md, err := toml.NewDecoder(bytes.NewReader(b)).Decode(&f)
	if err != nil {
		return nil, fmt.Errorf("lens %s: %w", clean(name), cleanErr(err))
	}
	if un := md.Undecoded(); len(un) > 0 {
		keys := make([]string, len(un))
		for i, k := range un {
			keys[i] = k.String()
		}
		return nil, fmt.Errorf("lens %s: unknown key %s; %s", clean(name), clean(strings.Join(keys, ", ")), lensKeysFor(un[0]))
	}
	l := &Lens{Name: clean(name), About: clean(f.About), format: f.Format, layout: f.TimeLayout}
	if err := l.setFormat(&f); err != nil {
		return nil, fmt.Errorf("lens %s: %w", l.Name, err)
	}
	if err := l.setSteps(&f); err != nil {
		return nil, fmt.Errorf("lens %s: %w", l.Name, err)
	}
	if err := l.setRules(&f); err != nil {
		return nil, fmt.Errorf("lens %s: %w", l.Name, err)
	}
	return l, nil
}

func (l *Lens) setFormat(f *lensFile) error {
	switch l.format {
	case LensJSON:
		if f.Text != nil {
			return errors.New("[text] is only read with format = \"text\"")
		}
		l.keys = DefaultLogKeys
		if f.JSON != nil {
			for _, k := range []struct {
				dst *string
				v   string
			}{{&l.keys.Event, f.JSON.Event}, {&l.keys.Step, f.JSON.Step}, {&l.keys.Time, f.JSON.Time}, {&l.actionKey, f.JSON.Action}, {&l.exitKey, f.JSON.Exit}} {
				if k.v != "" {
					*k.dst = k.v
				}
			}
		}
	case LensText:
		if f.JSON != nil {
			return errors.New("[json] is only read with format = \"json\"")
		}
		if f.Text != nil && f.Text.Pattern != "" {
			re, err := regexp.Compile(f.Text.Pattern)
			if err != nil {
				return fmt.Errorf("[text] pattern: %w", cleanErr(err))
			}
			for _, g := range re.SubexpNames() {
				if strings.HasPrefix(g, "@") {
					return fmt.Errorf("[text] pattern: group %q: a name starting with @ is reserved", clean(g))
				}
			}
			l.pattern = re
		}
	case "":
		return errors.New(`format is missing: set format = "json" (one JSON object per line) or format = "text"`)
	default:
		return fmt.Errorf("format %q: want \"json\" or \"text\"", clean(l.format))
	}
	return nil
}

func (l *Lens) setSteps(f *lensFile) error {
	if len(f.Steps) > maxLensSteps {
		return fmt.Errorf("more than %d [[step]] entries", maxLensSteps)
	}
	// Ids are compared as cleaned, the form the events carry them in.
	seen := map[string]bool{}
	for i, s := range f.Steps {
		id := clean(s.ID)
		if id == "" {
			return fmt.Errorf("[[step]] %d: id is missing", i+1)
		}
		if seen[id] {
			return fmt.Errorf("[[step]] %d: id %q is listed twice", i+1, id)
		}
		seen[id] = true
		after := make([]string, len(s.After))
		for k, a := range s.After {
			after[k] = clean(a)
		}
		l.steps = append(l.steps, StepDef{ID: id, Note: clean(s.Note), After: after})
	}
	for i, d := range l.steps {
		for _, a := range d.After {
			if !seen[a] {
				return fmt.Errorf("[[step]] %d (%s): after %q names no [[step]]", i+1, d.ID, a)
			}
		}
	}
	return nil
}

func (l *Lens) setRules(f *lensFile) error {
	if len(f.Rules) > maxLensRules {
		return fmt.Errorf("more than %d [[rule]] entries", maxLensRules)
	}
	for i, r := range f.Rules {
		at := fmt.Sprintf("[[rule]] %d", i+1)
		a := Action(r.Action)
		if !slices.Contains(lensActions, a) {
			return fmt.Errorf("%s: action %q: want start, close, fail, skip, end, note or ignore", at, clean(r.Action))
		}
		if r.Match == "" {
			return fmt.Errorf("%s: match is missing: a regular expression the line's event must match", at)
		}
		re, err := regexp.Compile(r.Match)
		if err != nil {
			return fmt.Errorf("%s: match: %w", at, cleanErr(err))
		}
		names := re.SubexpNames()
		switch {
		case a == ActionNote && r.Say == "":
			return fmt.Errorf("%s: a note rule needs say = \"…\": the plain words to show for the line", at)
		case a == ActionIgnore && r.Say != "":
			return fmt.Errorf("%s: an ignore rule drops its line, so its say is never shown", at)
		case strings.Count(r.Say, "{") != len(sayRef.FindAllString(r.Say, -1)):
			return fmt.Errorf("%s: say: a { must open a {name}: a group of match, or a field of the line", at)
		case len(sayRef.FindAllString(r.Say, -1)) > maxSayRefs:
			return fmt.Errorf("%s: say: more than %d {name}s", at, maxSayRefs)
		case r.Exit != nil && a != ActionEnd:
			return fmt.Errorf("%s: exit is only read on an end rule", at)
		case r.Exit != nil && (*r.Exit < 0 || *r.Exit > 255):
			return fmt.Errorf("%s: exit %d: want 0 to 255", at, *r.Exit)
		}
		stepped := r.Step != "" || slices.Contains(names, "step")
		switch a {
		case ActionStart, ActionClose, ActionFail, ActionSkip:
			if !stepped && !l.hasStepSource() {
				return fmt.Errorf("%s: a %s rule needs a step: set step = \"…\", capture (?P<step>…), or give the line format a step", at, a)
			}
		}
		if len(l.steps) > 0 && r.Step != "" && !slices.ContainsFunc(l.steps, func(d StepDef) bool { return d.ID == r.Step }) {
			return fmt.Errorf("%s: step %q names no [[step]]", at, clean(r.Step))
		}
		l.rules = append(l.rules, lensRule{action: a, field: r.Field, match: re, step: clean(r.Step), say: r.Say, exit: r.Exit})
	}
	return nil
}

// lensKeysFor lists the keys allowed where an unknown key k was found, so
// the error names the fix in that table rather than at the top level.
func lensKeysFor(k toml.Key) string {
	table := ""
	if len(k) > 1 {
		table = k[0]
	}
	switch table {
	case "json":
		return "the keys in [json] are event, step, time, action and exit"
	case "text":
		return "the only key in [text] is pattern (time_layout goes at the top, before any table)"
	case "step":
		return "the keys in [[step]] are id, note and after"
	case "rule":
		return "the keys in [[rule]] are action, match, field, step, say and exit"
	}
	return "the top-level keys are about, format and time_layout, then the tables [json], [text], [[step]] and [[rule]]"
}

// hasStepSource reports whether the line format itself can carry a step: a
// JSON step key, or a step group in the text pattern.
func (l *Lens) hasStepSource() bool {
	if l.format == LensJSON {
		return l.keys.Step != ""
	}
	return l.pattern != nil && slices.Contains(l.pattern.SubexpNames(), "step")
}

// EventsLensName is the built-in lens for the event vocabulary: one JSON
// object per line with event, step, time, action and exit keys. A translator
// written in any language turns an app's log into these lines, and the run
// view draws them with no lens of its own (ADR-0014).
const EventsLensName = "events"

// EventsLens is the built-in lens EventsLensName names.
func EventsLens() *Lens {
	l, err := ParseLens(EventsLensName, []byte(`
about  = "the event vocabulary: event, step, time, action (start, close, fail, skip, end) and exit"
format = "json"
[json]
action = "action"
exit   = "exit"
`))
	if err != nil {
		panic("runview: the built-in events lens does not parse: " + err.Error())
	}
	return l
}

// Spec is the fold a lens's runs take: the action and exit its rules wrote,
// and the steps it declares, or else the steps as the log names them.
func (l *Lens) Spec() *Spec {
	return &Spec{ActionField: LensActionField, ExitField: lensExitField, ExitOptional: true, Discover: len(l.steps) == 0}
}

// Steps is the steps the lens declares; none means they are discovered.
func (l *Lens) Steps() []StepDef { return slices.Clone(l.steps) }

// Ends reports whether a rule can end the run. Without one nothing in the
// log says the run is over, so its state stays unknown.
func (l *Lens) Ends() bool {
	return l.actionKey != "" || slices.ContainsFunc(l.rules, func(r lensRule) bool { return r.action == ActionEnd })
}

// Format is how the lens splits a line: LensJSON or LensText.
func (l *Lens) Format() string { return l.format }

// Splits reports whether the lens has a line format to split lines by:
// always for JSON, and for text only with a pattern.
func (l *Lens) Splits() bool { return l.format == LensJSON || l.pattern != nil }

// Rules is how many rules the lens has.
func (l *Lens) Rules() int { return len(l.rules) }

// RuleText describes rule i (from 1) for a coverage report.
func (l *Lens) RuleText(i int) string {
	if i < 1 || i > len(l.rules) {
		return ""
	}
	r := l.rules[i-1]
	pat := r.match.String()
	quoted := "'" + pat + "'" // as a TOML literal string, the way it was written
	if strings.ContainsAny(pat, "'\n") {
		quoted = strconv.Quote(pat)
	}
	s := fmt.Sprintf("%-6s %s", r.action, quoted)
	if r.field != "" {
		s += " on " + r.field
	}
	if r.step != "" {
		s += " step=" + r.step
	}
	if r.say != "" {
		s += " say=" + strconv.Quote(r.say)
	}
	return clean(s)
}

// lineResult is what a lens made of one line.
type lineResult int

const (
	lineEvent   lineResult = iota // an event
	lineDropped                   // unreadable: not JSON, or no event name
	lineIgnored                   // an ignore rule matched
)

// read turns one log line into an event. droppedFields counts JSON values
// left out (as for a plain log). The event's fields carry @action and @rule
// when a rule matched.
//
// split reports that the line format read the line: it parsed as JSON, or
// the text pattern matched it. A text lens with no pattern splits nothing.
func (l *Lens) read(seq int, b []byte) (e Event, droppedFields int, res lineResult, rule int, split bool) {
	switch l.format {
	case LensJSON:
		raw, nf, ok := scalarFields(b)
		if !ok {
			return Event{}, 0, lineDropped, 0, false
		}
		split = true
		raw = slices.DeleteFunc(raw, func(f Field) bool {
			if strings.HasPrefix(f.Key, "@") {
				nf++
				return true
			}
			return false
		})
		var named bool
		e, named = logEvent(LogKeys{Event: l.keys.Event, Step: l.keys.Step, Time: ""}, seq, raw)
		if !named {
			return Event{}, nf, lineDropped, 0, split
		}
		if l.keys.Time != "" {
			if v, ok := e.field(l.keys.Time); ok {
				e.Time = l.parseTime(v)
			}
		}
		droppedFields = nf
	default:
		e, split = l.readText(seq, string(bytes.TrimRight(b, "\r")))
		if e.Name == "" {
			return Event{}, 0, lineDropped, 0, split
		}
	}
	if a, ok := l.ownAction(e); ok {
		if a == ActionIgnore {
			return Event{}, droppedFields, lineIgnored, 0, split
		}
		if v, ok := e.field(l.exitKey); ok && l.exitKey != "" {
			e.Fields = append(e.Fields, Field{Key: lensExitField, Value: v})
		}
		e.Fields = append(e.Fields, Field{Key: LensActionField, Value: string(a)})
		return e, droppedFields, lineEvent, 0, split
	}
	e, res, rule = l.classify(e)
	return e, droppedFields, res, rule, split
}

// ownAction is the action a JSON line names under the lens's action key,
// when it names one the vocabulary has. It decides before the rules: the
// line's writer knew what it meant.
func (l *Lens) ownAction(e Event) (Action, bool) {
	if l.actionKey == "" {
		return "", false
	}
	v, ok := e.field(l.actionKey)
	if !ok || !slices.Contains(lensActions, Action(v)) {
		return "", false
	}
	return Action(v), true
}

// readText splits a text line by the pattern. A line the pattern does not
// match (a stack trace's continuation, say) is an event named by the whole
// line, with no step or time.
func (l *Lens) readText(seq int, line string) (Event, bool) {
	e := Event{Seq: seq}
	if l.pattern == nil {
		e.Name = clean(line)
		return e, false
	}
	m := l.pattern.FindStringSubmatch(line)
	if m == nil {
		e.Name = clean(line)
		return e, false
	}
	named := false
	for i, g := range l.pattern.SubexpNames() {
		if i == 0 || g == "" {
			continue
		}
		v := clean(m[i])
		switch g {
		case "event":
			e.Name, named = v, true
		case "step":
			e.Step = v
		case "time":
			e.Time = l.parseTime(v)
		default:
			if len(e.Fields) < maxEventFields {
				e.Fields = append(e.Fields, Field{Key: clean(g), Value: v})
			}
		}
	}
	if !named {
		e.Name = clean(line)
	}
	return e, true
}

// classify runs the rules over an event, first match wins, and writes what
// the matching rule says into it. rule is the rule that matched, from 1, or
// 0 for none.
func (l *Lens) classify(e Event) (_ Event, _ lineResult, rule int) {
	for i, r := range l.rules {
		target := e.Name
		if r.field != "" {
			v, ok := e.field(r.field)
			if !ok {
				continue
			}
			target = v
		}
		m := r.match.FindStringSubmatch(target)
		if m == nil {
			continue
		}
		if r.action == ActionIgnore {
			return Event{}, lineIgnored, i + 1
		}
		if r.step != "" {
			e.Step = r.step
		}
		for k, g := range r.match.SubexpNames() {
			if k == 0 || g == "" {
				continue
			}
			v := clean(m[k])
			switch g {
			case "step":
				if r.step == "" {
					e.Step = v
				}
			case "exit":
				e.Fields = append(e.Fields, Field{Key: lensExitField, Value: v})
			default:
				if len(e.Fields) < maxEventFields {
					e.Fields = append(e.Fields, Field{Key: clean(g), Value: v})
				}
			}
		}
		if r.exit != nil && !slices.Contains(r.match.SubexpNames(), "exit") {
			e.Fields = append(e.Fields, Field{Key: lensExitField, Value: strconv.Itoa(*r.exit)})
		}
		if r.say != "" {
			e.Fields = append(e.Fields, Field{Key: LensLineField, Value: e.Name})
			e.Name = clean(r.render(m, e))
		}
		e.Fields = append(e.Fields,
			Field{Key: LensActionField, Value: string(r.action)},
			Field{Key: LensRuleField, Value: strconv.Itoa(i + 1)})
		return e, lineEvent, i + 1
	}
	return e, lineEvent, 0
}

// render fills the rule's say: {name} takes match's group of that name, else
// the event's step for {step}, else its field; a name that is none of them is
// left as written, so a lens check shows the mistake.
func (r lensRule) render(m []string, e Event) string {
	return sayRef.ReplaceAllStringFunc(r.say, func(ref string) string {
		name := ref[1 : len(ref)-1]
		if i := r.match.SubexpIndex(name); i > 0 {
			return m[i]
		}
		if name == "step" && e.Step != "" {
			return e.Step
		}
		if name == "event" {
			return e.Name
		}
		if v, ok := e.field(name); ok {
			return v
		}
		return ref
	})
}

// parseTime reads a time with the lens's layout, or else as a plain log
// does: RFC 3339, or epoch seconds. A layout with no zone reads local time.
func (l *Lens) parseTime(v string) time.Time {
	if l.layout == "" {
		return parseTime(v)
	}
	t, err := time.ParseInLocation(l.layout, v, time.Local)
	if err != nil || t.Year() > 9999 {
		return time.Time{}
	}
	return t
}
