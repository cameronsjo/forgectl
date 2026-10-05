// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package runview

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/cameronsjo/forgectl/internal/termsafe"
	"github.com/cameronsjo/forgectl/internal/tomlerr"
)

// ErrSpec marks a run spec that cannot be used. Its message names the file
// and the TOML key: `run spec "<file>": <key>: <problem>`.
var ErrSpec = errors.New("run spec")

// Action is what an event does to a run. The [on] table maps event names to
// actions; checks are mapped by [[checks]], not by [on].
type Action string

const (
	ActionStart   Action = "start"
	ActionClose   Action = "close"
	ActionReject  Action = "reject"
	ActionFail    Action = "fail"
	ActionRestart Action = "restart"
	ActionEnd     Action = "end"
)

var actions = map[Action]bool{
	ActionStart: true, ActionClose: true, ActionReject: true,
	ActionFail: true, ActionRestart: true, ActionEnd: true,
}

// Spec describes where a family of runs lives and how to read their logs.
// The format is experimental and carries no stability promise. Every field is
// exported so a source can build a Spec without a file.
type Spec struct {
	Path      string // the file it was loaded from; empty when built in code
	Name      string
	RootEnv   string // names the variable holding the absolute root
	Runs      string // relative pattern with one '*'; each match is a run dir
	Summaries string // relative pattern with one '*'; the match is the run name

	Log     LogSpec
	Steps   []StepDef
	Gate    GateSpec
	On      map[string]Action // event name -> action
	End     EndSpec
	Checks  []CheckDef
	Exits   map[int]string // exit code -> meaning
	Metrics MetricsSpec
	Live    LiveSpec
}

// LogSpec names the events file (relative to the run dir) and the JSON keys
// that carry each event's time, name, step and attempt. Show lists the
// fields an event row displays.
type LogSpec struct {
	Path, Time, Event, Step, Attempt string
	Show                             []string
}

// GateSpec is the reject loop every step passes through. MaxRejects is 0
// when the spec has no [gate]; it is only drawn, never enforced.
type GateSpec struct {
	Label      string
	MaxRejects int
}

// EndSpec names the end event's field that holds the exit code.
type EndSpec struct{ ExitField string }

// CheckDef is a check fed by one event: it passes when Field is 0.
// FailExit is the exit the run takes when the check fails, for display.
type CheckDef struct {
	ID, Event, Field, Label string
	FailExit                int
}

// MetricsSpec names the metrics file, the fields summed across its lines,
// and the one field/value pair that marks a line as an error.
type MetricsSpec struct {
	Path      string
	Sum       []string
	ErrorWhen *Field
}

// LiveSpec bounds how long a run may go without an event and still count as
// live. Zero means a run is never stale.
type LiveSpec struct{ StaleAfter time.Duration }

// rawSpec is the TOML shape. Exits keys and durations arrive as strings and
// are parsed in validate.
type rawSpec struct {
	Name      string `toml:"name"`
	RootEnv   string `toml:"root_env"`
	Runs      string `toml:"runs"`
	Summaries string `toml:"summaries"`
	Log       struct {
		Path    string   `toml:"path"`
		Time    string   `toml:"time"`
		Event   string   `toml:"event"`
		Step    string   `toml:"step"`
		Attempt string   `toml:"attempt"`
		Show    []string `toml:"show"`
	} `toml:"log"`
	Steps []struct {
		ID    string   `toml:"id"`
		Note  string   `toml:"note"`
		After []string `toml:"after"`
	} `toml:"steps"`
	Gate struct {
		Label      string `toml:"label"`
		MaxRejects int    `toml:"max_rejects"`
	} `toml:"gate"`
	On  map[string]string `toml:"on"`
	End struct {
		ExitField string `toml:"exit_field"`
	} `toml:"end"`
	Checks []struct {
		ID       string `toml:"id"`
		Event    string `toml:"event"`
		Field    string `toml:"field"`
		Label    string `toml:"label"`
		FailExit int    `toml:"fail_exit"`
	} `toml:"checks"`
	Exits   map[string]string `toml:"exits"`
	Metrics struct {
		Path      string            `toml:"path"`
		Sum       []string          `toml:"sum"`
		ErrorWhen map[string]string `toml:"error_when"`
	} `toml:"metrics"`
	Live struct {
		StaleAfter string `toml:"stale_after"`
	} `toml:"live"`
}

// LoadSpec reads and validates a run spec. The spec is trusted operator
// config, but its patterns name paths under an untrusted root, so each one
// must stay relative and below it. Every error names path and the TOML key.
func LoadSpec(path string) (*Spec, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: the operator names their own spec file; it is trusted config
	if err != nil {
		// The message names path once: a PathError would name it again.
		var pe *fs.PathError
		if errors.As(err, &pe) {
			err = pe.Err
		}
		return nil, fmt.Errorf("%w %s: %w", ErrSpec, termsafe.QuotePath(path), termsafe.Error(err))
	}
	var raw rawSpec
	meta, err := toml.Decode(string(data), &raw)
	if err != nil {
		return nil, fmt.Errorf("%w %s: %w", ErrSpec, termsafe.QuotePath(path), tomlerr.Scrub(err))
	}
	if keys := meta.Undecoded(); len(keys) > 0 {
		return nil, fmt.Errorf("%w %s: unknown key(s) %s", ErrSpec, termsafe.QuotePath(path), tomlerr.Keys(keys))
	}
	spec, key, problem := raw.validate(meta.IsDefined("gate"))
	if problem != "" {
		return nil, fmt.Errorf("%w %s: %s: %s", ErrSpec, termsafe.QuotePath(path), key, problem)
	}
	spec.Path = path
	return spec, nil
}

// validate converts raw into a Spec, or returns the offending key and what is
// wrong with it.
func (raw *rawSpec) validate(hasGate bool) (spec *Spec, key, problem string) {
	for _, req := range []struct{ key, value string }{
		{"name", raw.Name}, {"root_env", raw.RootEnv}, {"runs", raw.Runs},
		{"log.path", raw.Log.Path}, {"log.event", raw.Log.Event},
	} {
		if req.value == "" {
			return nil, req.key, "required"
		}
	}
	// runs and summaries match one '*'; the log and metrics paths are
	// opened as written, so a '*' in them would never match a file.
	for _, p := range []struct {
		key, value string
		stars      int
	}{
		{"runs", raw.Runs, 1}, {"summaries", raw.Summaries, 1},
		{"log.path", raw.Log.Path, 0}, {"metrics.path", raw.Metrics.Path, 0},
	} {
		if p.value == "" {
			continue
		}
		if problem := checkPattern(p.value, p.stars); problem != "" {
			return nil, p.key, problem
		}
	}

	spec = &Spec{
		Name: raw.Name, RootEnv: raw.RootEnv, Runs: raw.Runs, Summaries: raw.Summaries,
		Log: LogSpec{
			Path: raw.Log.Path, Time: raw.Log.Time, Event: raw.Log.Event,
			Step: raw.Log.Step, Attempt: raw.Log.Attempt, Show: raw.Log.Show,
		},
		Gate:    GateSpec{Label: raw.Gate.Label, MaxRejects: raw.Gate.MaxRejects},
		End:     EndSpec{ExitField: raw.End.ExitField},
		Metrics: MetricsSpec{Path: raw.Metrics.Path, Sum: raw.Metrics.Sum},
	}

	if len(raw.Steps) == 0 {
		return nil, "steps", "at least one step is required"
	}
	declared := make(map[string]bool, len(raw.Steps))
	for i, s := range raw.Steps {
		at := "steps[" + strconv.Itoa(i) + "]"
		if s.ID == "" {
			return nil, at + ".id", "required"
		}
		if declared[s.ID] {
			return nil, at + ".id", "duplicate step id " + termsafe.QuoteArgMax(s.ID, 0)
		}
		// Naming only earlier steps keeps the edges acyclic by construction.
		for _, a := range s.After {
			if !declared[a] {
				return nil, at + ".after", "names " + termsafe.QuoteArgMax(a, 0) + ", which is not a step declared before this one"
			}
		}
		declared[s.ID] = true
		spec.Steps = append(spec.Steps, StepDef{ID: s.ID, Note: s.Note, After: s.After})
	}

	if hasGate && raw.Gate.MaxRejects < 1 {
		return nil, "gate.max_rejects", "must be at least 1"
	}

	spec.On = make(map[string]Action, len(raw.On))
	for event, name := range raw.On {
		a := Action(name)
		if !actions[a] {
			return nil, "on." + keyPart(event), "unknown action " + termsafe.QuoteArgMax(name, 0) +
				" (want start, close, reject, fail, restart or end)"
		}
		spec.On[event] = a
		if a == ActionEnd && raw.End.ExitField == "" {
			return nil, "end.exit_field", "required when an event maps to end"
		}
	}

	ids := map[string]bool{}
	for i, c := range raw.Checks {
		at := "checks[" + strconv.Itoa(i) + "]"
		switch {
		case c.ID == "":
			return nil, at + ".id", "required"
		case ids[c.ID]:
			return nil, at + ".id", "duplicate check id " + termsafe.QuoteArgMax(c.ID, 0)
		case c.Event == "":
			return nil, at + ".event", "required"
		case spec.On[c.Event] != "":
			return nil, at + ".event", "already mapped in [on]"
		case c.Field == "":
			return nil, at + ".field", "required"
		case c.FailExit < 0 || c.FailExit > 255:
			return nil, at + ".fail_exit", "must be an integer 0–255"
		}
		ids[c.ID] = true
		spec.Checks = append(spec.Checks, CheckDef{ID: c.ID, Event: c.Event, Field: c.Field, Label: c.Label, FailExit: c.FailExit})
	}

	if len(raw.Exits) > 0 {
		spec.Exits = make(map[int]string, len(raw.Exits))
	}
	for k, meaning := range raw.Exits {
		code, ok := parseExit(k)
		if !ok {
			return nil, "exits." + keyPart(k), "key must be an integer 0–255"
		}
		spec.Exits[code] = meaning
	}

	if raw.Metrics.ErrorWhen != nil {
		if len(raw.Metrics.ErrorWhen) != 1 {
			return nil, "metrics.error_when", "must have exactly one key"
		}
		for k, v := range raw.Metrics.ErrorWhen {
			spec.Metrics.ErrorWhen = &Field{Key: k, Value: v}
		}
	}

	if raw.Live.StaleAfter != "" {
		d, err := time.ParseDuration(raw.Live.StaleAfter)
		if err != nil || d <= 0 {
			return nil, "live.stale_after", "must be a positive duration such as \"10m\""
		}
		spec.Live.StaleAfter = d
	}
	return spec, "", ""
}

// checkPattern vets a path pattern the sources will resolve under the run
// root. Both separators count, so a pattern means the same on every OS, and
// ':' is refused anywhere, since a drive letter or a stream name would
// reach past the root. stars is the exact number of '*' required.
func checkPattern(p string, stars int) string {
	switch {
	case strings.HasPrefix(p, "/") || strings.HasPrefix(p, `\`) || driveRe.MatchString(p):
		return "must be a relative path"
	case strings.Contains(p, ":"):
		return "must not contain ':'"
	case strings.ContainsAny(p, "?["):
		return "only '*' is allowed as a wildcard"
	}
	for _, part := range strings.FieldsFunc(p, func(r rune) bool { return r == '/' || r == '\\' }) {
		if part == ".." {
			return "must not contain '..'"
		}
	}
	switch n := strings.Count(p, "*"); {
	case stars == 0 && n > 0:
		return "is opened as written and must not contain '*'"
	case stars > 0 && n != stars:
		return "must contain exactly one '*'"
	}
	return ""
}

// driveRe is a pattern that starts with a drive letter, such as C:.
var driveRe = regexp.MustCompile(`^[A-Za-z]:`)

// parseExit parses an [exits] key. Only the canonical decimal form counts,
// so "03" and "+3" are not silently read as 3.
func parseExit(k string) (int, bool) {
	n, err := strconv.Atoi(k)
	if err != nil || n < 0 || n > 255 || strconv.Itoa(n) != k {
		return 0, false
	}
	return n, true
}

var bareKey = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]{0,63}$`)

// keyPart renders a user-chosen TOML key piece for an error: bare when it
// reads as a name, otherwise quoted and capped.
func keyPart(k string) string {
	if bareKey.MatchString(k) {
		return k
	}
	return termsafe.QuoteArgMax(k, 0)
}
