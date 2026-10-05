// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

//go:build unix

package runview

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/cameronsjo/forgectl/internal/termsafe/termsafetest"
)

func mkfifo(p string) error { return syscall.Mkfifo(p, 0o600) }

func TestFieldRules(t *testing.T) {
	f := newFixture(t)
	f.write(logRel, strings.Join([]string{
		`{"kind":"stage_started","stage":"fetch","try":2,"big":1152921504606846976,"neg":-5}`,
		`not json`,
		`{"kind":"stage_done","stage":"fetch","nested":{"a":[1,2]},"ratio":0.5,"exp":1e3,"huge":9223372036854775808,"ok":true,"none":null,"rc":0}`,
		`{"kind":"first","kind":"second","stage":"build"}`,
		`{"kind":"stage_done"} {"kind":"extra"}`,
		`["kind","stage_done"]`,
		`{"stage":"fetch"}`,
		``,
	}, "\n")+"\n")
	d := f.load("alpha", &Cursor{})

	if got := fmt.Sprint(names(d.Events)); got != "[1:stage_started 3:stage_done 4:first]" {
		t.Fatalf("events %s", got)
	}
	// 2^60 survives exactly; json.Unmarshal into float64 would round it.
	want0 := []Field{{"kind", "stage_started"}, {"stage", "fetch"}, {"try", "2"}, {"big", "1152921504606846976"}, {"neg", "-5"}}
	if !reflect.DeepEqual(d.Events[0].Fields, want0) {
		t.Errorf("fields = %v, want %v", d.Events[0].Fields, want0)
	}
	if d.Events[0].Fields[3].Value != strconv.FormatInt(1<<60, 10) {
		t.Errorf("2^60 = %s", d.Events[0].Fields[3].Value)
	}
	// The nested value, the float, the exponent, the out-of-range integer,
	// the boolean and the null are dropped from the field; rc stays.
	want2 := []Field{{"kind", "stage_done"}, {"stage", "fetch"}, {"rc", "0"}}
	if !reflect.DeepEqual(d.Events[1].Fields, want2) {
		t.Errorf("fields = %v, want %v", d.Events[1].Fields, want2)
	}
	if d.DroppedFields != 6 {
		t.Errorf("DroppedFields = %d, want 6", d.DroppedFields)
	}
	// The first of a repeated key wins.
	if d.Events[2].Name != "first" || len(d.Events[2].Fields) != 2 {
		t.Errorf("duplicate key: %+v", d.Events[2])
	}
	// "not json", the two-value line, the array line and the line with no
	// event name are dropped whole; the blank line is not counted.
	if d.Dropped != 4 {
		t.Errorf("Dropped = %d, want 4", d.Dropped)
	}
}

func TestEventsMapTheSpecKeys(t *testing.T) {
	f := newFixture(t)
	f.write(logRel, `{"at":"2026-01-02T03:04:05Z","kind":"stage_started","stage":"fetch","try":3}`+"\n"+
		`{"at":1767323050,"kind":"run_finished","rc":0}`+"\n")
	d := f.load("alpha", &Cursor{})
	if len(d.Events) != 2 {
		t.Fatalf("events %v", names(d.Events))
	}
	e := d.Events[0]
	if e.Name != "stage_started" || e.Step != "fetch" || !e.Time.Equal(t0) {
		t.Errorf("event 1 = %+v", e)
	}
	if d.Events[1].Time.Unix() != 1767323050 {
		t.Errorf("epoch time = %v", d.Events[1].Time)
	}
	s := Fold(f.spec, nil, d.Events, at(60))
	if st := step(t, s, "fetch"); st.Status != StepRunning || st.Attempt != 3 {
		t.Errorf("fold: fetch = %+v", st)
	}
	if s.Exit == nil || *s.Exit != 0 || s.Live != LiveEnded {
		t.Errorf("fold: exit %v live %s", s.Exit, s.Live)
	}
}

func TestMetricsAreReadPerLine(t *testing.T) {
	f := newFixture(t)
	f.write(logRel, evLine("stage_started", "fetch"))
	f.write("runs/alpha/out/usage.jsonl", strings.Join([]string{
		`{"in":10,"out":4,"status":"ok"}`,
		`{"in":1.5,"out":2,"status":"error"}`,
		`oops`,
	}, "\n")+"\n")
	cur := &Cursor{}
	d := f.load("alpha", cur)
	want := []Metric{
		{Seq: 1, Values: map[string]int64{"in": 10, "out": 4}},
		{Seq: 2, Values: map[string]int64{"out": 2}, Error: true},
	}
	if !reflect.DeepEqual(d.Metrics, want) {
		t.Fatalf("metrics = %+v, want %+v", d.Metrics, want)
	}
	if d.Dropped != 1 || d.DroppedFields != 1 {
		t.Errorf("dropped %d, fields %d; want 1 and 1", d.Dropped, d.DroppedFields)
	}
	f.appendTo("runs/alpha/out/usage.jsonl", `{"in":3}`+"\n")
	if d := f.load("alpha", cur); len(d.Metrics) != 1 || d.Metrics[0].Seq != 4 || len(d.Events) != 0 {
		t.Fatalf("second load: metrics %+v, events %d", d.Metrics, len(d.Events))
	}
}

func TestASummaryStemJoinsItsRun(t *testing.T) {
	f := newFixture(t)
	f.write(logRel, evLine("stage_started", "fetch"))
	f.write("results/alpha.summary.json", `{"score":7,"name":"alpha","tags":["a","b"],"detail":{"rc":0,"ratio":0.5},"name":"dup"}`)
	f.write("results/beta.summary.json", `{"score":1}`)
	f.write("results/bad name.summary.json", `{}`)
	refs, err := f.src.List()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]RunRef{}
	for _, r := range refs {
		got[r.Name] = r
	}
	if len(got) != 2 || got["alpha"].Kind != KindLog || !got["alpha"].HasSummary || got["beta"].Kind != KindSummary {
		t.Fatalf("refs = %+v", refs)
	}

	cur := &Cursor{}
	d, err := f.src.Load(got["alpha"], cur)
	if err != nil {
		t.Fatal(err)
	}
	want := []KV{{"detail.rc", "0"}, {"name", "alpha"}, {"score", "7"}, {"tags[0]", "a"}, {"tags[1]", "b"}}
	if !reflect.DeepEqual(d.Summary, want) || len(d.Events) != 1 || d.DroppedFields != 1 {
		t.Fatalf("summary %v (want %v), events %d, droppedFields %d", d.Summary, want, len(d.Events), d.DroppedFields)
	}
	if d, _ := f.src.Load(got["alpha"], cur); d.Summary != nil {
		t.Errorf("unchanged summary returned again: %v", d.Summary)
	}
	f.write("results/alpha.summary.json", `{"score":8}`)
	if d, _ := f.src.Load(got["alpha"], cur); !reflect.DeepEqual(d.Summary, []KV{{"score", "8"}}) {
		t.Errorf("changed summary = %v", d.Summary)
	}
	d, err = f.src.Load(got["beta"], &Cursor{})
	if err != nil || !reflect.DeepEqual(d.Summary, []KV{{"score", "1"}}) || d.Err != nil {
		t.Errorf("summary-only run: %v, %v, %v", d.Summary, err, d.Err)
	}
}

func TestListSkipsBadNamesAndNonDirs(t *testing.T) {
	f := newFixture(t)
	for _, name := range []string{"good", ".hidden", "bad name", "-dash"} {
		f.write("runs/"+name+"/keep", "")
	}
	f.write("runs/plainfile", "")
	f.symlink("good", "runs/alias")
	refs, err := f.src.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 || refs[0].Name != "good" {
		t.Fatalf("refs = %+v; want only good", refs)
	}
	if _, err := f.src.Load(RunRef{Name: "../x", Kind: KindLog}, &Cursor{}); !errors.Is(err, ErrRefused) {
		t.Errorf("Load(../x) err = %v", err)
	}
}

func TestTheRootMustBeSetAndAbsolute(t *testing.T) {
	spec := loadExample(t)
	t.Setenv("PIPELINE_ROOT", "")
	if _, err := NewSpecSource(spec); !errors.Is(err, ErrRoot) || !strings.Contains(err.Error(), "$PIPELINE_ROOT is unset") {
		t.Errorf("unset: %v", err)
	}
	t.Setenv("PIPELINE_ROOT", "relative/dir")
	if _, err := NewSpecSource(spec); !errors.Is(err, ErrRoot) || !strings.Contains(err.Error(), "absolute") {
		t.Errorf("relative: %v", err)
	}
}

// The spec source hands back the spec its runs fold with.
func TestASpecSourceProvidesItsSpec(t *testing.T) {
	spec := loadExample(t)
	t.Setenv("PIPELINE_ROOT", t.TempDir())
	src, err := NewSpecSource(spec)
	if err != nil {
		t.Fatal(err)
	}
	p, ok := src.(SpecProvider)
	if !ok || p.Spec() != spec {
		t.Fatalf("the spec source does not provide its spec: %v", ok)
	}
}

// Every string a Delta carries is inert, whichever field the hostile text
// arrived in.
func TestHostileLogTextIsInert(t *testing.T) {
	f := newFixture(t)
	h := termsafetest.Hostile("x")
	qb, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	q := string(qb)
	f.write(logRel, fmt.Sprintf(`{"kind":%s,"stage":%s,%s:%s,"at":%s}`+"\n", q, q, q, q, q))
	f.write("runs/alpha/out/usage.jsonl", fmt.Sprintf(`{"in":1,%s:2,"status":%s}`+"\n", q, q))
	f.write("results/alpha.summary.json", fmt.Sprintf(`{%s:%s,"n":{%s:[%s]}}`, q, q, q, q))
	refs, err := f.src.List()
	if err != nil || len(refs) != 1 {
		t.Fatalf("List: %v %v", refs, err)
	}
	d, err := f.src.Load(refs[0], &Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Events) != 1 || len(d.Summary) != 2 {
		t.Fatalf("events %d summary %d; want the hostile text kept, made inert", len(d.Events), len(d.Summary))
	}
	assertDeltaInert(t, d)
}

// assertDeltaInert checks every string reachable from d.
func assertDeltaInert(t *testing.T, d Delta) {
	t.Helper()
	check := func(label, s string) { termsafetest.AssertInert(t, label, s) }
	for _, e := range d.Events {
		check("event name", e.Name)
		check("event step", e.Step)
		for _, fl := range e.Fields {
			check("field key", fl.Key)
			check("field value", fl.Value)
		}
	}
	for _, m := range d.Metrics {
		for k := range m.Values {
			check("metric key", k)
		}
	}
	for _, kv := range d.Summary {
		check("summary key", kv.Key)
		check("summary value", kv.Value)
	}
	for _, def := range d.Defs {
		check("def id", def.ID)
		check("def note", def.Note)
		for _, a := range def.After {
			check("def after", a)
		}
	}
	for _, it := range d.Items {
		for _, s := range []string{it.Name, it.Kind, it.State, it.What, it.Why, it.SkipReason, it.SHA256, it.Refusal} {
			check("item", s)
		}
		for _, l := range it.Body {
			check("item body", l)
		}
	}
	for _, tm := range d.Timing {
		check("timing step", tm.Step)
		check("timing state", tm.State)
	}
	if d.Err != nil {
		check("err", d.Err.Error())
	}
}

// A [log] key the spec leaves unset never matches a JSON key that is empty.
func TestAnUnsetLogKeyMatchesNothing(t *testing.T) {
	spec := loadExample(t)
	spec.Log.Step, spec.Log.Time = "", ""
	e, ok := logEvent(spec, 1, []Field{{"", "fetch"}, {"kind", "stage_started"}})
	if !ok || e.Step != "" || !e.Time.IsZero() || e.Name != "stage_started" {
		t.Errorf("event = %+v, %v", e, ok)
	}
}
