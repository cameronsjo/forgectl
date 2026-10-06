// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

//go:build unix

package runview

import (
	"errors"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cameronsjo/forgectl/internal/desk"
)

// deskSource reads a desk: every item that is running, done or skipped is a
// run. A pending item is not a run yet; the dashboard's queue shows it.
type deskSource struct {
	d *desk.Desk

	mu   sync.Mutex
	snap *desk.Snapshot // the latest scan; Load reads items from it
}

// deskCursor is a desk run's read state, kept in Cursor.desk.
type deskCursor struct {
	w        *desk.Watcher
	manifest *desk.Manifest
	durs     map[string]time.Duration // step -> STEP-END dur=, for a step the status file has not ended
	// rescannedAt is the live state the item was last read again for, so a
	// watcher and a scan that disagree cost one rescan, not one per poll.
	rescannedAt LiveState
}

// NewDeskSource returns the source for d. Scan, which List calls, changes the
// queue: it fixes the hash of a hand-dropped item and moves a changed one to
// skipped/ (ADR-0012).
func NewDeskSource(d *desk.Desk) Source { return &deskSource{d: d} }

func (s *deskSource) Name() string { return "desk" }

func (s *deskSource) scan() (*desk.Snapshot, error) {
	snap, err := s.d.Scan()
	if err != nil {
		return nil, cleanErr(err)
	}
	s.mu.Lock()
	s.snap = snap
	s.mu.Unlock()
	return snap, nil
}

// List returns every running, done and skipped item as a run, newest first.
// A legacy done log (no NN- number) is left out: no verb acts on it, and it
// has no events.
func (s *deskSource) List() ([]RunRef, error) {
	snap, err := s.scan()
	if err != nil {
		return nil, err
	}
	var refs []RunRef
	for _, group := range [][]desk.Item{snap.Running, snap.Done, snap.Skipped} {
		for _, it := range group {
			if it.Legacy {
				continue
			}
			refs = append(refs, RunRef{Source: s.Name(), Name: clean(it.Name), Kind: KindDesk, Updated: itemUpdated(it, snap.Taken), Live: liveOfItem(it.State)})
		}
	}
	slices.SortFunc(refs, func(a, b RunRef) int {
		if c := b.Updated.Compare(a.Updated); c != 0 {
			return c
		}
		return strings.Compare(a.Name, b.Name)
	})
	return refs, nil
}

func itemUpdated(it desk.Item, fallback time.Time) time.Time {
	switch {
	case !it.Ended.IsZero():
		return it.Ended
	case !it.Started.IsZero():
		return it.Started
	case it.Meta.SkippedAt != nil:
		return *it.Meta.SkippedAt
	case it.Meta.AddedAt != nil:
		return *it.Meta.AddedAt
	}
	return fallback
}

// item finds name in the latest scan, scanning again when it is not there.
func (s *deskSource) item(name string) (desk.Item, error) {
	s.mu.Lock()
	snap := s.snap
	s.mu.Unlock()
	for attempt := 0; attempt < 2; attempt++ {
		if snap != nil {
			for _, group := range [][]desk.Item{snap.Running, snap.Done, snap.Skipped, snap.Pending} {
				for _, it := range group {
					if it.Name == name {
						return it, nil
					}
				}
			}
		}
		if attempt == 0 {
			var err error
			if snap, err = s.scan(); err != nil {
				return desk.Item{}, err
			}
		}
	}
	return desk.Item{}, desk.ErrNotFound
}

// Load polls the run's events. A batch's Defs come from its manifest (After
// gives the edges); a script is one step, ScriptStep.
func (s *deskSource) Load(ref RunRef, cur *Cursor) (Delta, error) {
	if !desk.ValidName(ref.Name) {
		return Delta{}, errors.New("desk: not an item name: " + clean(ref.Name))
	}
	if cur == nil {
		cur = &Cursor{}
	}
	it, err := s.item(ref.Name)
	if err != nil {
		return Delta{}, cleanErr(err)
	}
	dc, _ := cur.desk.(*deskCursor)
	if dc == nil {
		w, err := s.d.NewWatcher(ref.Name, 0)
		if err != nil {
			return Delta{}, cleanErr(err)
		}
		dc = &deskCursor{w: w}
		cur.desk = dc
	}

	var d Delta
	if it.Kind == desk.KindBatch {
		if dc.manifest == nil {
			dc.manifest, d.Err = s.manifest(ref.Name)
		}
		d.Defs = batchDefs(dc.manifest)
		if rows, err := s.d.BatchStatus(ref.Name); err == nil {
			d.Timing = batchTiming(rows)
		} else if !errors.Is(err, desk.ErrNotFound) && d.Err == nil {
			d.Err = cleanErr(err)
		}
	} else {
		d.Defs = []StepDef{{ID: ScriptStep, Note: clean(it.What)}}
	}

	lines, ws, err := dc.w.Poll()
	d.Live = liveOf(ws)
	if err == nil && d.Live != liveOfItem(it.State) && d.Live != dc.rescannedAt {
		dc.rescannedAt = d.Live // once per state change, not on every poll
		// The scan this item came from is older than the run: it ended or
		// was lost since. Read the item again for its times and exit.
		if fresh, ferr := s.rescanItem(ref.Name); ferr == nil {
			it = fresh
		}
	}
	if it.Kind != desk.KindBatch {
		d.Timing = []StepTiming{scriptTiming(it)}
	}
	if err != nil {
		// Poll reports a failed read as ended; the scan's state is the
		// better guess then.
		d.Live = liveOfItem(it.State)
		if d.Err == nil {
			d.Err = cleanErr(err)
		}
	}
	fileLines := len(lines)
	if ws == desk.WatchLost && fileLines > 0 && strings.HasPrefix(lines[fileLines-1], desk.EventRunLost+" ") {
		fileLines-- // Poll adds RUN-LOST itself; it was never in the file
	}
	cur.Offset = int64(dc.w.Seen())

	script := it.Kind != desk.KindBatch
	for i, line := range lines {
		if i == fileLines {
			if cur.lost {
				break // a lost run is reported once
			}
			cur.lost = true
		}
		addEvents(&d, cur, dc, line, script)
	}
	for i, t := range d.Timing {
		if dur, ok := dc.durs[t.Step]; ok && t.Dur == 0 {
			d.Timing[i].Dur = dur
		}
	}
	d.Dropped = cur.dropped
	return d, nil
}

// manifest reads and parses a batch's record. A record that is gone is no
// manifest, not an error: the run still shows its events.
func (s *deskSource) manifest(name string) (*desk.Manifest, error) {
	data, kind, err := s.d.Record(name)
	switch {
	case errors.Is(err, desk.ErrNotFound):
		return nil, nil
	case err != nil:
		return nil, cleanErr(err)
	case kind != desk.KindBatch:
		return nil, nil
	}
	m, err := desk.LoadManifest(data, name)
	if err != nil {
		return nil, cleanErr(err)
	}
	return m, nil
}

// addEvents maps one event line to the Events it stands for: usually one.
// A script's RUN-START is followed by its step's STEP-START, and its RUN-END
// is preceded by its step's STEP-END (or STEP-FAIL), so the one step folds
// like a batch step. A STEP-END whose rc is not 0 is named STEP-FAIL.
func addEvents(d *Delta, cur *Cursor, dc *deskCursor, line string, script bool) {
	p, ok := desk.ParseEvent(line)
	if !ok {
		cur.dropped++
		return
	}
	fields := deskFields(p.Fields)
	e := Event{Name: p.Key, Fields: fields}
	if strings.HasPrefix(p.Key, "STEP-") {
		e.Step = clean(p.Fields["id"])
	}
	failed := p.Fields["rc"] != "0"
	if p.Key == deskStepEnd {
		if failed {
			e.Name = deskStepFail
		}
		if dur, ok := stepDur(p.Fields["dur"]); ok {
			if dc.durs == nil {
				dc.durs = map[string]time.Duration{}
			}
			dc.durs[e.Step] = dur
		}
	}
	var evs []Event
	switch {
	case script && p.Key == desk.EventRunStart:
		evs = []Event{e, {Name: deskStepStart, Step: ScriptStep}}
	case script && p.Key == deskRunEnd:
		end := Event{Name: deskStepEnd, Step: ScriptStep, Fields: fieldsNamed(fields, "rc", "reason")}
		if failed {
			end.Name = deskStepFail
		}
		evs = []Event{end, e}
	default:
		evs = []Event{e}
	}
	for _, ev := range evs {
		if cur.kept >= maxRunEvents {
			cur.dropped++
			continue
		}
		cur.kept++
		cur.lines++
		ev.Seq = cur.lines
		d.Events = append(d.Events, ev)
	}
}

// deskFields orders an event's fields by key, with msg (which runs to the
// end of the line) last, and cleans each.
func deskFields(m map[string]string) []Field {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b string) int {
		if (a == "msg") != (b == "msg") {
			if a == "msg" {
				return 1
			}
			return -1
		}
		return strings.Compare(a, b)
	})
	out := make([]Field, 0, len(keys))
	for _, k := range keys {
		out = append(out, Field{Key: clean(k), Value: clean(m[k])})
	}
	return out
}

func fieldsNamed(fields []Field, keys ...string) []Field {
	var out []Field
	for _, f := range fields {
		if slices.Contains(keys, f.Key) {
			out = append(out, f)
		}
	}
	return out
}

func liveOf(ws desk.WatchState) LiveState {
	switch ws {
	case desk.WatchRunning:
		return LiveRunning
	case desk.WatchEnded:
		return LiveEnded
	case desk.WatchLost:
		return LiveLost
	case desk.WatchSkipped:
		return LiveSkipped
	}
	return LiveWaiting
}

func liveOfItem(st desk.State) LiveState {
	switch st {
	case desk.StateRunning:
		return LiveRunning
	case desk.StateDone:
		return LiveEnded
	case desk.StateLost:
		return LiveLost
	case desk.StateSkipped:
		return LiveSkipped
	}
	return LiveWaiting
}

// batchDefs turns a manifest into step defs: After gives the edges, and the
// note is the step's command, cleaned.
func batchDefs(m *desk.Manifest) []StepDef {
	if m == nil {
		return nil
	}
	defs := make([]StepDef, 0, len(m.Steps))
	for _, st := range m.Steps {
		def := StepDef{ID: clean(st.ID), Note: clean(st.Command)}
		for _, a := range st.After {
			def.After = append(def.After, clean(a))
		}
		defs = append(defs, def)
	}
	return defs
}

// batchTiming copies a batch's status rows, cleaned, with each step's
// duration.
func batchTiming(rows []desk.StepStatus) []StepTiming {
	out := make([]StepTiming, 0, len(rows))
	for _, r := range rows {
		t := StepTiming{Step: clean(r.ID), State: clean(r.State), Start: r.Start, End: r.End}
		if r.RC != nil {
			rc := *r.RC
			t.RC = &rc
		}
		if !r.Start.IsZero() && !r.End.IsZero() {
			t.Dur = r.End.Sub(r.Start)
		}
		out = append(out, t)
	}
	return out
}

// scriptTiming is a script's one step, from its item.
func scriptTiming(it desk.Item) StepTiming {
	t := StepTiming{Step: ScriptStep, State: clean(string(it.State)), Start: it.Started, End: it.Ended}
	if it.ExitCode != nil {
		rc := *it.ExitCode
		t.RC = &rc
	}
	if !t.Start.IsZero() && !t.End.IsZero() {
		t.Dur = t.End.Sub(t.Start)
	}
	return t
}

// stepDur reads a STEP-END dur= value, seconds with a fraction.
func stepDur(v string) (time.Duration, bool) {
	secs, err := strconv.ParseFloat(v, 64)
	if err != nil || math.IsNaN(secs) || secs < 0 || secs > 1e9 {
		return 0, false
	}
	return time.Duration(secs * float64(time.Second)), true
}

// rescanItem scans the desk again and returns name as the new scan sees it.
func (s *deskSource) rescanItem(name string) (desk.Item, error) {
	if _, err := s.scan(); err != nil {
		return desk.Item{}, err
	}
	return s.item(name)
}
