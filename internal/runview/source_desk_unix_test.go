// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

//go:build unix

package runview

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
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

// A real batch, run by the desk's own runner in t.TempDir(): the manifest
// gives the DAG, the events give each step's state, and status.tsv the
// times. One step fails, so one dependent is skipped.
func TestADeskBatchGivesTheDAGAndStates(t *testing.T) {
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
	if err != nil || len(refs) != 1 || refs[0].Name != c.Name || refs[0].Kind != KindDesk {
		t.Fatalf("List = %+v, %v", refs, err)
	}
	cur := &Cursor{}
	before := loadDesk(t, s, c.Name, cur)
	if before.Live != LiveRunning || fmt.Sprint(names(before.Events)) != "[1:RUN-START]" {
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
		ID: c.Name, Dir: dir, EventDir: "done/" + c.Name + ".d", Jobs: 2,
		WorkDir: t.TempDir(), Log: logF,
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
	st := Fold(DeskSpec(), after.Defs, events, time.Now())
	wantEdges := [][2]string{{"fetch", "build"}, {"build", "stage"}, {"fetch", "check"}}
	if !reflect.DeepEqual(st.Edges, wantEdges) {
		t.Errorf("edges = %v, want %v", st.Edges, wantEdges)
	}
	status := map[string]StepStatus{}
	for _, sst := range st.Steps {
		status[sst.ID] = sst.Status
	}
	wantStatus := map[string]StepStatus{"fetch": StepClosed, "build": StepFailed, "stage": StepPending, "check": StepClosed}
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
}

func TestADeskScriptIsOneStep(t *testing.T) {
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
	if fmt.Sprint(names(dl.Events)) != "[1:RUN-START 2:STEP-START 3:STEP-FAIL 4:RUN-END]" {
		t.Fatalf("events = %v", names(dl.Events))
	}
	if !reflect.DeepEqual(dl.Defs, []StepDef{{ID: ScriptStep, Note: "build the thing"}}) {
		t.Errorf("defs = %+v", dl.Defs)
	}
	st := Fold(DeskSpec(), dl.Defs, dl.Events, time.Now())
	if len(st.Steps) != 1 || st.Steps[0].Status != StepFailed || st.Exit == nil || *st.Exit != 4 {
		t.Errorf("fold = %+v exit %v", st.Steps, st.Exit)
	}
	if tm := timingOf(dl, ScriptStep); tm.RC == nil || *tm.RC != 4 || tm.Start.IsZero() || tm.End.IsZero() {
		t.Errorf("timing = %+v", tm)
	}
}

// The watcher starts over on a replaced events file, and the source says so.
func TestADeskEventsFileReplacedResets(t *testing.T) {
	d := openDesk(t)
	name := "07-x"
	done := filepath.Join(d.Path(), "done")
	writeFile(t, filepath.Join(done, name+".sh"), "#!/bin/bash\n")
	writeFile(t, filepath.Join(done, name+".log"), "EXIT=0\n")
	writeFile(t, filepath.Join(done, name+".events"), "RUN-START id=07-x pid=1\nRUN-END rc=0 reason=ok\n")
	s := NewDeskSource(d)
	if _, err := s.List(); err != nil {
		t.Fatal(err)
	}
	cur := &Cursor{}
	if dl := loadDesk(t, s, name, cur); len(dl.Events) != 4 || dl.Reset || dl.Live != LiveEnded {
		t.Fatalf("first: %v reset %v", names(dl.Events), dl.Reset)
	}
	writeFile(t, filepath.Join(done, "new"), "RUN-START id=07-x pid=2\nSTEP-WARN id=script msg=again\nRUN-END rc=0 reason=ok\n")
	if err := os.Rename(filepath.Join(done, "new"), filepath.Join(done, name+".events")); err != nil {
		t.Fatal(err)
	}
	dl := loadDesk(t, s, name, cur)
	if !dl.Reset || len(dl.Events) != 5 || dl.Events[0].Seq != 1 {
		t.Fatalf("after replace: reset %v, events %v", dl.Reset, names(dl.Events))
	}
}

// queueItem copies a hash only when it is the hash of the bytes shown.
func TestAQueueItemWhoseHashDoesNotMatchCannotBeApproved(t *testing.T) {
	content := []byte("#!/bin/bash\n# WHAT: w\n# WHY: y\ntrue\n")
	good := queueItem(desk.Item{Name: "01-a", State: desk.StateWaiting, Content: content, Meta: desk.Meta{SHA256: desk.SHA256Hex(content)}})
	if good.SHA256 != desk.SHA256Hex(content) || good.Refusal != "" || len(good.Body) != 4 {
		t.Errorf("matching item: %+v", good)
	}
	bad := queueItem(desk.Item{Name: "01-a", State: desk.StateWaiting, Content: content, Meta: desk.Meta{SHA256: strings.Repeat("0", 64)}})
	if bad.SHA256 != "" || bad.Refusal == "" {
		t.Errorf("mismatched item: SHA256 %q, refusal %q; want empty and a reason", bad.SHA256, bad.Refusal)
	}
}

// The approve modal's counts come from the raw bytes: an invisible rune that
// SafeLine draws as is, a look-alike letter, and an escaped control are each
// counted on their own line, and the size is the content's.
func TestAQueueItemMarksTheLinesThatHideSomething(t *testing.T) {
	content := []byte("ls\u3164 -la\nс at\nprintf '\x1b[2J'\nplain\n")
	it := queueItem(desk.Item{Name: "01-a", State: desk.StateWaiting, Content: content, Meta: desk.Meta{SHA256: desk.SHA256Hex(content)}})
	want := []LineMarks{
		{Invisible: 1, NonASCII: 1},
		{NonASCII: 1},
		{Escaped: 1},
		{},
	}
	if it.Bytes != len(content) {
		t.Errorf("Bytes = %d, want %d", it.Bytes, len(content))
	}
	if !slices.Equal(it.Marks, want) {
		t.Errorf("Marks = %+v, want %+v", it.Marks, want)
	}
	if len(it.Marks) != len(it.Body) {
		t.Errorf("%d marks for %d body lines", len(it.Marks), len(it.Body))
	}
	// SafeLine leaves U+3164 as is; only the count flags it.
	if !strings.ContainsRune(it.Body[0], '\u3164') {
		t.Errorf("body line 0 = %q", it.Body[0])
	}
}

func TestADeskSourceProvidesItsDesk(t *testing.T) {
	d := openDesk(t)
	p, ok := NewDeskSource(d).(DeskProvider)
	if !ok || p.Desk() != d {
		t.Fatalf("the desk source does not hand back its desk: %v", ok)
	}
}

// Hostile text in a header, a body, a skip reason, an event line and a
// status row reaches no Delta or QueueItem string as a control.
func TestHostileDeskTextIsInert(t *testing.T) {
	d := openDesk(t)
	h := termsafetest.Hostile("x")
	pending := filepath.Join(d.Path(), "pending")
	writeFile(t, filepath.Join(pending, "01-hostile.sh"), "#!/bin/bash\n# WHAT: "+h+"\n# WHY: "+h+"\necho "+h+"\n")
	skipped := filepath.Join(d.Path(), "skipped")
	writeFile(t, filepath.Join(skipped, "02-gone.sh"), "# WHAT: "+h+"\n")
	reason, err := json.Marshal(map[string]string{"skip_reason": "\x1b]0;title\a" + h})
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(skipped, "02-gone.meta.json"), string(reason))
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

	s := NewDeskSource(d).(QueueSource)
	items, err := s.Queue()
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0].SHA256 == "" || len(items[0].Body) != 4 || items[1].SkipReason == "" {
		t.Fatalf("items = %+v", items)
	}
	if _, err := s.List(); err != nil {
		t.Fatal(err)
	}
	dl := loadDesk(t, s, name, &Cursor{})
	if len(dl.Events) != 5 || len(dl.Items) != 2 || len(dl.Timing) != 1 || len(dl.Defs) != 1 {
		t.Fatalf("delta: events %d items %d timing %d defs %d", len(dl.Events), len(dl.Items), len(dl.Timing), len(dl.Defs))
	}
	assertDeltaInert(t, dl)
	assertDeltaInert(t, Delta{Items: items})
}

// Poll adds RUN-LOST on every poll of a lost run; the source delivers it once.
func TestARunLostIsDeliveredOnce(t *testing.T) {
	d := openDesk(t)
	name := "08-x"
	done := filepath.Join(d.Path(), "done")
	writeFile(t, filepath.Join(done, name+".sh"), "#!/bin/bash\n")
	writeFile(t, filepath.Join(done, name+".log"), "")
	writeFile(t, filepath.Join(done, name+".events"), "RUN-START id=08-x pid=1\n")
	s := NewDeskSource(d)
	cur := &Cursor{}
	first := loadDesk(t, s, name, cur)
	if first.Live != LiveLost || fmt.Sprint(names(first.Events)) != "[1:RUN-START 2:STEP-START 3:RUN-LOST]" {
		t.Fatalf("first: live %s, events %v", first.Live, names(first.Events))
	}
	if again := loadDesk(t, s, name, cur); again.Live != LiveLost || len(again.Events) != 0 || again.Reset {
		t.Fatalf("again: live %s, events %v, reset %v", again.Live, names(again.Events), again.Reset)
	}
}
