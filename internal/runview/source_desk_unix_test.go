// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

//go:build unix

package runview

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/desk"
	"github.com/cameronsjo/forgectl/internal/termsafe/termsafetest"
)

// TestMain gives the package a scratch HOME and TMPDIR: the desk tests run
// batch steps, and nothing they do may land in the real home directory.
func TestMain(m *testing.M) {
	os.Exit(runWithScratchHome(m))
}

func runWithScratchHome(m *testing.M) int {
	scratch, err := os.MkdirTemp("", "runview-test-home-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "scratch home:", err)
		return 2
	}
	defer os.RemoveAll(scratch) //nolint:errcheck // best effort
	home, tmp := filepath.Join(scratch, "home"), filepath.Join(scratch, "tmp")
	for _, dir := range []string{home, tmp} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			fmt.Fprintln(os.Stderr, "scratch home:", err)
			return 2
		}
	}
	for k, v := range map[string]string{"HOME": home, "TMPDIR": tmp} {
		if err := os.Setenv(k, v); err != nil {
			fmt.Fprintln(os.Stderr, "scratch home:", err)
			return 2
		}
	}
	return m.Run()
}

func openDesk(t *testing.T) *desk.Desk {
	t.Helper()
	d, err := desk.Open(filepath.Join(t.TempDir(), "desk"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func writeFile(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// addAndClaim queues src and claims it, as the desk does on approval.
func addAndClaim(t *testing.T, d *desk.Desk, src string) *desk.Claimed {
	t.Helper()
	a, err := d.Add(src, "build the thing", "the next stage needs it", false)
	if err != nil {
		t.Fatal(err)
	}
	c, err := d.Claim(a.Name, a.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func loadDesk(t *testing.T, src Source, name string, cur *Cursor) Delta {
	t.Helper()
	d, err := src.Load(RunRef{Source: src.Name(), Name: name, Kind: KindDesk}, cur)
	if err != nil {
		t.Fatalf("Load(%s): %v", name, err)
	}
	return d
}

func timingOf(d Delta, step string) StepTiming {
	for _, tm := range d.Timing {
		if tm.Step == step {
			return tm
		}
	}
	return StepTiming{}
}

// doneItem writes a finished script item straight into done/, with its events.
func doneItem(t *testing.T, d *desk.Desk, name, events string) {
	t.Helper()
	done := filepath.Join(d.Path(), "done")
	writeFile(t, filepath.Join(done, name+".sh"), "#!/bin/bash\n")
	writeFile(t, filepath.Join(done, name+".log"), "EXIT=0\n")
	writeFile(t, filepath.Join(done, name+".events"), events)
}

// A real batch, run by the desk's own runner in t.TempDir(): the manifest
// gives the DAG, the events give each step's state, and status.tsv the
// times. One step fails, so one dependent is skipped.
func TestDeskBatchGivesTheDAGAndStates(t *testing.T) {
	d := openDesk(t)
	src := filepath.Join(t.TempDir(), "pipeline.manifest")
	writeFile(t, src, strings.Join([]string{
		"fetch -- true",
		"build after=fetch -- exit 3",
		"stage after=build -- true",
		"check after=fetch -- sleep 0.2",
	}, "\n")+"\n")
	c := addAndClaim(t, d, src)
	run, err := d.BeginRun(c.Name, os.Getpid(), "steps=4", "jobs=2")
	if err != nil {
		t.Fatal(err)
	}

	s := NewDeskSource(d)
	refs, err := s.List()
	if err != nil || len(refs) != 1 || refs[0].Name != c.Name || refs[0].Kind != KindDesk || refs[0].Live != LiveRunning {
		t.Fatalf("List = %+v, %v", refs, err)
	}
	cur := &Cursor{}
	before := loadDesk(t, s, c.Name, cur)
	if before.Live != LiveRunning || names(before.Events) != "[1:RUN-START]" {
		t.Fatalf("before the batch: live %s, events %v", before.Live, names(before.Events))
	}

	dir := filepath.Join(d.Path(), "done", c.Name+".d")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	logF, err := os.OpenFile(filepath.Join(d.Path(), "done", c.Name+".log"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	m, err := desk.LoadManifest(c.Content, c.Name+".manifest")
	if err != nil {
		t.Fatal(err)
	}
	res := desk.NewBatch(m, desk.BatchOptions{
		ID: c.Name, Dir: dir, Jobs: 2, Log: logF,
		Event: func(line string) { _ = run.Events.Emit(line) },
	}).Run()
	_ = logF.Close()
	if err := run.Finish(res.RC, res.Reason, res.Fields()...); err != nil {
		t.Fatal(err)
	}

	after := loadDesk(t, s, c.Name, cur)
	if after.Reset || after.Live != LiveEnded {
		t.Fatalf("after: reset %v, live %s", after.Reset, after.Live)
	}
	wantDefs := []StepDef{
		{ID: "fetch", Note: "true"},
		{ID: "build", Note: "exit 3", After: []string{"fetch"}},
		{ID: "stage", Note: "true", After: []string{"build"}},
		{ID: "check", Note: "sleep 0.2", After: []string{"fetch"}},
	}
	if !reflect.DeepEqual(after.Defs, wantDefs) {
		t.Fatalf("defs = %+v, want %+v", after.Defs, wantDefs)
	}

	events := append(before.Events, after.Events...)
	for i, e := range events {
		if e.Seq != i+1 {
			t.Fatalf("Seq not continuous across loads: %v", names(events))
		}
	}
	st := Fold(DeskSpec(), after.Defs, events)
	wantEdges := [][2]string{{"fetch", "build"}, {"build", "stage"}, {"fetch", "check"}}
	if !reflect.DeepEqual(st.Edges, wantEdges) {
		t.Errorf("edges = %v, want %v", st.Edges, wantEdges)
	}
	status := map[string]StepStatus{}
	for _, sst := range st.Steps {
		status[sst.ID] = sst.Status
	}
	wantStatus := map[string]StepStatus{"fetch": StepClosed, "build": StepFailed, "stage": StepSkipped, "check": StepClosed}
	if !reflect.DeepEqual(status, wantStatus) {
		t.Errorf("status = %v, want %v (events %v)", status, wantStatus, names(events))
	}
	if st.Exit == nil || *st.Exit != 1 {
		t.Errorf("exit = %v, want 1", st.Exit)
	}
	if tm := timingOf(after, "stage"); tm.State != "skipped" {
		t.Errorf("stage timing = %+v, want skipped", tm)
	}
	if tm := timingOf(after, "check"); tm.State != "ok" || tm.Dur < 150*time.Millisecond || tm.RC == nil || *tm.RC != 0 {
		t.Errorf("check timing = %+v", tm)
	}
	if tm := timingOf(after, "build"); tm.State != "failed" || tm.RC == nil || *tm.RC != 3 {
		t.Errorf("build timing = %+v", tm)
	}
	// A skipped step reads skipped through the legend.
	if m := StepMark(IconGlyphs, step(t, st, "stage").Status, RunnerState(after.Timing, "stage")); m.Word != "skipped" {
		t.Errorf("stage mark = %+v", m)
	}
}

func TestDeskScriptIsOneStep(t *testing.T) {
	d := openDesk(t)
	src := filepath.Join(t.TempDir(), "tidy.sh")
	writeFile(t, src, "#!/bin/bash\ntrue\n")
	c := addAndClaim(t, d, src)
	run, err := d.BeginRun(c.Name, os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Finish(4, "failed"); err != nil {
		t.Fatal(err)
	}
	s := NewDeskSource(d)
	if _, err := s.List(); err != nil {
		t.Fatal(err)
	}
	dl := loadDesk(t, s, c.Name, &Cursor{})
	if names(dl.Events) != "[1:RUN-START 2:STEP-START 3:STEP-FAIL 4:RUN-END]" {
		t.Fatalf("events = %v", names(dl.Events))
	}
	if !reflect.DeepEqual(dl.Defs, []StepDef{{ID: ScriptStep, Note: "build the thing"}}) {
		t.Errorf("defs = %+v", dl.Defs)
	}
	st := Fold(DeskSpec(), dl.Defs, dl.Events)
	if len(st.Steps) != 1 || st.Steps[0].Status != StepFailed || st.Exit == nil || *st.Exit != 4 || st.Live != LiveEnded {
		t.Errorf("fold = %+v exit %v live %s", st.Steps, st.Exit, st.Live)
	}
	if tm := timingOf(dl, ScriptStep); tm.RC == nil || *tm.RC != 4 || tm.Start.IsZero() || tm.End.IsZero() {
		t.Errorf("timing = %+v", tm)
	}
}

func TestDeskScriptThatSucceedsClosesItsStep(t *testing.T) {
	d := openDesk(t)
	src := filepath.Join(t.TempDir(), "ok.sh")
	writeFile(t, src, "#!/bin/bash\ntrue\n")
	c := addAndClaim(t, d, src)
	run, err := d.BeginRun(c.Name, os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	s := NewDeskSource(d)
	cur := &Cursor{}
	if _, err := s.List(); err != nil {
		t.Fatal(err)
	}
	// While it runs: the step is running, the run live-by-desk.
	running := loadDesk(t, s, c.Name, cur)
	st := Fold(DeskSpec(), running.Defs, running.Events)
	if running.Live != LiveRunning || names(running.Events) != "[1:RUN-START 2:STEP-START]" || st.Steps[0].Status != StepRunning {
		t.Fatalf("running: live %s events %v step %+v", running.Live, names(running.Events), st.Steps)
	}
	if err := run.Finish(0, "ok"); err != nil {
		t.Fatal(err)
	}
	dl := loadDesk(t, s, c.Name, cur)
	if names(dl.Events) != "[3:STEP-END 4:RUN-END]" {
		t.Fatalf("events = %v; want Seq to continue at 3", names(dl.Events))
	}
	all := append(running.Events, dl.Events...)
	st = Fold(DeskSpec(), dl.Defs, all)
	if st.Steps[0].Status != StepClosed || st.Exit == nil || *st.Exit != 0 {
		t.Errorf("fold = %+v exit %v", st.Steps, st.Exit)
	}
}

// STEP-SKIP skips a step that never started, and Timing carries the runner's
// word for it.
func TestDeskBatchStepSkipSkipsThePendingStep(t *testing.T) {
	d := openDesk(t)
	name := "05-skip"
	done := filepath.Join(d.Path(), "done")
	writeFile(t, filepath.Join(done, name+".manifest"), "fetch -- true\nbuild after=fetch -- true\n")
	writeFile(t, filepath.Join(done, name+".log"), "EXIT=1\n")
	writeFile(t, filepath.Join(done, name+".d", "status.tsv"),
		"step\tstate\trc\tstart\tend\tdeps\nfetch\tfailed\t1\t-\t-\t-\nbuild\tskipped\t-\t-\t-\tfetch\n")
	writeFile(t, filepath.Join(done, name+".events"), strings.Join([]string{
		"RUN-START id=05-skip pid=1 steps=2",
		"STEP-START id=fetch deps=",
		"STEP-END id=fetch rc=1 dur=0.1 reason=failed outputs= log=private",
		"STEP-SKIP id=build reason=dependency",
		"RUN-END rc=1 reason=failed",
	}, "\n")+"\n")
	s := NewDeskSource(d)
	if _, err := s.List(); err != nil {
		t.Fatal(err)
	}
	dl := loadDesk(t, s, name, &Cursor{})
	if got := names(dl.Events); got != "[1:RUN-START 2:STEP-START 3:STEP-FAIL 4:STEP-SKIP 5:RUN-END]" {
		t.Fatalf("events = %v", got)
	}
	st := Fold(DeskSpec(), dl.Defs, dl.Events)
	if step(t, st, "fetch").Status != StepFailed || step(t, st, "build").Status != StepSkipped {
		t.Errorf("steps = %+v", st.Steps)
	}
	if tm := timingOf(dl, "build"); tm.State != "skipped" || tm.RC != nil {
		t.Errorf("build timing = %+v", tm)
	}
	if tm := timingOf(dl, "fetch"); tm.State != "failed" || tm.RC == nil || *tm.RC != 1 {
		t.Errorf("fetch timing = %+v", tm)
	}
	if m := StepMark(IconGlyphs, step(t, st, "build").Status, RunnerState(dl.Timing, "build")); m.Word != "skipped" {
		t.Errorf("build mark = %+v; want skipped", m)
	}
}

// A STEP-END dur= fills a timing that the status file has not ended yet.
func TestDeskStepEndDurFillsAnOpenTiming(t *testing.T) {
	d := openDesk(t)
	name := "06-dur"
	done := filepath.Join(d.Path(), "done")
	writeFile(t, filepath.Join(done, name+".manifest"), "fetch -- true\n")
	writeFile(t, filepath.Join(done, name+".log"), "EXIT=0\n")
	writeFile(t, filepath.Join(done, name+".d", "status.tsv"), "step\tstate\trc\tstart\tend\tdeps\nfetch\trunning\t-\t-\t-\t-\n")
	writeFile(t, filepath.Join(done, name+".events"),
		"RUN-START id=06-dur pid=1\nSTEP-START id=fetch deps=\nSTEP-END id=fetch rc=0 dur=1.5 reason=ok outputs= log=private\nRUN-END rc=0 reason=ok\n")
	s := NewDeskSource(d)
	if _, err := s.List(); err != nil {
		t.Fatal(err)
	}
	dl := loadDesk(t, s, name, &Cursor{})
	if tm := timingOf(dl, "fetch"); tm.Dur != 1500*time.Millisecond {
		t.Errorf("fetch timing = %+v, want Dur 1.5s from the STEP-END", tm)
	}
}

// A manifest the loader refuses is reported in Delta.Err, and the run still
// shows its events.
func TestDeskBatchWithABadManifestStillShowsItsEvents(t *testing.T) {
	d := openDesk(t)
	name := "04-badmanifest"
	done := filepath.Join(d.Path(), "done")
	writeFile(t, filepath.Join(done, name+".manifest"), "build after=nothere -- true\n")
	writeFile(t, filepath.Join(done, name+".log"), "EXIT=0\n")
	writeFile(t, filepath.Join(done, name+".events"), "RUN-START id=04-badmanifest pid=1\nRUN-END rc=0 reason=ok\n")
	s := NewDeskSource(d)
	if _, err := s.List(); err != nil {
		t.Fatal(err)
	}
	dl := loadDesk(t, s, name, &Cursor{})
	if dl.Err == nil || len(dl.Defs) != 0 || names(dl.Events) != "[1:RUN-START 2:RUN-END]" {
		t.Fatalf("err %v defs %+v events %v; want the error, no defs and the events", dl.Err, dl.Defs, names(dl.Events))
	}
	termsafetest.AssertInert(t, "err", dl.Err.Error())
}

func TestDeskListSkipsLegacyAndPendingItems(t *testing.T) {
	d := openDesk(t)
	done := filepath.Join(d.Path(), "done")
	doneItem(t, d, "07-real", "RUN-START id=07-real pid=1\nRUN-END rc=0 reason=ok\n")
	writeFile(t, filepath.Join(done, "old-run.log"), "EXIT=0\n") // no NN- number: a legacy log
	src := filepath.Join(t.TempDir(), "queued.sh")
	writeFile(t, src, "#!/bin/bash\ntrue\n")
	if _, err := d.Add(src, "queued", "later", false); err != nil {
		t.Fatal(err)
	}
	s := NewDeskSource(d)
	refs, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 || refs[0].Name != "07-real" || refs[0].Live != LiveEnded {
		t.Fatalf("List = %+v; want only the done item", refs)
	}
	// A legacy name is not loadable.
	if _, err := s.Load(RunRef{Name: "old-run", Kind: KindDesk}, &Cursor{}); err == nil {
		t.Errorf("Load of a legacy name succeeded")
	}
}

func TestDeskListIncludesSkippedItems(t *testing.T) {
	d := openDesk(t)
	src := filepath.Join(t.TempDir(), "gone.sh")
	writeFile(t, src, "#!/bin/bash\ntrue\n")
	a, err := d.Add(src, "gone", "never", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Skip(a.Name, "not needed"); err != nil {
		t.Fatal(err)
	}
	s := NewDeskSource(d)
	refs, err := s.List()
	if err != nil || len(refs) != 1 || refs[0].Live != LiveSkipped || refs[0].Updated.IsZero() {
		t.Fatalf("List = %+v, %v", refs, err)
	}
	dl := loadDesk(t, s, a.Name, &Cursor{})
	if dl.Live != LiveSkipped || len(dl.Events) != 0 {
		t.Errorf("skipped run: live %s events %v", dl.Live, names(dl.Events))
	}
}

func TestDeskLoadRefusesABadNameAndAnUnknownItem(t *testing.T) {
	d := openDesk(t)
	s := NewDeskSource(d)
	if _, err := s.Load(RunRef{Name: "../x", Kind: KindDesk}, &Cursor{}); err == nil {
		t.Errorf("Load(../x) succeeded")
	}
	if _, err := s.Load(RunRef{Name: "99-nothing", Kind: KindDesk}, &Cursor{}); !errors.Is(err, desk.ErrNotFound) {
		t.Errorf("Load(99-nothing) err = %v; want ErrNotFound", err)
	}
}

// Poll adds RUN-LOST on every poll of a lost run; the source delivers it once.
func TestDeskRunLostIsDeliveredOnce(t *testing.T) {
	d := openDesk(t)
	name := "08-x"
	done := filepath.Join(d.Path(), "done")
	writeFile(t, filepath.Join(done, name+".sh"), "#!/bin/bash\n")
	writeFile(t, filepath.Join(done, name+".log"), "")
	writeFile(t, filepath.Join(done, name+".events"), "RUN-START id=08-x pid=1\n")
	s := NewDeskSource(d)
	cur := &Cursor{}
	first := loadDesk(t, s, name, cur)
	if first.Live != LiveLost || names(first.Events) != "[1:RUN-START 2:STEP-START 3:RUN-LOST]" {
		t.Fatalf("first: live %s, events %v", first.Live, names(first.Events))
	}
	for range 2 {
		if again := loadDesk(t, s, name, cur); again.Live != LiveLost || len(again.Events) != 0 || again.Reset {
			t.Fatalf("again: live %s, events %v, reset %v", again.Live, names(again.Events), again.Reset)
		}
	}
	// RUN-LOST folds: the run reads lost and its step interrupted, not running
	// (#1106), so a replay agrees with the delta's Live.
	st := Fold(DeskSpec(), first.Defs, first.Events)
	if st.Live != LiveLost || st.Steps[0].Status != StepInterrupted {
		t.Errorf("fold of a lost run = %+v", st)
	}
}

// A finished run reads ended on every Load, not lost on the second.
func TestDeskFinishedRunStaysEnded(t *testing.T) {
	d := openDesk(t)
	name := "09-x"
	doneItem(t, d, name, "RUN-START id=09-x pid=1\nRUN-END rc=0 reason=ok\n")
	s := NewDeskSource(d)
	cur := &Cursor{}
	if dl := loadDesk(t, s, name, cur); dl.Live != LiveEnded || len(dl.Events) != 4 {
		t.Fatalf("first load: live %s, events %v", dl.Live, names(dl.Events))
	}
	// The source keeps one watcher per run; a finished run polled again
	// stays ended (desk.Watcher remembers its RUN-END).
	for i := 2; i <= 3; i++ {
		dl := loadDesk(t, s, name, cur)
		if dl.Live != LiveEnded || len(dl.Events) != 0 {
			t.Fatalf("load %d: live %s, events %v", i, dl.Live, names(dl.Events))
		}
	}
}

func TestDeskUnparseableEventLinesAreCounted(t *testing.T) {
	d := openDesk(t)
	name := "11-x"
	doneItem(t, d, name, "RUN-START id=11-x pid=1\nNOT-AN-EVENT x=1\nRUN-END rc=0 reason=ok\n")
	s := NewDeskSource(d)
	dl := loadDesk(t, s, name, &Cursor{})
	if dl.Dropped != 1 || names(dl.Events) != "[1:RUN-START 2:STEP-START 3:STEP-END 4:RUN-END]" {
		t.Fatalf("dropped %d events %v", dl.Dropped, names(dl.Events))
	}
}

func TestDeskFieldsAreOrderedByKeyWithMsgLast(t *testing.T) {
	d := openDesk(t)
	name := "12-x"
	doneItem(t, d, name, "RUN-START id=12-x pid=1\nSTEP-WARN id=script zzz=1 aaa=2 msg=hello there\nRUN-END rc=0 reason=ok\n")
	dl := loadDesk(t, NewDeskSource(d), name, &Cursor{})
	var warn Event
	for _, e := range dl.Events {
		if e.Name == "STEP-WARN" {
			warn = e
		}
	}
	if warn.Step != ScriptStep {
		t.Fatalf("no STEP-WARN with the script step in %v", names(dl.Events))
	}
	var keys []string
	for _, f := range warn.Fields {
		keys = append(keys, f.Key)
	}
	if got := strings.Join(keys, ","); got != "aaa,id,zzz,msg" {
		t.Errorf("field order = %s; want key order with msg last", got)
	}
}

func TestStepDurRefusesBadValues(t *testing.T) {
	for _, v := range []string{"NaN", "-1", "1e10", "x", "", "Inf"} {
		if d, ok := stepDur(v); ok {
			t.Errorf("stepDur(%q) = %v, ok", v, d)
		}
	}
	if d, ok := stepDur("1.5"); !ok || d != 1500*time.Millisecond {
		t.Errorf("stepDur(1.5) = %v, %v", d, ok)
	}
	if d, ok := stepDur("0"); !ok || d != 0 {
		t.Errorf("stepDur(0) = %v, %v", d, ok)
	}
}

// Hostile text in a manifest, a status row, an event line and a skip reason
// reaches no Delta string as a control.
func TestHostileDeskTextIsInert(t *testing.T) {
	d := openDesk(t)
	h := termsafetest.Hostile("x")
	done := filepath.Join(d.Path(), "done")
	name := "03-batch"
	writeFile(t, filepath.Join(done, name+".manifest"), "fetch -- echo "+h+"\n")
	writeFile(t, filepath.Join(done, name+".log"), "EXIT=1\n")
	writeFile(t, filepath.Join(done, name+".d", "status.tsv"), "step\tstate\trc\tstart\tend\tdeps\n"+h+"\t"+h+"\t1\t-\t-\t-\n")
	writeFile(t, filepath.Join(done, name+".events"), strings.Join([]string{
		"RUN-START id=03-batch pid=1",
		"STEP-START id=" + h + " deps=",
		"STEP-WARN id=fetch msg=" + h + " more " + h,
		"STEP-END id=fetch rc=1 dur=0.1 reason=" + h + " outputs= log=private",
		"RUN-END rc=1 reason=failed",
	}, "\n")+"\n")
	s := NewDeskSource(d)
	refs, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range refs {
		termsafetest.AssertInert(t, "ref name", r.Name)
	}
	dl := loadDesk(t, s, name, &Cursor{})
	if len(dl.Events) != 5 || len(dl.Defs) != 1 {
		t.Fatalf("delta: events %d defs %d", len(dl.Events), len(dl.Defs))
	}
	assertDeltaInert(t, dl)

	// A hostile event value arriving through a live run's own event log.
	src := filepath.Join(t.TempDir(), "x.sh")
	writeFile(t, src, "#!/bin/bash\ntrue\n")
	c := addAndClaim(t, d, src)
	run, err := d.BeginRun(c.Name, os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Events.Emit("STEP-WARN id=script msg=\x1b[2Jwiped\x1b]0;title\a"); err != nil {
		t.Fatal(err)
	}
	live := loadDesk(t, s, c.Name, &Cursor{})
	var saw bool
	for _, e := range live.Events {
		if e.Name == "STEP-WARN" {
			saw = true
		}
	}
	if !saw {
		t.Fatalf("the emitted warning did not arrive: %v", names(live.Events))
	}
	assertDeltaInert(t, live)
	for _, e := range live.Events {
		for _, f := range e.Fields {
			if strings.ContainsRune(f.Value, 0x1b) || strings.ContainsRune(f.Value, 0x07) {
				t.Errorf("control byte survived in %s.%s = %q", e.Name, f.Key, f.Value)
			}
		}
	}
}

// An item the desk skipped because it changed lists and loads as changed,
// the word the queue and the outcomes use, not skipped (#1106).
func TestDeskChangedItemReadsChanged(t *testing.T) {
	d := openDesk(t)
	src := filepath.Join(t.TempDir(), "s2.sh")
	writeFile(t, src, "#!/bin/bash\necho s2\n")
	a, err := d.Add(src, "print", "a test", false)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(d.Path(), desk.DirPending, a.Name+".sh"), "#!/bin/bash\necho s2\n# appended\n")
	s := NewDeskSource(d)
	refs, err := s.List() // the scan moves it to skipped/ as changed
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 || refs[0].Live != LiveChanged {
		t.Fatalf("List = %+v; want one changed run", refs)
	}
	if dl := loadDesk(t, s, a.Name, &Cursor{}); dl.Live != LiveChanged {
		t.Errorf("Load Live = %s, want changed", dl.Live)
	}
	if mk := RunMark(IconGlyphs, LiveChanged, nil); mk.Word != "changed" || mk.Glyph != "!" {
		t.Errorf("RunMark(changed) = %+v", mk)
	}
}
