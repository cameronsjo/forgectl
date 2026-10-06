// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package runview

import (
	"errors"
	"fmt"
	"io/fs"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestScalarFieldsKeepsStringsIntegersAndBooleans(t *testing.T) {
	got, dropped, ok := scalarFields([]byte(`{"s":"v","i":2,"big":1152921504606846976,"neg":-5,"t":true,"f":false,"max":9223372036854775807}`))
	if !ok || dropped != 0 {
		t.Fatalf("ok %v dropped %d", ok, dropped)
	}
	want := []Field{{"s", "v"}, {"i", "2"}, {"big", strconv.FormatInt(1<<60, 10)}, {"neg", "-5"}, {"t", "true"}, {"f", "false"}, {"max", "9223372036854775807"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("fields = %v, want %v", got, want)
	}
}

func TestScalarFieldsDropsAndCountsTheRest(t *testing.T) {
	for name, line := range map[string]string{
		"float":         `{"a":0.5}`,
		"exponent":      `{"a":1e3}`,
		"int64 overrun": `{"a":9223372036854775808}`,
		"null":          `{"a":null}`,
		"object":        `{"a":{"b":[1,{"c":2}]}}`,
		"array":         `{"a":[1,[2,3],{"x":null}]}`,
	} {
		got, dropped, ok := scalarFields([]byte(line))
		if !ok || dropped != 1 || len(got) != 0 {
			t.Errorf("%s: fields %v dropped %d ok %v; want none, 1 dropped", name, got, dropped, ok)
		}
	}
	// A nested value does not derail the fields after it.
	got, dropped, ok := scalarFields([]byte(`{"a":{"x":[1,2]},"b":"kept","c":null,"d":"also"}`))
	if want := []Field{{"b", "kept"}, {"d", "also"}}; !ok || dropped != 2 || !reflect.DeepEqual(got, want) {
		t.Errorf("fields %v dropped %d ok %v", got, dropped, ok)
	}
}

func TestScalarFieldsFirstDuplicateWins(t *testing.T) {
	got, dropped, ok := scalarFields([]byte(`{"k":"first","k":"second","n":1,"n":2}`))
	if want := []Field{{"k", "first"}, {"n", "1"}}; !ok || dropped != 0 || !reflect.DeepEqual(got, want) {
		t.Errorf("fields %v dropped %d ok %v, want %v", got, dropped, ok, want)
	}
	// The first decides even when it was dropped: the later string is not kept.
	got, dropped, ok = scalarFields([]byte(`{"k":null,"k":"later"}`))
	if !ok || dropped != 1 || len(got) != 0 {
		t.Errorf("dropped-first duplicate: fields %v dropped %d ok %v", got, dropped, ok)
	}
}

func TestScalarFieldsRejectsAnythingButOneObject(t *testing.T) {
	for name, line := range map[string]string{
		"empty":            ``,
		"not json":         `not json`,
		"array":            `["kind","x"]`,
		"scalar":           `"x"`,
		"two objects":      `{"a":"b"} {"c":"d"}`,
		"trailing garbage": `{"a":"b"} x`,
		"trailing comma":   `{"a":"b",}`,
		"unterminated":     `{"a":"b"`,
		"truncated value":  `{"a":`,
		"truncated nested": `{"a":{"b":`,
	} {
		if got, _, ok := scalarFields([]byte(line)); ok || got != nil {
			t.Errorf("%s: fields %v ok %v; want rejected", name, got, ok)
		}
	}
	if got, dropped, ok := scalarFields([]byte(`{}`)); !ok || dropped != 0 || len(got) != 0 {
		t.Errorf("empty object: %v %d %v", got, dropped, ok)
	}
	if _, _, ok := scalarFields([]byte(`  {"a":"b"}  `)); !ok {
		t.Errorf("surrounding whitespace rejected")
	}
}

func TestLogEventReadsNameStepAndTime(t *testing.T) {
	e, ok := logEvent(DefaultLogKeys, 7, []Field{{"time", "2026-01-02T03:04:05Z"}, {"event", "start"}, {"step", "fetch"}, {"extra", "1"}})
	if !ok || e.Seq != 7 || e.Name != "start" || e.Step != "fetch" || !e.Time.Equal(t0) || len(e.Fields) != 4 {
		t.Errorf("event = %+v, %v", e, ok)
	}
	// Custom keys.
	keys := LogKeys{Event: "kind", Step: "stage", Time: "at"}
	e, ok = logEvent(keys, 1, []Field{{"kind", "k"}, {"stage", "s"}, {"at", "60"}, {"event", "ignored"}})
	if !ok || e.Name != "k" || e.Step != "s" || e.Time.Unix() != 60 {
		t.Errorf("custom keys: %+v, %v", e, ok)
	}
}

func TestLogEventWithoutAnEventNameIsDropped(t *testing.T) {
	for name, raw := range map[string][]Field{
		"no fields":    nil,
		"no event key": {{"step", "fetch"}},
		"empty name":   {{"event", ""}, {"step", "fetch"}},
	} {
		if e, ok := logEvent(DefaultLogKeys, 1, raw); ok {
			t.Errorf("%s: kept %+v", name, e)
		}
	}
}

func TestLogEventUnsetStepAndTimeKeysMatchNothing(t *testing.T) {
	keys := LogKeys{Event: "event"}
	e, ok := logEvent(keys, 1, []Field{{"", "fetch"}, {"event", "x"}, {"step", "s"}, {"time", "60"}})
	if !ok || e.Step != "" || !e.Time.IsZero() || e.Name != "x" {
		t.Errorf("event = %+v, %v", e, ok)
	}
}

func TestLogEventCleansKeysAndValues(t *testing.T) {
	e, ok := logEvent(DefaultLogKeys, 1, []Field{{"event", "go\x1b[2Jne"}, {"k\x1b]0;t\a", "v\x00"}})
	if !ok {
		t.Fatal("dropped")
	}
	for _, s := range []string{e.Name, e.Fields[0].Value, e.Fields[1].Key, e.Fields[1].Value} {
		if strings.ContainsAny(s, "\x1b\x00\a") {
			t.Errorf("control byte survived in %q", s)
		}
	}
}

func TestLogEventFirstOfARepeatedKeyWins(t *testing.T) {
	// Fields can repeat only through logEvent's own callers; the first match
	// still decides name, step and time.
	e, ok := logEvent(DefaultLogKeys, 1, []Field{{"event", "a"}, {"event", "b"}, {"step", "s1"}, {"step", "s2"}, {"time", "10"}, {"time", "20"}})
	if !ok || e.Name != "a" || e.Step != "s1" || e.Time.Unix() != 10 {
		t.Errorf("event = %+v", e)
	}
}

func TestParseTime(t *testing.T) {
	for _, c := range []struct {
		in   string
		want time.Time
	}{
		{"2026-01-02T03:04:05Z", t0},
		{"2026-01-02T03:04:05.5Z", t0.Add(500 * time.Millisecond)},
		{"2026-01-02T04:04:05+01:00", t0},
		{strconv.FormatInt(t0.Unix(), 10), t0},
		{"0", time.Unix(0, 0).UTC()},
		{"", time.Time{}},
		{"yesterday", time.Time{}},
		{"1.5", time.Time{}},
		{"2026-01-02", time.Time{}},
		{"1e3", time.Time{}},
		{"99999999999999999999", time.Time{}},
	} {
		if got := parseTime(c.in); !got.Equal(c.want) {
			t.Errorf("parseTime(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestCleanErrKeepsTheWrappedError(t *testing.T) {
	if cleanErr(nil) != nil {
		t.Errorf("cleanErr(nil) != nil")
	}
	err := cleanErr(errors.New("bad\x1b[2J path"))
	if strings.Contains(err.Error(), "\x1b") {
		t.Errorf("error text still has ESC: %q", err.Error())
	}
	wrapped := cleanErr(errors.Join(fs.ErrNotExist, errors.New("x")))
	if !errors.Is(wrapped, fs.ErrNotExist) {
		t.Errorf("cleanErr lost the wrapped error")
	}
}

func TestDeskSpecMapsTheDeskVocabulary(t *testing.T) {
	s := DeskSpec()
	want := map[string]Action{"STEP-START": ActionStart, "STEP-END": ActionClose, "STEP-FAIL": ActionFail, "STEP-SKIP": ActionSkip, "RUN-END": ActionEnd, "RUN-LOST": ActionLost}
	if !reflect.DeepEqual(s.On, want) || s.ExitField != "rc" {
		t.Errorf("DeskSpec = %+v", s)
	}
}

// An epoch count past year 9999 (milliseconds, say) is no time at all: kept,
// it is a year JSON cannot encode, and it sorts the run past every other.
func TestParseTimeRefusesAnEpochPastJSONsRange(t *testing.T) {
	for _, v := range []string{"1700000000000", "-1", "253402300800"} {
		if got := parseTime(v); !got.IsZero() {
			t.Errorf("parseTime(%q) = %v, want the zero time", v, got)
		}
	}
	if got := parseTime("253402300799"); got.Year() != 9999 {
		t.Errorf("parseTime(the last encodable second) = %v, want year 9999", got)
	}
}

// One line keeps at most maxEventFields fields; the rest are counted.
func TestScalarFieldsCapsFieldsPerLine(t *testing.T) {
	var b strings.Builder
	b.WriteString("{")
	for i := range maxEventFields + 10 {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `"k%d":%d`, i, i)
	}
	b.WriteString("}")
	fields, dropped, ok := scalarFields([]byte(b.String()))
	if !ok || len(fields) != maxEventFields || dropped != 10 {
		t.Fatalf("kept %d dropped %d ok %v, want %d and 10", len(fields), dropped, ok, maxEventFields)
	}
}
