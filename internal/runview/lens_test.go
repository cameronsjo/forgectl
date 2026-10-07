// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package runview

import (
	"strings"
	"testing"
	"time"
)

const backupLens = `
about  = "nightly backup"
format = "text"

[text]
pattern = '^(?P<time>\S+) (?P<level>\w+) (?P<event>.*)$'

[[rule]]
action = "ignore"
match  = '^heartbeat'
[[rule]]
action = "start"
match  = '^backing up (?P<step>\S+)'
[[rule]]
action = "close"
match  = '^backed up (?P<step>\S+)'
[[rule]]
action = "fail"
field  = "level"
match  = '^ERROR$'
step   = "upload"
[[rule]]
action = "end"
match  = '^done rc=(?P<exit>\d+)'
`

func mustLens(t *testing.T, text string) *Lens {
	t.Helper()
	l, err := ParseLens("test", []byte(text))
	if err != nil {
		t.Fatalf("ParseLens: %v", err)
	}
	return l
}

func TestParseLens_Refusals(t *testing.T) {
	for _, c := range []struct{ name, text, want string }{
		{"no format", "[[rule]]\naction='end'\nmatch='x'", "format is missing"},
		{"bad format", "format='xml'\n[[rule]]\naction='end'\nmatch='x'", `format "xml"`},
		{"typo key", "format='text'\nrules=1\n[[rule]]\naction='end'\nmatch='x'", "unknown key rules"},
		{"no rules", "format='text'", "no [[rule]]"},
		{"bad action", "format='text'\n[[rule]]\naction='begin'\nmatch='x'", `action "begin"`},
		{"no match", "format='text'\n[[rule]]\naction='end'", "match is missing"},
		{"bad regexp", "format='text'\n[[rule]]\naction='end'\nmatch='('", "[[rule]] 1: match"},
		{"no step", "format='text'\n[[rule]]\naction='start'\nmatch='go'", "needs a step"},
		{"reserved group", "format='text'\n[text]\npattern='(?P<x>.)'\n[[rule]]\naction='end'\nmatch='x'", ""},
		{"text table on json", "format='json'\n[text]\npattern='x'\n[[rule]]\naction='end'\nmatch='x'", "[text] is only read"},
		{"after names nothing", "format='json'\n[[step]]\nid='a'\nafter=['b']\n[[rule]]\naction='end'\nmatch='x'", `after "b"`},
		{"twice", "format='json'\n[[step]]\nid='a'\n[[step]]\nid='a'\n[[rule]]\naction='end'\nmatch='x'", "listed twice"},
		{"rule step undeclared", "format='json'\n[[step]]\nid='a'\n[[rule]]\naction='start'\nmatch='x'\nstep='b'", `step "b" names no [[step]]`},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := ParseLens("t", []byte(c.text))
			if c.name == "reserved group" {
				if err != nil {
					t.Fatalf("a plain group is a field, not an error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want it to contain %q", err, c.want)
			}
		})
	}
	// A JSON step key is enough for a step rule; json has one by default.
	if _, err := ParseLens("t", []byte("format='json'\n[[rule]]\naction='start'\nmatch='go'")); err != nil {
		t.Errorf("a json lens's step key should satisfy a start rule: %v", err)
	}
}

func readAll(t *testing.T, l *Lens, lines ...string) (events []Event, ignored, dropped int, hits []int) {
	t.Helper()
	hits = make([]int, l.Rules())
	for _, line := range lines {
		e, _, res, rule := l.read(len(events)+1, []byte(line))
		if rule > 0 {
			hits[rule-1]++
		}
		switch res {
		case lineIgnored:
			ignored++
		case lineDropped:
			dropped++
		default:
			events = append(events, e)
		}
	}
	return events, ignored, dropped, hits
}

func TestLens_TextLinesFoldIntoSteps(t *testing.T) {
	l := mustLens(t, backupLens)
	evs, ignored, dropped, hits := readAll(t, l,
		"2026-10-07T01:00:00Z INFO starting",
		"2026-10-07T01:00:01Z INFO backing up photos",
		"2026-10-07T01:00:02Z DEBUG heartbeat",
		"2026-10-07T01:03:00Z INFO backed up photos",
		"2026-10-07T01:03:01Z INFO backing up docs",
		"2026-10-07T01:04:00Z ERROR connection reset",
		"    at upload.go:42",
		"2026-10-07T01:04:01Z INFO done rc=1",
	)
	if ignored != 1 || dropped != 0 || len(evs) != 7 {
		t.Fatalf("ignored=%d dropped=%d events=%d, want 1, 0, 7", ignored, dropped, len(evs))
	}
	if want := []int{1, 2, 1, 1, 1}; !equalInts(hits, want) {
		t.Errorf("hits = %v, want %v", hits, want)
	}
	if evs[1].Step != "photos" || evs[1].Time != time.Date(2026, 10, 7, 1, 0, 1, 0, time.UTC) {
		t.Errorf("event 2 = %+v, want step photos at 01:00:01", evs[1])
	}
	if cont := evs[5]; cont.Name != "    at upload.go:42" || cont.Step != "" || !cont.Time.IsZero() {
		t.Errorf("a line the pattern does not match should be the event whole: %+v", cont)
	}

	s := Fold(l.Spec(), l.Steps(), evs)
	if len(s.Steps) != 3 {
		t.Fatalf("steps = %+v, want photos, docs and upload in the order named", s.Steps)
	}
	for i, want := range []struct {
		id     string
		status StepStatus
	}{{"photos", StepClosed}, {"docs", StepInterrupted}, {"upload", StepFailed}} {
		if s.Steps[i].ID != want.id || s.Steps[i].Status != want.status {
			t.Errorf("step %d = %s %s, want %s %s", i, s.Steps[i].ID, s.Steps[i].Status, want.id, want.status)
		}
	}
	if s.Exit == nil || *s.Exit != 1 || s.Live != LiveEnded || len(s.Edges) != 0 {
		t.Errorf("exit=%v live=%s edges=%v, want 1, ended, none", s.Exit, s.Live, s.Edges)
	}
}

func TestLens_JSONKeysAndALineCannotSetItsOwnAction(t *testing.T) {
	l := mustLens(t, `
format = "json"
time_layout = "2006-01-02 15:04:05"
[json]
event = "msg"
step  = "stage"
time  = "ts"
[[rule]]
action = "start"
match  = '^stage begin$'
`)
	evs, _, dropped, _ := readAll(t, l,
		`{"msg":"stage begin","stage":"build","ts":"2026-10-07 09:00:00"}`,
		`{"msg":"noise","stage":"build","@action":"fail"}`,
		`{"nomsg":true}`,
	)
	if dropped != 1 || len(evs) != 2 {
		t.Fatalf("dropped=%d events=%d, want 1 and 2", dropped, len(evs))
	}
	if evs[0].Time.IsZero() || evs[0].Time.Hour() != 9 {
		t.Errorf("time_layout not used: %v", evs[0].Time)
	}
	s := Fold(l.Spec(), nil, evs)
	if len(s.Steps) != 1 || s.Steps[0].Status != StepRunning {
		t.Errorf("a line's own @action must not fail its step: %+v", s.Steps)
	}
	if s.Live != LiveLive {
		t.Errorf("live = %s, want live", s.Live)
	}
}

func TestLens_DeclaredStepsCountUnknownOnes(t *testing.T) {
	l := mustLens(t, `
format = "json"
[[step]]
id = "fetch"
[[step]]
id = "build"
after = ["fetch"]
[[rule]]
action = "start"
match = '^go$'
`)
	evs, _, _, _ := readAll(t, l, `{"event":"go","step":"fetch"}`, `{"event":"go","step":"lint"}`)
	s := Fold(l.Spec(), l.Steps(), evs)
	if len(s.Steps) != 2 || s.UnknownSteps != 1 || len(s.Edges) != 1 {
		t.Errorf("steps=%d unknown=%d edges=%v, want 2, 1, fetch→build", len(s.Steps), s.UnknownSteps, s.Edges)
	}
}

// A replay before a step was discovered must not show it: discovery adds to
// the reducer's step map, so a checkpoint and a replay cannot share one.
func TestFolder_DiscoveredStepsReplayFromCheckpoints(t *testing.T) {
	spec := &Spec{On: map[string]Action{"go": ActionStart}, Discover: true}
	f := NewFolder(spec, nil)
	var evs []Event
	for i := range checkpointEvery + 5 {
		evs = append(evs, Event{Seq: i + 1, Name: "tick"})
	}
	evs = append(evs, Event{Seq: len(evs) + 1, Name: "go", Step: "late"})
	f.Append(evs...)
	if got := f.At(checkpointEvery + 1).Steps; len(got) != 0 {
		t.Fatalf("At before the step = %+v, want no steps", got)
	}
	if got := f.At(f.Len()).Steps; len(got) != 1 || got[0].ID != "late" {
		t.Fatalf("At the tip = %+v, want late", got)
	}
	f.Append(Event{Seq: f.Len() + 1, Name: "go", Step: "later"})
	if got := f.At(checkpointEvery + 1).Steps; len(got) != 0 {
		t.Errorf("a later discovery leaked into an earlier replay: %+v", got)
	}
}

func TestLens_RuleText(t *testing.T) {
	l := mustLens(t, backupLens)
	if got := l.RuleText(4); got != `fail   '^ERROR$' on level step=upload` {
		t.Errorf("RuleText(4) = %q", got)
	}
	if l.RuleText(0) != "" || l.RuleText(99) != "" {
		t.Error("RuleText out of range should be empty")
	}
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestLens_SayRewritesALineIntoPlainWords(t *testing.T) {
	l := mustLens(t, `
format = "text"
[text]
pattern = '^(?P<level>\w+) (?P<event>.*)$'
[[rule]]
action = "note"
match  = '^q=ord\.sel cid=(?P<cid>\d+)'
say    = "querying orders for customer {cid} ({level}, {nope})"
[[rule]]
action = "start"
match  = '^stg (?P<step>\w+) beg$'
say    = "{step} started"
`)
	evs, _, _, hits := readAll(t, l, "INFO q=ord.sel cid=42 lim=10", "INFO stg sync beg")
	if want := []int{1, 1}; !equalInts(hits, want) {
		t.Fatalf("hits = %v, want %v", hits, want)
	}
	if got := evs[0].Name; got != "querying orders for customer 42 (INFO, {nope})" {
		t.Errorf("say = %q", got)
	}
	if v, _ := evs[0].field(LensLineField); v != "q=ord.sel cid=42 lim=10" {
		t.Errorf("@line = %q, want the line as read", v)
	}
	if evs[1].Name != "sync started" || evs[1].Step != "sync" {
		t.Errorf("event 2 = %+v", evs[1])
	}
	s := Fold(l.Spec(), nil, evs)
	if len(s.Steps) != 1 || s.Steps[0].ID != "sync" {
		t.Errorf("a note must not touch a step: %+v", s.Steps)
	}
	for _, bad := range []string{
		"format='text'\n[[rule]]\naction='note'\nmatch='x'",
		"format='text'\n[[rule]]\naction='ignore'\nmatch='x'\nsay='y'",
		"format='text'\n[[rule]]\naction='note'\nmatch='x'\nsay='a { b'",
	} {
		if _, err := ParseLens("t", []byte(bad)); err == nil {
			t.Errorf("ParseLens(%q) should refuse", bad)
		}
	}
}
