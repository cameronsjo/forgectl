// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package runview

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// RunKind says what a run is read from.
type RunKind string

const (
	KindLog     RunKind = "log"     // a run dir with an events log
	KindSummary RunKind = "summary" // a summary file with no run dir
	KindDesk    RunKind = "desk"    // a desk item
)

// RunRef names one run of a source. Updated is the newest modification time
// the source saw for it. HasSummary is set on a log run whose name also has
// a summary file. Live is a desk run's state as the listing scan saw it
// (empty for a log run), so a caller can tell that a run it does not reload
// changed state, as when its supervisor died.
type RunRef struct {
	Source, Name string
	Kind         RunKind
	Updated      time.Time
	HasSummary   bool
	Live         LiveState
}

// Metric is one line of a run's metrics file: the integer values of the
// spec's sum keys on that line (a key whose value is missing or not an
// integer is absent), and whether the line matched error_when. Seq is the
// line's number in the file, from 1. Summing the series is the reader's job.
type Metric struct {
	Seq    int
	Values map[string]int64
	Error  bool
}

// KV is one leaf of a flattened summary: Key is its path ("a.b", "list[0]"),
// Value its text. A Delta's Summary is sorted by Key.
type KV struct{ Key, Value string }

// Cursor is where a Load stopped in one run, so the next Load reads only what
// was added. The caller keeps one Cursor per run and passes the same pointer
// to every Load; a zero Cursor reads the run from the start. Every byte the
// next Load needs (a held partial line, the counts behind Seq and the caps)
// lives here, not in the Source, so a Source holds no per-run read state.
//
// Offset, Dev and Ino are the events log's position and identity (for a
// desk run, Offset is the number of event lines the watcher has seen).
// Metrics is the metrics file's own position, and Summary the summary
// file's identity, so each file is resumed and checked for replacement on
// its own.
type Cursor struct {
	Offset   int64
	Dev, Ino uint64
	Metrics  *Cursor
	Summary  *Cursor

	opened   bool      // Dev and Ino were recorded, so a change is a replacement
	size     int64     // summary only: the size and mtime last read
	mtime    time.Time // summary only
	held     []byte    // the start of a line whose newline has not arrived
	dropping bool      // the held line passed maxLineBytes; drop it at its newline
	lines    int       // complete lines read, the next Seq
	kept     int       // events or metric lines delivered, for maxRunEvents
	dropped  int       // lines dropped: too long, not JSON, or past maxRunEvents
	fields   int       // field values dropped: not a string or an int64
	lost     bool      // desk only: RUN-LOST was delivered
	desk     any       // desk only: the source's watcher for this run
}

// Delta is what one Load found.
//
// Events and Metrics are new since the cursor's last Load; the caller
// appends them. When Reset is set, a file was replaced or shrank since the
// last Load: the caller discards every Event and Metric it holds for the run,
// and this Delta carries the run again from its first line.
//
// Defs and Timing are complete each time they are set. Summary is nil
// when the summary file did not change since the last Load, and the complete
// flattened summary (possibly empty) when it did.
//
// Dropped and DroppedFields are totals for the run since the last Reset, not
// increments. Partial reports a file past the 32 MiB cap; the part past it is
// not read. Err reports a file that could not be read while the rest of the
// run still loaded; it wraps fs.ErrNotExist when the events log does not
// exist yet, and ErrRefused when a path is a symlink or not a regular file.
//
// Every string in a Delta has been through termsafe at ingest.
type Delta struct {
	Events  []Event
	Metrics []Metric
	Summary []KV
	Defs    []StepDef
	Dropped int
	Partial bool
	Reset   bool
	Err     error

	// DroppedFields counts field values left out of kept lines: floats,
	// integers outside int64, nested values and nulls. For a summary it also
	// counts a leaf whose key repeats one already kept.
	DroppedFields int
	// Live is a desk run's state as the desk sees it, and ended for a
	// summary-only run (a summary is written once a run is over); empty for
	// a log run, whose state Fold derives.
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

// QueueItem is the sanitized copy of a desk item that the queue view and the
// approve modal show.
//
// SHA256 is the full 64-hex hash of the item's bytes, and is set only when
// the item's recorded hash equals the hash of the content the scan read; an
// empty SHA256 means the item cannot be approved. Refusal says why an item
// cannot be approved (the desk refused it, or its hash did not match).
// Bytes is the size of that content. Claimed is set for an item that was
// claimed at some point, which Unskip refuses to re-arm.
//
// Body and Marks are set only by QueueSource.QueueBody, for the one item the
// approve modal opens on: Body is the item's lines through termsafe.SafeLine,
// uncapped, and Marks has one entry per Body line, counted from the raw
// bytes before they were escaped. Queue leaves both empty, so a queue of
// large items costs no more than their names.
type QueueItem struct {
	Name, Kind, State, What, Why, SkipReason, SHA256 string
	TTY, Stale, Claimed                              bool
	Body                                             []string
	Exit                                             *int
	Refusal                                          string
	Bytes                                            int
	Marks                                            []LineMarks
}

// QueueList is one Queue result: the items, and how many pending items the
// scan left unread because they fell past its caps.
type QueueList struct {
	Items  []QueueItem
	Unread int
}

// LineMarks counts the runes on one raw line that do not read as what they
// are. Escaped runes are those termsafe.SafeLine does not draw as
// themselves. Invisible runes (termsafe.IsInvisibleRune) can be drawn and
// still look like nothing, so a command name can hide one. NonASCII counts
// every rune past U+007F, since a look-alike letter is one of them.
type LineMarks struct {
	Escaped, Invisible, NonASCII int
}

// Any reports whether the line holds a rune worth a second look.
func (l LineMarks) Any() bool { return l.Escaped+l.Invisible+l.NonASCII > 0 }

// MarkLine counts line's runes. An invalid byte counts as escaped, since
// SafeLine draws it as U+FFFD.
func MarkLine(line string) LineMarks {
	var m LineMarks
	for i := 0; i < len(line); {
		r, size := utf8.DecodeRuneInString(line[i:])
		i += size
		if r < utf8.RuneSelf && r >= ' ' && r != 0x7f {
			continue
		}
		if r >= utf8.RuneSelf || r == utf8.RuneError {
			m.NonASCII++
		}
		if termsafe.IsInvisibleRune(r) {
			m.Invisible++
		}
		if (r == utf8.RuneError && size == 1) || termsafe.SafeLine(string(r)) != string(r) {
			m.Escaped++
		}
	}
	return m
}

// Source lists runs and loads them incrementally.
type Source interface {
	Name() string
	List() ([]RunRef, error)
	Load(ref RunRef, cur *Cursor) (Delta, error)
}

// SpecProvider is a Source whose runs fold with a spec: the spec source. A
// source without one shows its events with no step state.
type SpecProvider interface {
	Spec() *Spec
}

// QueueSource is a Source that also has a queue: the desk. Queue scans it,
// so the queue view works when no run exists yet. QueueBody reads one
// pending item again for review: its Body and Marks as well, from bytes that
// must still hash to sha (an error wrapping desk.ErrChanged otherwise).
type QueueSource interface {
	Source
	Queue() (QueueList, error)
	QueueBody(name, sha string) (QueueItem, error)
}

// Errors a caller branches on.
var (
	// ErrRefused marks a path component that is a symlink, a directory
	// expected to be a file or the reverse, or anything not a regular file.
	ErrRefused = errors.New("refused")
	// ErrRoot marks a spec whose root variable is unset or not absolute.
	ErrRoot = errors.New("run root")
)

// Ingest caps.
const (
	maxFileBytes    = 32 << 20 // per file, from offset 0; past it the run is Partial
	maxLineBytes    = 64 << 10 // a longer line is dropped and counted
	maxRunEvents    = 50000    // per run (and per metrics file); the rest are counted
	maxMatches      = 500      // run dirs or summaries per pattern, newest first
	maxFieldRunes   = 256      // every ingested string, after termsafe
	maxSummaryBytes = 1 << 20  // a larger summary is refused and the run is Partial
	maxSummaryKeys  = 2000     // leaves kept from one summary; the rest are counted
	maxSummaryDepth = 32       // a value nested deeper is dropped and counted
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
// one step, "script", whose start and end the desk source derives from
// RUN-START and RUN-END. STEP-SKIP and RUN-LOST stay unmapped: the step stays
// pending and Delta.Timing and Delta.Live say why.
func DeskSpec() *Spec {
	return &Spec{
		Name: "desk",
		On: map[string]Action{
			deskStepStart: ActionStart,
			deskStepEnd:   ActionClose,
			deskStepFail:  ActionFail,
			deskRunEnd:    ActionEnd,
		},
		End: EndSpec{ExitField: "rc"},
	}
}

// Desk event names as Events carry them. They match the desk's own keys,
// except deskStepFail.
const (
	deskStepStart = "STEP-START"
	deskStepEnd   = "STEP-END"
	deskStepFail  = "STEP-FAIL"
	deskRunEnd    = "RUN-END"
	// ScriptStep is the one step of a desk script.
	ScriptStep = "script"
)

// scalarFields decodes one line holding one JSON object into its fields, in
// the order they appear. A string, an integer that fits in int64, or a
// boolean is kept as text; any other value (a float, a larger integer, an
// object or array, null) is left out and counted in dropped. For a repeated key the
// first one decides, whether or not it was kept. ok is false when the line is
// not exactly one JSON object. Keys and values are raw; the caller cleans them.
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
		if !keep {
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

// flattenSummary decodes a summary document into its leaves, keyed by path,
// sorted, with the same kept-value rule as scalarFields. A leaf past
// maxSummaryKeys or deeper than maxSummaryDepth is counted in dropped, and
// so is a leaf whose cleaned key is one already kept ({"a.b":1,"a":{"b":2}}
// flattens both to a.b): the first wins. Keys and values are cleaned.
func flattenSummary(data []byte) (kvs []KV, dropped int, err error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	seen := map[string]bool{}
	var walk func(path string, depth int) error
	walk = func(path string, depth int) error {
		t, err := dec.Token()
		if err != nil {
			return err
		}
		d, nested := t.(json.Delim)
		if !nested {
			text, keep := scalar(t)
			key := clean(path)
			switch {
			case !keep || len(kvs) >= maxSummaryKeys || seen[key]:
				dropped++
			default:
				seen[key] = true
				kvs = append(kvs, KV{Key: key, Value: clean(text)})
			}
			return nil
		}
		if depth >= maxSummaryDepth {
			dropped++
			return skipValue(dec)
		}
		for i := 0; dec.More(); i++ {
			child := path + "[" + strconv.Itoa(i) + "]"
			if d == '{' {
				k, err := dec.Token()
				if err != nil {
					return err
				}
				key, _ := k.(string)
				child = key
				if path != "" {
					child = path + "." + key
				}
			}
			if err := walk(child, depth+1); err != nil {
				return err
			}
		}
		_, err = dec.Token() // the closing delimiter
		return err
	}
	if err := walk("", 0); err != nil {
		return nil, 0, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, 0, errors.New("more than one JSON value")
	}
	slices.SortFunc(kvs, func(a, b KV) int { return strings.Compare(a.Key, b.Key) })
	if kvs == nil {
		kvs = []KV{}
	}
	return kvs, dropped, nil
}

// logEvent builds an Event from one log line's fields under spec's [log]
// keys. Every key and value is cleaned. ok is false when the line has no
// event name, which drops the line.
func logEvent(spec *Spec, seq int, raw []Field) (Event, bool) {
	e := Event{Seq: seq, Fields: make([]Field, 0, len(raw))}
	named := false
	for _, f := range raw {
		f = Field{Key: clean(f.Key), Value: clean(f.Value)}
		e.Fields = append(e.Fields, f)
		if f.Key == "" {
			continue // an unset [log] key must not match it
		}
		if f.Key == spec.Log.Event && !named {
			e.Name, named = f.Value, true
		}
		if f.Key == spec.Log.Step && e.Step == "" {
			e.Step = f.Value
		}
		if f.Key == spec.Log.Time && e.Time.IsZero() {
			e.Time = parseTime(f.Value)
		}
	}
	return e, named && e.Name != ""
}

// parseTime reads an event time: RFC 3339 text, or an integer count of
// seconds since the epoch. Anything else is the zero time.
func parseTime(v string) time.Time {
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		return time.Unix(n, 0).UTC()
	}
	if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
		return t
	}
	return time.Time{}
}

// metricLine builds a Metric from one metrics line's fields.
func metricLine(spec *Spec, seq int, raw []Field) Metric {
	m := Metric{Seq: seq, Values: map[string]int64{}}
	for _, f := range raw {
		if ew := spec.Metrics.ErrorWhen; ew != nil && f.Key == ew.Key && f.Value == ew.Value {
			m.Error = true
		}
		if !slices.Contains(spec.Metrics.Sum, f.Key) {
			continue
		}
		if n, err := strconv.ParseInt(f.Value, 10, 64); err == nil {
			m.Values[clean(f.Key)] = n
		}
	}
	return m
}
