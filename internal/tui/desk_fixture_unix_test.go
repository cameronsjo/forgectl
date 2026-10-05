// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

//go:build unix

package tui

import (
	"context"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/cameronsjo/forgectl/internal/desk"
	"github.com/cameronsjo/forgectl/internal/runview"
	"github.com/cameronsjo/forgectl/internal/theme"
)

// deskNow is the fixed clock every desk test starts from.
var deskNow = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)

// fakeDeskSource is a Source (and QueueSource) with canned runs. Each Load of
// a run pops that run's next Delta; once they are used up a Load returns an
// empty one.
type fakeDeskSource struct {
	mu       sync.Mutex
	name     string
	spec     *runview.Spec
	refs     []runview.RunRef
	deltas   map[string][]runview.Delta
	items    []runview.QueueItem
	listErr  error
	unread   int
	queueErr error
	bodies   int // QueueBody calls
	loads    map[string]int
	curs     map[string]*runview.Cursor
}

func (f *fakeDeskSource) Name() string { return f.name }

func (f *fakeDeskSource) Spec() *runview.Spec { return f.spec }

func (f *fakeDeskSource) List() ([]runview.RunRef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	return append([]runview.RunRef(nil), f.refs...), nil
}

// Queue lists the items without their bodies, as the desk source does.
func (f *fakeDeskSource) Queue() (runview.QueueList, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.queueErr != nil {
		return runview.QueueList{}, f.queueErr
	}
	out := runview.QueueList{Unread: f.unread}
	for _, it := range f.items {
		it.Body, it.Marks = nil, nil
		out.Items = append(out.Items, it)
	}
	return out, nil
}

var _ runview.QueueSource = (*fakeDeskSource)(nil)

// QueueBody is the item with its body, when its hash is still sha.
func (f *fakeDeskSource) QueueBody(name, sha string) (runview.QueueItem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bodies++
	for _, it := range f.items {
		if it.Name == name {
			if it.SHA256 != sha {
				return runview.QueueItem{}, desk.ErrChanged
			}
			return it, nil
		}
	}
	return runview.QueueItem{}, desk.ErrNotFound
}

func (f *fakeDeskSource) Load(ref runview.RunRef, cur *runview.Cursor) (runview.Delta, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.loads == nil {
		f.loads = map[string]int{}
		f.curs = map[string]*runview.Cursor{}
	}
	f.loads[ref.Name]++
	f.curs[ref.Name] = cur
	q := f.deltas[ref.Name]
	if len(q) == 0 {
		return runview.Delta{}, nil
	}
	f.deltas[ref.Name] = q[1:]
	return q[0], nil
}

func (f *fakeDeskSource) loadCount(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.loads[name]
}

// deskSpec is a neutral spec: four steps, a reject gate, one check, exits,
// metrics, and a staleness limit.
func deskSpec() *runview.Spec {
	return &runview.Spec{
		Name: "pipeline",
		Runs: "runs/*",
		Log: runview.LogSpec{
			Path: "out/events.jsonl", Time: "at", Event: "kind", Step: "stage", Attempt: "try",
			Show: []string{"stage", "try", "rc", "err"},
		},
		Steps: []runview.StepDef{
			{ID: "fetch", Note: "pull the inputs"},
			{ID: "build", Note: "compile and package"},
			{ID: "test", Note: "run the suites"},
			{ID: "ship", Note: "publish the result"},
		},
		Gate: runview.GateSpec{Label: "accepted?", MaxRejects: 3},
		On: map[string]runview.Action{
			"stage_started": runview.ActionStart,
			"stage_done":    runview.ActionClose,
			"stage_refused": runview.ActionReject,
			"stage_gave_up": runview.ActionFail,
			"worker_boot":   runview.ActionRestart,
			"run_finished":  runview.ActionEnd,
		},
		End:     runview.EndSpec{ExitField: "rc"},
		Checks:  []runview.CheckDef{{ID: "audit", Event: "audit_result", Field: "rc", Label: "audit ok?", FailExit: 3}},
		Exits:   map[int]string{0: "finished, audit passed", 3: "audit rejected"},
		Metrics: runview.MetricsSpec{Path: "out/usage.jsonl", Sum: []string{"in", "out"}, ErrorWhen: &runview.Field{Key: "status", Value: "error"}},
		Live:    runview.LiveSpec{StaleAfter: 10 * time.Minute},
	}
}

// ev builds an event at a clock time (hh:mm:ss on deskNow's day).
func ev(seq int, at, name, step string, kv ...string) runview.Event {
	e := runview.Event{Seq: seq, Name: name, Step: step}
	if at != "" {
		t, err := time.Parse("2006-01-02 15:04:05", "2026-10-05 "+at)
		if err != nil {
			panic(err)
		}
		e.Time = t.UTC()
	}
	if step != "" {
		e.Fields = append(e.Fields, runview.Field{Key: "stage", Value: step})
	}
	for i := 0; i+1 < len(kv); i += 2 {
		e.Fields = append(e.Fields, runview.Field{Key: kv[i], Value: kv[i+1]})
	}
	return e
}

func numberEvents(evs []runview.Event) []runview.Event {
	for i := range evs {
		evs[i].Seq = i + 1
	}
	return evs
}

// finishedRun is the events of a run that went through all four steps.
func finishedRun(day string, rc string) []runview.Event {
	return numberEvents([]runview.Event{
		ev(0, day+":00", "worker_boot", ""),
		ev(0, day+":01", "stage_started", "fetch", "try", "1"),
		ev(0, day+":30", "stage_done", "fetch", "try", "1"),
		ev(0, day+":31", "stage_started", "build", "try", "1"),
		ev(0, day+":50", "stage_done", "build", "try", "1"),
		ev(0, day+":51", "stage_started", "test", "try", "1"),
		ev(0, day+":59", "stage_done", "test", "try", "1"),
		ev(0, day+":59", "stage_started", "ship", "try", "1"),
		ev(0, day+":59", "stage_done", "ship", "try", "1"),
		ev(0, day+":59", "audit_result", "", "rc", rc),
		ev(0, day+":59", "run_finished", "", "rc", rc),
	})
}

// deskFixture is the sources the goldens draw: a log source with four runs
// (live, ok, failed, stale) and a desk source with a lost batch and a queue.
func deskFixture() (*fakeDeskSource, *fakeDeskSource) {
	spec := deskSpec()
	alpha := numberEvents([]runview.Event{
		ev(0, "11:50:00", "worker_boot", ""),
		ev(0, "11:50:01", "stage_started", "fetch", "try", "1"),
		ev(0, "11:51:00", "stage_done", "fetch", "try", "1"),
		ev(0, "11:51:01", "stage_started", "build", "try", "1"),
		ev(0, "11:55:00", "stage_refused", "build", "try", "1", "rc", "1", "err", "lint"),
		ev(0, "11:55:30", "stage_started", "build", "try", "2"),
		ev(0, "11:58:00", "stage_refused", "build", "try", "2", "rc", "1", "err", "lint"),
		ev(0, "11:58:30", "stage_started", "build", "try", "3"),
		ev(0, "11:59:48", "heartbeat", "", "note", "cache warm"),
	})
	beta := finishedRun("11:55", "0")
	gamma := finishedRun("10:59", "3")
	nightly := numberEvents([]runview.Event{
		ev(0, "09:50:00", "worker_boot", ""),
		ev(0, "09:50:01", "stage_started", "fetch", "try", "1"),
		ev(0, "09:55:00", "stage_done", "fetch", "try", "1"),
		ev(0, "09:55:01", "stage_started", "build", "try", "1"),
	})
	metrics := []runview.Metric{
		{Seq: 1, Values: map[string]int64{"in": 120, "out": 40}},
		{Seq: 2, Values: map[string]int64{"in": 300, "out": 90}},
		{Seq: 3, Values: map[string]int64{"in": 80, "out": 10}, Error: true},
		{Seq: 4, Values: map[string]int64{"in": 500, "out": 220}},
		{Seq: 5, Values: map[string]int64{"in": 260, "out": 130}},
	}
	summary := []runview.KV{
		{Key: "result.artifacts", Value: "3"},
		{Key: "result.status", Value: "accepted"},
		{Key: "timing.build.secs", Value: "19"},
		{Key: "timing.fetch.secs", Value: "29"},
		{Key: "version", Value: "2"},
	}
	at := func(hms string) time.Time {
		t, _ := time.Parse("2006-01-02 15:04:05", "2026-10-05 "+hms)
		return t.UTC()
	}
	logSrc := &fakeDeskSource{
		name: "pipeline",
		spec: spec,
		refs: []runview.RunRef{
			{Source: "pipeline", Name: "alpha-run-7", Kind: runview.KindLog, Updated: at("11:59:48")},
			{Source: "pipeline", Name: "beta-run-7", Kind: runview.KindLog, Updated: at("11:55:59"), HasSummary: true},
			{Source: "pipeline", Name: "gamma-run-7", Kind: runview.KindLog, Updated: at("10:59:59")},
			{Source: "pipeline", Name: "nightly-b3", Kind: runview.KindLog, Updated: at("09:55:01")},
		},
		deltas: map[string][]runview.Delta{
			"alpha-run-7": {{Events: alpha, Metrics: metrics}},
			"beta-run-7":  {{Events: beta, Summary: summary}},
			"gamma-run-7": {{Events: gamma}},
			"nightly-b3":  {{Events: nightly}},
		},
	}
	zero := 0
	deskSrc := &fakeDeskSource{
		name: "desk",
		refs: []runview.RunRef{
			{Source: "desk", Name: "batch-04", Kind: runview.KindDesk, Updated: at("09:00:00")},
		},
		deltas: map[string][]runview.Delta{
			"batch-04": {{
				Events: numberEvents([]runview.Event{
					{Name: "STEP-START", Step: "lint"},
					{Name: "STEP-END", Step: "lint", Fields: []runview.Field{{Key: "rc", Value: "0"}}},
					{Name: "STEP-START", Step: "pack"},
				}),
				Defs:   []runview.StepDef{{ID: "lint", Note: "check the sources"}, {ID: "pack", Note: "bundle the output", After: []string{"lint"}}},
				Live:   runview.LiveLost,
				Timing: []runview.StepTiming{{Step: "lint", State: "ok", RC: &zero, Dur: 41 * time.Second}},
			}},
		},
		items: []runview.QueueItem{
			{Name: "0007-refresh", Kind: "script", State: "waiting", What: "refresh the cache", Why: "nightly refresh", SHA256: "x"},
			{Name: "0008-prune", Kind: "script", State: "waiting", What: "prune old output", Stale: true, Refusal: "recorded hash does not match the content"},
			{Name: "0006-sweep", Kind: "batch", State: "lost", What: "sweep the scratch dirs"},
			{Name: "0004-report", Kind: "script", State: "skipped", What: "send the report", SkipReason: "superseded by 0007"},
		},
	}
	return logSrc, deskSrc
}

// deskTestModel builds a model on the fixed clock with the tick scheduler
// stubbed, so a model driven by hand never sleeps.
func deskTestModel(t *testing.T, opts DeskOptions) deskModel {
	t.Helper()
	t.Setenv("NO_COLOR", "")
	if opts.Now == nil {
		opts.Now = func() time.Time { return deskNow }
	}
	m := newDeskModel(context.Background(), opts)
	m.schedule = func() tea.Cmd { return nil }
	return m
}

// pumpDesk runs cmd and every command it leads to, feeding each message back
// through Update, until none is left.
func pumpDesk(t *testing.T, m deskModel, cmd tea.Cmd) deskModel {
	t.Helper()
	queue := []tea.Cmd{cmd}
	for steps := 0; len(queue) > 0; steps++ {
		if steps > 500 {
			t.Fatal("pumpDesk: commands never settled")
		}
		c := queue[0]
		queue = queue[1:]
		if c == nil {
			continue
		}
		msg := c()
		switch v := msg.(type) {
		case tea.BatchMsg:
			queue = append(queue, v...)
			continue
		case tea.QuitMsg:
			continue
		}
		next, nc := m.Update(msg)
		m = next.(deskModel)
		queue = append(queue, nc)
	}
	return m
}

// openDesk sizes a model, runs its Init to quiescence, and returns it.
func openDesk(t *testing.T, opts DeskOptions, w, h int) deskModel {
	t.Helper()
	m := deskTestModel(t, opts)
	next, _ := m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	m = next.(deskModel)
	return pumpDesk(t, m, m.Init())
}

// pressDesk delivers one key and runs what it started.
func pressDesk(t *testing.T, m deskModel, k tea.KeyPressMsg) deskModel {
	t.Helper()
	next, cmd := m.Update(k)
	return pumpDesk(t, next.(deskModel), cmd)
}

func namedKey(code rune) tea.KeyPressMsg { return tea.KeyPressMsg{Code: code} }

// goldenDeskOptions is the fixture as DeskOptions.
func goldenDeskOptions(noIcons bool) DeskOptions {
	logSrc, deskSrc := deskFixture()
	return DeskOptions{Sources: []runview.Source{logSrc, deskSrc}, Theme: theme.Default(), NoIcons: noIcons}
}
