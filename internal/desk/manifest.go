package desk

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Manifest grammar, one step per line (comment and blank lines ignored):
//
//	<step> [after=a,b] [timeout=S] [private] -- <command>
//
// The line splits on the FIRST " -- "; everything after it goes verbatim to
// `/bin/bash -c`, so a later " -- " or "#" is part of the command.
var (
	stepIDRe  = regexp.MustCompile(`^[a-z][a-z0-9]{0,31}$`) // no "_", so OUT_<step>_<key> is unambiguous
	outKeyRe  = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*$`)
	outRefRe  = regexp.MustCompile(`OUT_([a-z][a-z0-9]{0,31})_([A-Za-z][A-Za-z0-9_]*)`)
	timeoutRe = regexp.MustCompile(`^[1-9][0-9]*$`)
)

const (
	manifestSeparator = " -- "
	knownOptions      = "after=, timeout=, private"
)

// ErrManifest marks a manifest that cannot run. Its message names the place:
// `<file>:<line>: step <s>: <problem>`, or `<file>: cycle: a -> b -> a`.
var ErrManifest = errors.New("manifest")

func manifestErr(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrManifest, fmt.Sprintf(format, args...))
}

// Step is one manifest line.
type Step struct {
	ID      string
	Command string
	Line    int
	After   []string
	Timeout int // seconds; 0 means none
	Private bool
}

// OptionsText renders the step's options the way the combined log header
// shows them, or "-" when there are none.
func (s Step) OptionsText() string {
	var parts []string
	if len(s.After) > 0 {
		parts = append(parts, "after="+strings.Join(s.After, ","))
	}
	if s.Timeout > 0 {
		parts = append(parts, "timeout="+strconv.Itoa(s.Timeout))
	}
	if s.Private {
		parts = append(parts, "private")
	}
	if len(parts) == 0 {
		return "-"
	}
	return strings.Join(parts, " ")
}

// Manifest is a parsed, validated batch. SHA256 covers the exact bytes it was
// parsed from, so the hash shown at plan time and the hash checked at run time
// are over the same thing.
type Manifest struct {
	Steps     []Step
	Ancestors map[string]map[string]bool
	SHA256    string
	byID      map[string]int
}

// Step returns the step with id; ok is false when there is none.
func (m *Manifest) Step(id string) (Step, bool) {
	i, ok := m.byID[id]
	if !ok {
		return Step{}, false
	}
	return m.Steps[i], true
}

func (m *Manifest) step(id string) Step {
	return m.Steps[m.byID[id]]
}

// LoadManifest parses data and records its sha256. name is used in errors.
func LoadManifest(data []byte, name string) (*Manifest, error) {
	if !utf8.Valid(data) {
		return nil, manifestErr("%s: not UTF-8", name)
	}
	m, err := ParseManifest(string(data), name)
	if err != nil {
		return nil, err
	}
	m.SHA256 = SHA256Hex(data)
	return m, nil
}

// SHA256Hex is the full lowercase hex sha256 of data. Hashes are always
// compared in full; nothing in this package compares a prefix.
func SHA256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// ParseManifest parses and validates text: ids, options, duplicate ids,
// unknown dependencies, and cycles.
func ParseManifest(text, name string) (*Manifest, error) {
	m := &Manifest{byID: map[string]int{}, Ancestors: map[string]map[string]bool{}}
	for i, raw := range strings.Split(text, "\n") {
		lineno := i + 1
		raw = strings.TrimSuffix(raw, "\r")
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		s, err := parseStepLine(raw, name, lineno)
		if err != nil {
			return nil, err
		}
		if _, dup := m.byID[s.ID]; dup {
			return nil, manifestErr("%s:%d: step %s: duplicate step id", name, lineno, s.ID)
		}
		m.byID[s.ID] = len(m.Steps)
		m.Steps = append(m.Steps, s)
	}
	if len(m.Steps) == 0 {
		return nil, manifestErr("%s: no steps (one per line: '<step> [options] -- <command>')", name)
	}
	known := make([]string, len(m.Steps))
	for i, s := range m.Steps {
		known[i] = s.ID
	}
	for _, s := range m.Steps {
		for _, d := range s.After {
			if _, ok := m.byID[d]; !ok {
				return nil, manifestErr("%s:%d: step %s: unknown dependency %q (known: %s)", name, s.Line, s.ID, d, strings.Join(known, ", "))
			}
		}
	}
	if cycle := findCycle(m); cycle != nil {
		return nil, manifestErr("%s: cycle: %s", name, strings.Join(cycle, " -> "))
	}
	for _, s := range m.Steps { // acyclic, so this terminates
		anc := map[string]bool{}
		todo := slices.Clone(s.After)
		for len(todo) > 0 {
			d := todo[len(todo)-1]
			todo = todo[:len(todo)-1]
			if !anc[d] {
				anc[d] = true
				todo = append(todo, m.step(d).After...)
			}
		}
		m.Ancestors[s.ID] = anc
	}
	return m, nil
}

func parseStepLine(text, name string, lineno int) (Step, error) {
	where := fmt.Sprintf("%s:%d", name, lineno)
	head, command, found := strings.Cut(text, manifestSeparator)
	if !found {
		first := "?"
		if f := strings.Fields(text); len(f) > 0 {
			first = f[0]
		}
		return Step{}, manifestErr("%s: step %s: missing ' -- ' between the step and its command", where, describe(first))
	}
	tokens := strings.Fields(head)
	if len(tokens) == 0 {
		return Step{}, manifestErr("%s: missing step id before ' -- '", where)
	}
	id, opts := tokens[0], tokens[1:]
	if !stepIDRe.MatchString(id) {
		return Step{}, manifestErr("%s: step %s: bad step id (ids match %s)", where, describe(id), stepIDRe)
	}
	if strings.TrimSpace(command) == "" {
		return Step{}, manifestErr("%s: step %s: empty command", where, id)
	}
	s := Step{ID: id, Command: command, Line: lineno}
	for _, opt := range opts {
		switch {
		case strings.HasPrefix(opt, "after="):
			s.After = nil
			for _, a := range strings.Split(strings.TrimPrefix(opt, "after="), ",") {
				if a != "" {
					s.After = append(s.After, a)
				}
			}
		case strings.HasPrefix(opt, "timeout="):
			v := strings.TrimPrefix(opt, "timeout=")
			n, err := strconv.Atoi(v)
			if !timeoutRe.MatchString(v) || err != nil {
				return Step{}, manifestErr("%s: step %s: bad timeout %q (whole seconds, at least 1)", where, id, v)
			}
			s.Timeout = n
		case opt == "private":
			s.Private = true
		case opt == "tty":
			return Step{}, manifestErr("%s: step %s: tty steps are not supported (steps run without a terminal); queue an interactive command as its own .sh item with '# TTY: yes'", where, id)
		default:
			return Step{}, manifestErr("%s: step %s: unknown option %q (known: %s)", where, id, opt, knownOptions)
		}
	}
	return s, nil
}

// findCycle returns a dependency cycle as a path that starts and ends on the
// same id, or nil.
func findCycle(m *Manifest) []string {
	const onPath, done = 1, 2
	state := map[string]int{}
	var path []string
	var visit func(n string) []string
	visit = func(n string) []string {
		state[n] = onPath
		path = append(path, n)
		for _, d := range m.step(n).After {
			if state[d] == onPath {
				start := slices.Index(path, d)
				return append(slices.Clone(path[start:]), d)
			}
			if state[d] == 0 {
				if found := visit(d); found != nil {
					return found
				}
			}
		}
		path = path[:len(path)-1]
		state[n] = done
		return nil
	}
	for _, s := range m.Steps {
		if state[s.ID] == 0 {
			if found := visit(s.ID); found != nil {
				return found
			}
		}
	}
	return nil
}

// Waves groups step ids by depth, each wave in manifest order.
func (m *Manifest) Waves() [][]string {
	level := map[string]int{}
	var depth func(id string) int
	depth = func(id string) int {
		if l, ok := level[id]; ok {
			return l
		}
		l := 0
		for _, d := range m.step(id).After {
			l = max(l, depth(d)+1)
		}
		level[id] = l
		return l
	}
	top := 0
	for _, s := range m.Steps {
		top = max(top, depth(s.ID))
	}
	out := make([][]string, top+1)
	for _, s := range m.Steps {
		out[level[s.ID]] = append(out[level[s.ID]], s.ID)
	}
	return out
}

// OrderLine renders waves as "a,b -> c -> d".
func OrderLine(waves [][]string) string {
	parts := make([]string, len(waves))
	for i, w := range waves {
		parts[i] = strings.Join(w, ",")
	}
	return strings.Join(parts, " -> ")
}

// Lint returns the plan's warnings: a command naming OUT_<x>_<k> where x is
// not a step, or not an ancestor (so the variable will be unset).
func (m *Manifest) Lint() []string {
	var out []string
	for _, s := range m.Steps {
		for _, ref := range outRefRe.FindAllStringSubmatch(s.Command, -1) {
			step, key := ref[1], ref[2]
			var msg string
			switch {
			case !m.has(step):
				msg = fmt.Sprintf("%s uses OUT_%s_%s but no step %s exists", s.ID, step, key, step)
			case !m.Ancestors[s.ID][step]:
				msg = fmt.Sprintf("%s uses OUT_%s_%s but %s is not an ancestor", s.ID, step, key, step)
			default:
				continue
			}
			if !slices.Contains(out, msg) {
				out = append(out, msg)
			}
		}
	}
	return out
}

func (m *Manifest) has(id string) bool {
	_, ok := m.byID[id]
	return ok
}

// nearestFailed walks dependencies breadth-first so the closest failed
// ancestor is named.
func (m *Manifest) nearestFailed(id string, failed map[string]bool) string {
	frontier := slices.Clone(m.step(id).After)
	seen := map[string]bool{}
	for len(frontier) > 0 {
		var next []string
		for _, d := range frontier {
			if failed[d] {
				return d
			}
			if !seen[d] {
				seen[d] = true
				next = append(next, m.step(d).After...)
			}
		}
		frontier = next
	}
	names := make([]string, 0, len(failed))
	for k := range failed {
		names = append(names, k)
	}
	slices.Sort(names)
	if len(names) == 0 {
		return ""
	}
	return names[0]
}
