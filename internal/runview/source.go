// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package runview

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"time"

	"github.com/cameronsjo/forgectl/internal/desk"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// RunKind says what a run is read from.
type RunKind string

const (
	KindDesk RunKind = "desk" // a desk item
	KindLog  RunKind = "log"  // a JSONL log a person named
)

// RunRef names one run of a source. Updated is the newest modification time
// the source saw for it. Live is a desk run's state as the listing scan saw
// it (empty for a log), so a caller can tell that a run it does not reload
// changed state, as when its supervisor died.
type RunRef struct {
	Source, Name string
	Kind         RunKind
	Updated      time.Time
	Live         LiveState
}

// Cursor is where a Load stopped in one run, so the next Load reads only what
// was added. The caller keeps one Cursor per run and passes the same pointer
// to every Load; a zero Cursor reads the run from the start. Every byte the
// next Load needs (a held partial line, the counts behind Seq and the caps)
// lives here, not in the Source.
//
// Offset, Dev and Ino are a log's position and identity. For a desk run,
// Offset is the number of event lines the desk's watcher has seen.
type Cursor struct {
	Offset   int64
	Dev, Ino uint64

	opened   bool   // Dev and Ino were recorded, so a change is a replacement
	held     []byte // the start of a line whose newline has not arrived
	dropping bool   // the held line passed maxLineBytes; drop it at its newline
	lines    int    // complete lines read, the next Seq
	kept     int    // events delivered, for maxRunEvents
	dropped  int    // lines dropped: too long, not JSON, unnamed, or past maxRunEvents
	ignored  int    // lines a lens's ignore rule matched
	ruleHits []int  // lines each lens rule matched
	// lensLines and split count the non-blank lines a lens read and the ones
	// its line format split.
	lensLines, split int
	fields           int  // field values dropped: not a string, an int64 or a boolean
	lost             bool // desk only: RUN-LOST was delivered
	desk             any  // desk only: the source's watcher for this run
}

// Delta is what one Load found.
//
// Events are new since the cursor's last Load; the caller appends them. When
// Reset is set, the log was replaced or shrank since the last Load: the
// caller discards every Event it holds for the run, and this Delta carries
// the run again from its first line.
//
// Defs and Timing are complete each time they are set. Dropped and
// DroppedFields are totals for the run since the last Reset, not increments.
// Partial reports a log past the 32 MiB cap; the part past it is not read.
// Err reports something that could not be read while the rest of the run
// still loaded.
//
// Every string in a Delta has been through termsafe at ingest.
type Delta struct {
	Events        []Event
	Defs          []StepDef
	Dropped       int
	DroppedFields int
	// Ignored counts lines a lens's ignore rule matched: noise left out on
	// purpose, not a fault. A total, like Dropped.
	Ignored int
	// RuleHits counts, for a lens's runs, the lines each rule matched,
	// ignored lines included: RuleHits[i] is rule i+1's. A total, like
	// Dropped. It is how a person (or an agent) sees which rules work.
	RuleHits []int
	// Lines and Split are, for a lens's runs, the non-blank lines read and
	// the ones the lens's format split (parsed as JSON, or matched by the
	// text pattern). Split far below Lines means the pattern is wrong.
	Lines, Split int
	Partial      bool
	Reset        bool
	// Held is set when the log ends in a line with no newline yet. It is
	// held back until its newline arrives; a log that is finished without
	// one never shows that line.
	Held bool
	Err  error

	// Live is the run's state as its source sees it: the desk's own word for
	// a desk run, LiveUnknown for a log.
	Live LiveState
	// Timing is a desk batch's per-step state and times from its status
	// file, or a script's one step from its item. Desk events carry no time
	// of their own, so this is where a desk run's durations come from.
	Timing []StepTiming
}

// StepTiming is one desk step's state and times. State is the runner's word
// (pending, running, ok, failed, cancelled, skipped). Dur is End minus Start,
// or the STEP-END dur= value when the status file has no end yet.
type StepTiming struct {
	Step, State string
	RC          *int
	Start, End  time.Time
	Dur         time.Duration
}

// Source lists runs and loads them incrementally.
type Source interface {
	Name() string
	List() ([]RunRef, error)
	Load(ref RunRef, cur *Cursor) (Delta, error)
}

// SpecOf is the spec ref's events fold with: the source's own when it has
// one (a lens), the desk's for a desk run, and none otherwise.
func SpecOf(src Source, ref RunRef) *Spec {
	if s, ok := src.(interface{ Spec(RunRef) *Spec }); ok {
		return s.Spec(ref)
	}
	if ref.Kind == KindDesk {
		return DeskSpec()
	}
	return &Spec{}
}

// ErrRefused marks a path that is a symlink or not a regular file.
var ErrRefused = errors.New("refused")

// Ingest caps.
const (
	maxFileBytes   = 32 << 20 // per log, from offset 0; past it the run is Partial
	maxLineBytes   = 64 << 10 // a longer line is dropped and counted
	maxRunEvents   = 50000    // per run; the rest are counted
	maxFieldRunes  = 256      // every ingested string, after termsafe
	maxEventFields = 256      // fields kept from one line; the rest are dropped and counted
	// maxEpochSeconds is 9999-12-31T23:59:59Z, the last time JSON can encode.
	maxEpochSeconds = 253402300799
)

// clean makes one ingested string inert and caps it.
func clean(s string) string { return termsafe.SafeLineMax(s, maxFieldRunes) }

// cleanError is an error whose text came from elsewhere, made inert. Is and
// As still see the error it wraps.
type cleanError struct{ err error }

func (e cleanError) Error() string { return clean(e.err.Error()) }
func (e cleanError) Unwrap() error { return e.err }

func cleanErr(err error) error {
	if err == nil {
		return nil
	}
	return cleanError{err}
}

// DeskSpec is the spec a desk run folds with. A batch's STEP-START starts a
// step, STEP-END closes it, and STEP-FAIL (the desk source's name for a
// STEP-END whose rc is not 0) fails it; RUN-END carries the exit. A script is
// one step, ScriptStep, whose start and end the desk source derives from
// RUN-START and RUN-END. STEP-SKIP skips a step that never started.
// RUN-LOST, which the desk source adds when a run's owner is gone with no
// RUN-END, ends the run as lost: a step still running reads interrupted.
func DeskSpec() *Spec {
	return &Spec{
		On: map[string]Action{
			deskStepStart: ActionStart,
			deskStepEnd:   ActionClose,
			deskStepFail:  ActionFail,
			deskStepSkip:  ActionSkip,
			deskRunEnd:    ActionEnd,
			deskRunLost:   ActionLost,
		},
		ExitField: "rc",
	}
}

// Desk event names as Events carry them. They match the desk's own keys,
// except deskStepFail.
const (
	deskStepStart = desk.EventStepStart
	deskStepEnd   = desk.EventStepEnd
	deskStepFail  = "STEP-FAIL"
	deskStepSkip  = desk.EventStepSkip
	deskRunEnd    = desk.EventRunEnd
	deskRunLost   = desk.EventRunLost
	// ScriptStep is the one step of a desk script.
	ScriptStep = "script"
)

// LogKeys names the JSON keys of a log line that carry its event name, its
// step and its time. Event is required; an unset Step or Time is not read.
type LogKeys struct{ Event, Step, Time string }

// DefaultLogKeys are the keys a log is read with unless the person names
// others.
var DefaultLogKeys = LogKeys{Event: "event", Step: "step", Time: "time"}

// scalarFields decodes one line holding one JSON object into its fields, in
// the order they appear. A string, an integer that fits in int64, or a
// boolean is kept as text; any other value (a float, a larger integer, an
// object or array, null) is left out and counted in dropped. For a repeated
// key the first one decides, whether or not it was kept. ok is false when the
// line is not exactly one JSON object. Keys and values are raw; the caller
// cleans them.
func scalarFields(line []byte) (fields []Field, dropped int, ok bool) {
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.UseNumber()
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil, 0, false
	}
	seen := map[string]bool{}
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return nil, 0, false
		}
		key, _ := t.(string)
		v, err := dec.Token()
		if err != nil {
			return nil, 0, false
		}
		if _, nested := v.(json.Delim); nested {
			if err := skipValue(dec); err != nil {
				return nil, 0, false
			}
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		text, keep := scalar(v)
		if !keep || len(fields) >= maxEventFields {
			dropped++
			continue
		}
		fields = append(fields, Field{Key: key, Value: text})
	}
	if t, err := dec.Token(); err != nil || t != json.Delim('}') {
		return nil, 0, false
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, 0, false // a second value, or trailing garbage
	}
	return fields, dropped, true
}

// scalar returns the text of a kept JSON token: a string as is, an integer in
// int64 range in canonical decimal, a boolean as true or false.
func scalar(v json.Token) (string, bool) {
	switch v := v.(type) {
	case string:
		return v, true
	case bool:
		return strconv.FormatBool(v), true
	case json.Number:
		n, err := strconv.ParseInt(v.String(), 10, 64)
		if err != nil {
			return "", false // a fraction, an exponent, or out of range
		}
		return strconv.FormatInt(n, 10), true
	}
	return "", false
}

// skipValue consumes the rest of an object or array whose opening delimiter
// was just read.
func skipValue(dec *json.Decoder) error {
	depth := 1
	for depth > 0 {
		t, err := dec.Token()
		if err != nil {
			return err
		}
		if d, ok := t.(json.Delim); ok {
			if d == '{' || d == '[' {
				depth++
			} else {
				depth--
			}
		}
	}
	return nil
}

// logEvent builds an Event from one log line's fields under keys. Every key
// and value is cleaned. ok is false when the line has no event name, which
// drops the line.
func logEvent(keys LogKeys, seq int, raw []Field) (Event, bool) {
	e := Event{Seq: seq, Fields: make([]Field, 0, len(raw))}
	named := false
	for _, f := range raw {
		f = Field{Key: clean(f.Key), Value: clean(f.Value)}
		e.Fields = append(e.Fields, f)
		if f.Key == "" {
			continue // an unset key must not match it
		}
		if f.Key == keys.Event && !named {
			e.Name, named = f.Value, true
		}
		if f.Key == keys.Step && e.Step == "" {
			e.Step = f.Value
		}
		if f.Key == keys.Time && e.Time.IsZero() {
			e.Time = parseTime(f.Value)
		}
	}
	return e, named && e.Name != ""
}

// parseTime reads an event time: RFC 3339 text, or an integer count of
// seconds since the epoch up to the end of year 9999. Anything else is the
// zero time: a count past that (epoch milliseconds, say) would be a year
// JSON cannot encode, and would sort the run past every other.
func parseTime(v string) time.Time {
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		if n < 0 || n > maxEpochSeconds {
			return time.Time{}
		}
		return time.Unix(n, 0).UTC()
	}
	if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
		return t
	}
	return time.Time{}
}
