// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

//go:build unix

package runview

import (
	"errors"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cameronsjo/forgectl/internal/desk"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// deskSource reads a desk: its items are the queue, and every item that is
// running, done or skipped is a run.
type deskSource struct {
	d *desk.Desk

	mu   sync.Mutex
	snap *desk.Snapshot // the latest scan; Load reads kinds and items from it
}

// deskCursor is a desk run's read state, kept in Cursor.desk.
type deskCursor struct {
	w        *desk.Watcher
	manifest *desk.Manifest
	dropped  int                      // Watcher.Dropped as of the last Load
	durs     map[string]time.Duration // step -> STEP-END dur=, for a step the status file has not ended
}

// NewDeskSource returns the source for d. Scan, which List and Queue call,
// changes the queue: it fixes the hash of a hand-dropped item and moves a
// changed one to skipped/.
func NewDeskSource(d *desk.Desk) Source { return &deskSource{d: d} }

func (s *deskSource) Name() string { return "desk" }

// DeskProvider is a Source backed by a desk, which the dashboard's queue
// verbs act on.
type DeskProvider interface {
	Desk() *desk.Desk
}

// Desk is the desk the source reads.
func (s *deskSource) Desk() *desk.Desk { return s.d }

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
// A pending item is not a run yet; Queue has it.
func (s *deskSource) List() ([]RunRef, error) {
	snap, err := s.scan()
	if err != nil {
		return nil, err
	}
	var refs []RunRef
	for _, group := range [][]desk.Item{snap.Running, snap.Done, snap.Skipped} {
		for _, it := range group {
			refs = append(refs, RunRef{Source: s.Name(), Name: clean(it.Name), Kind: KindDesk, Updated: itemUpdated(it, snap.Taken)})
		}
	}
	slices.SortFunc(refs, func(a, b RunRef) int {
		return newestFirst(match{a.Name, a.Updated}, match{b.Name, b.Updated})
	})
	return refs, nil
}

func itemUpdated(it desk.Item, fallback time.Time) time.Time {
	switch {
	case !it.Ended.IsZero():
		return it.Ended
	case !it.Started.IsZero():
		return it.Started
	case it.Meta.AddedAt != nil:
		return *it.Meta.AddedAt
	}
	return fallback
}

// Queue scans the desk and returns its pending, running and skipped items.
func (s *deskSource) Queue() ([]QueueItem, error) {
	snap, err := s.scan()
	if err != nil {
		return nil, err
	}
	return queueItems(snap), nil
}

func queueItems(snap *desk.Snapshot) []QueueItem {
	out := []QueueItem{}
	for _, group := range [][]desk.Item{snap.Pending, snap.Running, snap.Skipped} {
		for _, it := range group {
			out = append(out, queueItem(it))
		}
	}
	return out
}

// queueItem is the sanitized copy of it. SHA256 is set only when the hash
// recorded in meta is the hash of the bytes this scan read; otherwise it is
// empty and Refusal says the item cannot be approved.
func queueItem(it desk.Item) QueueItem {
	q := QueueItem{
		Name: clean(it.Name), Kind: clean(string(it.Kind)), State: clean(string(it.State)),
		What: clean(it.What), Why: clean(it.Why), SkipReason: clean(it.Meta.SkipReason),
		TTY: it.TTY, Stale: it.Stale, Refusal: clean(it.Refusal),
	}
	if it.ExitCode != nil {
		rc := *it.ExitCode
		q.Exit = &rc
	}
	if it.Content != nil {
		if sum := desk.SHA256Hex(it.Content); it.Meta.SHA256 == sum {
			q.SHA256 = sum
		} else if q.Refusal == "" {
			q.Refusal = "its bytes do not match the hash recorded when it was queued"
		}
		q.Bytes = len(it.Content)
		body := strings.TrimSuffix(string(it.Content), "\n")
		for _, line := range strings.Split(body, "\n") {
			q.Body = append(q.Body, termsafe.SafeLine(line))
			q.Marks = append(q.Marks, MarkLine(line))
		}
	}
	return q
}

// item finds name in the latest scan, scanning again when it is not there.
func (s *deskSource) item(name string) (desk.Item, *desk.Snapshot, error) {
	s.mu.Lock()
	snap := s.snap
	s.mu.Unlock()
	for attempt := 0; attempt < 2; attempt++ {
		if snap != nil {
			for _, group := range [][]desk.Item{snap.Running, snap.Done, snap.Skipped, snap.Pending} {
				for _, it := range group {
					if it.Name == name {
						return it, snap, nil
					}
				}
			}
		}
		if attempt == 0 {
			var err error
			if snap, err = s.scan(); err != nil {
				return desk.Item{}, nil, err
			}
		}
	}
	return desk.Item{}, snap, desk.ErrNotFound
}

// Load polls the run's events. A batch's Defs come from its manifest (After
// gives the edges), a script is one step, ScriptStep. Items is the queue as
// of the latest List or Queue.
func (s *deskSource) Load(ref RunRef, cur *Cursor) (Delta, error) {
	if !desk.ValidName(ref.Name) {
		return Delta{}, errors.New("desk: not an item name: " + clean(ref.Name))
	}
	if cur == nil {
		cur = &Cursor{}
	}
	it, snap, err := s.item(ref.Name)
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

	d := Delta{Items: queueItems(snap)}
	if it.Kind == desk.KindBatch {
		if dc.manifest == nil {
			if m, err := s.d.Manifest(ref.Name); err == nil {
				dc.manifest = m
			} else if !errors.Is(err, desk.ErrNotFound) {
				d.Err = cleanErr(err)
			}
		}
		d.Defs = batchDefs(dc.manifest)
		if rows, err := s.d.BatchStatus(ref.Name); err == nil {
			d.Timing = batchTiming(rows)
		} else if !errors.Is(err, desk.ErrNotFound) && d.Err == nil {
			d.Err = cleanErr(err)
		}
	} else {
		d.Defs = []StepDef{{ID: ScriptStep, Note: clean(it.What)}}
		d.Timing = []StepTiming{scriptTiming(it)}
	}

	prevSeen := int(cur.Offset)
	lines, ws, err := dc.w.Poll()
	d.Live = liveOf(ws)
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
	newDropped := dc.w.Dropped() - dc.dropped
	dc.dropped = dc.w.Dropped()
	if dc.w.Seen() != prevSeen+fileLines+newDropped {
		// The watcher started over on a replaced or shrunk events file, so
		// its count no longer continues the cursor's.
		keep := cur.desk
		*cur = Cursor{desk: keep}
		dc.durs = nil
		d.Reset = true
	}
	cur.Offset = int64(dc.w.Seen())
	cur.dropped += newDropped

	script := it.Kind != desk.KindBatch
	for i, line := range lines {
		if i == fileLines {
			if cur.lost {
				break
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
func batchTiming(rows []desk.StatusRow) []StepTiming {
	out := make([]StepTiming, 0, len(rows))
	for _, r := range rows {
		t := StepTiming{Step: clean(r.Step), State: clean(r.State), Start: r.Start, End: r.End}
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
	if err != nil || secs < 0 || secs > 1e9 {
		return 0, false
	}
	return time.Duration(secs * float64(time.Second)), true
}
