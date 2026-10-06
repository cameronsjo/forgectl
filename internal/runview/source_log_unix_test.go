// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

//go:build unix

package runview

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"

	"github.com/cameronsjo/forgectl/internal/termsafe/termsafetest"
)

// logFixture is a directory holding one log, read through a log source.
type logFixture struct {
	t    *testing.T
	dir  string
	path string
	src  Source
}

func newLogFixture(t *testing.T) *logFixture {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "events.jsonl")
	src, err := NewLogSource(p, DefaultLogKeys)
	if err != nil {
		t.Fatalf("NewLogSource: %v", err)
	}
	return &logFixture{t: t, dir: dir, path: p, src: src}
}

func (f *logFixture) write(content string) {
	f.t.Helper()
	if err := os.WriteFile(f.path, []byte(content), 0o600); err != nil {
		f.t.Fatal(err)
	}
}

func (f *logFixture) appendTo(content string) {
	f.t.Helper()
	fh, err := os.OpenFile(f.path, os.O_WRONLY|os.O_APPEND, 0) //nolint:gosec // G304: a path below the test's own temp dir
	if err != nil {
		f.t.Fatal(err)
	}
	defer fh.Close() //nolint:errcheck // test
	if _, err := fh.WriteString(content); err != nil {
		f.t.Fatal(err)
	}
}

// load returns the delta and the error Load reported.
func (f *logFixture) loadErr(cur *Cursor) (Delta, error) {
	f.t.Helper()
	return f.src.Load(RunRef{Source: f.src.Name(), Name: filepath.Base(f.path), Kind: KindLog}, cur)
}

func (f *logFixture) load(cur *Cursor) Delta {
	f.t.Helper()
	d, err := f.loadErr(cur)
	if err != nil {
		f.t.Fatalf("Load: %v", err)
	}
	return d
}

// logLine renders one event line in the default keys.
func logLine(event, step string) string {
	return fmt.Sprintf(`{"time":"2026-01-02T03:04:05Z","event":%q,"step":%q}`+"\n", event, step)
}

func names(evs []Event) string {
	out := make([]string, 0, len(evs))
	for _, e := range evs {
		out = append(out, fmt.Sprintf("%d:%s", e.Seq, e.Name))
	}
	return fmt.Sprint(out)
}

func TestNewLogSourceRefusesBadArguments(t *testing.T) {
	if _, err := NewLogSource("relative/events.jsonl", DefaultLogKeys); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Errorf("relative path: err %v", err)
	}
	if _, err := NewLogSource("", DefaultLogKeys); err == nil {
		t.Errorf("empty path accepted")
	}
	if _, err := NewLogSource("/tmp/x.jsonl", LogKeys{Step: "step"}); err == nil || !strings.Contains(err.Error(), "event key") {
		t.Errorf("empty event key: err %v", err)
	}
	s, err := NewLogSource("/tmp/../tmp/x.jsonl", DefaultLogKeys)
	if err != nil || s.Name() != "log" {
		t.Errorf("valid args: %v, %v", s, err)
	}
}

func TestDefaultLogKeys(t *testing.T) {
	if DefaultLogKeys != (LogKeys{Event: "event", Step: "step", Time: "time"}) {
		t.Errorf("DefaultLogKeys = %+v", DefaultLogKeys)
	}
}

func TestLogListNamesTheOneRun(t *testing.T) {
	f := newLogFixture(t)
	f.write(logLine("a", ""))
	refs, err := f.src.List()
	if err != nil || len(refs) != 1 {
		t.Fatalf("List = %+v, %v", refs, err)
	}
	r := refs[0]
	if r.Name != "events.jsonl" || r.Kind != KindLog || r.Source != "log" || r.Updated.IsZero() || r.Live != "" {
		t.Errorf("ref = %+v", r)
	}
}

func TestLogLoadHasUnknownLiveAndNoDefs(t *testing.T) {
	f := newLogFixture(t)
	f.write(logLine("a", "s"))
	d := f.load(&Cursor{})
	if d.Live != LiveUnknown || d.Defs != nil || d.Timing != nil || d.Reset || d.Partial || d.Err != nil {
		t.Errorf("delta = %+v", d)
	}
	// A nil cursor reads from the start.
	if d, err := f.loadErr(nil); err != nil || len(d.Events) != 1 {
		t.Errorf("nil cursor: %v, %v", names(d.Events), err)
	}
}

func TestLogLoadReadsOnlyWhatWasAdded(t *testing.T) {
	f := newLogFixture(t)
	f.write(logLine("start", "fetch"))
	cur := &Cursor{}
	if d := f.load(cur); names(d.Events) != "[1:start]" || d.Err != nil {
		t.Fatalf("first load: %v, err %v", names(d.Events), d.Err)
	}
	if d := f.load(cur); len(d.Events) != 0 || d.Reset {
		t.Fatalf("second load with nothing added: %v, reset %v", names(d.Events), d.Reset)
	}
	f.appendTo(logLine("end", "fetch"))
	if d := f.load(cur); names(d.Events) != "[2:end]" {
		t.Fatalf("third load: %v", names(d.Events))
	}
}

// The held start of a line lives in the cursor, not the source: a second
// source given the same cursor completes it.
func TestLogPartialLineIsHeldUntilItsNewline(t *testing.T) {
	f := newLogFixture(t)
	whole := logLine("end", "fetch")
	f.write(logLine("start", "fetch") + whole[:20])
	cur := &Cursor{}
	if d := f.load(cur); names(d.Events) != "[1:start]" {
		t.Fatalf("first load: %v", names(d.Events))
	}
	if len(cur.held) != 20 {
		t.Fatalf("held %d bytes, want 20", len(cur.held))
	}
	f.appendTo(whole[20:])
	other, err := NewLogSource(f.path, DefaultLogKeys)
	if err != nil {
		t.Fatal(err)
	}
	d, err := other.Load(RunRef{Name: "events.jsonl", Kind: KindLog}, cur)
	if err != nil {
		t.Fatal(err)
	}
	if names(d.Events) != "[2:end]" || d.Events[0].Step != "fetch" || len(cur.held) != 0 {
		t.Fatalf("second load: %+v held %d", d.Events, len(cur.held))
	}
}

func TestLogLineWithoutANewlineIsNotDeliveredYet(t *testing.T) {
	f := newLogFixture(t)
	f.write(strings.TrimSuffix(logLine("a", ""), "\n"))
	cur := &Cursor{}
	if d := f.load(cur); len(d.Events) != 0 || d.Dropped != 0 {
		t.Fatalf("an unfinished line delivered: %v dropped %d", names(d.Events), d.Dropped)
	}
	f.appendTo("\n")
	if d := f.load(cur); names(d.Events) != "[1:a]" {
		t.Fatalf("after the newline: %v", names(d.Events))
	}
}

func TestLogFieldRules(t *testing.T) {
	f := newLogFixture(t)
	f.write(strings.Join([]string{
		`{"event":"a","step":"fetch","try":2,"big":1152921504606846976,"neg":-5}`,
		`not json`,
		`{"event":"b","nested":{"a":[1,2]},"ratio":0.5,"exp":1e3,"huge":9223372036854775808,"ok":true,"no":false,"none":null,"rc":0}`,
		`{"event":"first","event":"second","step":"build"}`,
		`{"event":"x"} {"event":"extra"}`,
		`["event","x"]`,
		`{"step":"fetch"}`,
		``,
		`   `,
	}, "\n") + "\n")
	d := f.load(&Cursor{})

	if got := names(d.Events); got != "[1:a 2:b 3:first]" { // Seq numbers events, not lines
		t.Fatalf("events %s", got)
	}
	want0 := []Field{{"event", "a"}, {"step", "fetch"}, {"try", "2"}, {"big", "1152921504606846976"}, {"neg", "-5"}}
	if !reflect.DeepEqual(d.Events[0].Fields, want0) {
		t.Errorf("fields = %v, want %v", d.Events[0].Fields, want0)
	}
	want2 := []Field{{"event", "b"}, {"ok", "true"}, {"no", "false"}, {"rc", "0"}}
	if !reflect.DeepEqual(d.Events[1].Fields, want2) {
		t.Errorf("fields = %v, want %v", d.Events[1].Fields, want2)
	}
	if d.DroppedFields != 5 {
		t.Errorf("DroppedFields = %d, want 5 (nested, float, exponent, overrun, null)", d.DroppedFields)
	}
	if d.Events[2].Name != "first" || len(d.Events[2].Fields) != 2 {
		t.Errorf("duplicate key: %+v", d.Events[2])
	}
	// "not json", the two-value line, the array and the line with no event
	// name are dropped whole; blank lines are not counted.
	if d.Dropped != 4 {
		t.Errorf("Dropped = %d, want 4", d.Dropped)
	}
}

func TestLogCountsAreTotalsAcrossLoads(t *testing.T) {
	f := newLogFixture(t)
	f.write("junk\n" + `{"event":"a","x":1.5}` + "\n")
	cur := &Cursor{}
	if d := f.load(cur); d.Dropped != 1 || d.DroppedFields != 1 {
		t.Fatalf("first: dropped %d fields %d", d.Dropped, d.DroppedFields)
	}
	f.appendTo("junk2\n" + `{"event":"b","y":null}` + "\n")
	if d := f.load(cur); d.Dropped != 2 || d.DroppedFields != 2 || names(d.Events) != "[2:b]" {
		t.Fatalf("second: dropped %d fields %d events %v; want running totals and Seq 2 (the second event)", d.Dropped, d.DroppedFields, names(d.Events))
	}
}

func TestLogEventsMapTheKeys(t *testing.T) {
	f := newLogFixture(t)
	f.write(`{"time":"2026-01-02T03:04:05Z","event":"start","step":"fetch"}` + "\n" +
		`{"time":1767323050,"event":"end"}` + "\n")
	d := f.load(&Cursor{})
	if len(d.Events) != 2 {
		t.Fatalf("events %v", names(d.Events))
	}
	if e := d.Events[0]; e.Name != "start" || e.Step != "fetch" || !e.Time.Equal(t0) {
		t.Errorf("event 1 = %+v", e)
	}
	if d.Events[1].Time.Unix() != 1767323050 {
		t.Errorf("epoch time = %v", d.Events[1].Time)
	}
}

func TestLogCustomKeys(t *testing.T) {
	f := newLogFixture(t)
	src, err := NewLogSource(f.path, LogKeys{Event: "kind", Step: "stage", Time: "at"})
	if err != nil {
		t.Fatal(err)
	}
	f.write(`{"at":"2026-01-02T03:04:05Z","kind":"go","stage":"s1","event":"ignored"}` + "\n")
	d, err := src.Load(RunRef{Kind: KindLog}, &Cursor{})
	if err != nil || len(d.Events) != 1 {
		t.Fatalf("%v %v", d.Events, err)
	}
	if e := d.Events[0]; e.Name != "go" || e.Step != "s1" || !e.Time.Equal(t0) {
		t.Errorf("event = %+v", e)
	}
}

func TestLogLineOver64KiBIsDroppedAndCounted(t *testing.T) {
	long := `{"event":"big","pad":"` + strings.Repeat("x", 65<<10) + `"}` + "\n"
	f := newLogFixture(t)
	f.write(logLine("boot", "") + long + logLine("end", ""))
	d := f.load(&Cursor{})
	if names(d.Events) != "[1:boot 2:end]" || d.Dropped != 1 {
		t.Fatalf("events %v, dropped %d; want the long line 2 dropped", names(d.Events), d.Dropped)
	}

	// The same line arriving in pieces, with no newline yet, is not held past
	// the cap, and is counted once when its newline comes.
	f2 := newLogFixture(t)
	f2.write(long[:40<<10])
	cur := &Cursor{}
	f2.load(cur)
	f2.appendTo(long[40<<10 : 66000])
	f2.load(cur)
	if len(cur.held) != 0 || !cur.dropping {
		t.Fatalf("held %d, dropping %v after passing the cap", len(cur.held), cur.dropping)
	}
	f2.appendTo(long[66000:])
	f2.appendTo(logLine("end", ""))
	d = f2.load(cur)
	if names(d.Events) != "[1:end]" || d.Dropped != 1 || len(cur.held) != 0 || cur.dropping {
		t.Fatalf("events %v, dropped %d, held %d, dropping %v", names(d.Events), d.Dropped, len(cur.held), cur.dropping)
	}
}

func TestLogLineExactlyAtTheCapIsKept(t *testing.T) {
	prefix := `{"event":"e","pad":"`
	suffix := `"}`
	line := prefix + strings.Repeat("x", maxLineBytes-len(prefix)-len(suffix)) + suffix
	if len(line) != maxLineBytes {
		t.Fatalf("fixture line is %d bytes", len(line))
	}
	f := newLogFixture(t)
	f.write(line + "\n")
	if d := f.load(&Cursor{}); len(d.Events) != 1 || d.Dropped != 0 {
		t.Fatalf("a line of %d bytes: events %d dropped %d", maxLineBytes, len(d.Events), d.Dropped)
	}
	f2 := newLogFixture(t)
	f2.write(line + "x\n")
	if d := f2.load(&Cursor{}); len(d.Events) != 0 || d.Dropped != 1 {
		t.Fatalf("a line of %d bytes: events %d dropped %d", maxLineBytes+1, len(d.Events), d.Dropped)
	}
}

func TestLogReplacedOrShrunkFileResets(t *testing.T) {
	for _, tc := range []struct {
		name    string
		replace func(f *logFixture)
	}{
		{"replaced, larger", func(f *logFixture) {
			next := filepath.Join(f.dir, "new.jsonl")
			if err := os.WriteFile(next, []byte(logLine("boot", "")+logLine("start", "build")+logLine("end", "build")), 0o600); err != nil {
				f.t.Fatal(err)
			}
			if err := os.Rename(next, f.path); err != nil {
				f.t.Fatal(err)
			}
		}},
		{"replaced, same size", func(f *logFixture) {
			next := filepath.Join(f.dir, "new.jsonl")
			if err := os.WriteFile(next, []byte(logLine("boot", "")+logLine("start", "build")), 0o600); err != nil {
				f.t.Fatal(err)
			}
			if err := os.Rename(next, f.path); err != nil {
				f.t.Fatal(err)
			}
		}},
		{"truncated in place", func(f *logFixture) {
			if err := os.Truncate(f.path, 0); err != nil {
				f.t.Fatal(err)
			}
			f.appendTo(logLine("boot", ""))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLogFixture(t)
			f.write(logLine("boot", "") + logLine("start", "fetch") + "junk\n")
			cur := &Cursor{}
			if d := f.load(cur); len(d.Events) != 2 || d.Reset || d.Dropped != 1 {
				t.Fatalf("first load: %v, reset %v dropped %d", names(d.Events), d.Reset, d.Dropped)
			}
			tc.replace(f)
			d := f.load(cur)
			if !d.Reset || len(d.Events) == 0 || d.Events[0].Seq != 1 || d.Events[0].Name != "boot" {
				t.Fatalf("after replace: reset %v, events %v; want a reset from line 1", d.Reset, names(d.Events))
			}
			if d.Dropped != 0 {
				t.Errorf("Dropped = %d after a reset, want the counts started over", d.Dropped)
			}
			if again := f.load(cur); again.Reset || len(again.Events) != 0 {
				t.Errorf("a second load reset again: %v reset %v", names(again.Events), again.Reset)
			}
		})
	}
}

func TestLogSymlinkLeafIsRefused(t *testing.T) {
	f := newLogFixture(t)
	target := filepath.Join(f.dir, "real.jsonl")
	if err := os.WriteFile(target, []byte(`{"event":"a","token":"SECRET"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, f.path); err != nil {
		t.Fatal(err)
	}
	d, err := f.loadErr(&Cursor{})
	if !errors.Is(err, ErrRefused) || len(d.Events) != 0 {
		t.Fatalf("Load: events %+v, err %v; want a refusal", d.Events, err)
	}
	if strings.Contains(fmt.Sprint(d, err), "SECRET") {
		t.Errorf("the target's content reached the result")
	}
	if _, err := f.src.List(); !errors.Is(err, ErrRefused) {
		t.Errorf("List err = %v; want a refusal", err)
	}
}

func TestLogFIFOAndDirectoryAreRefused(t *testing.T) {
	f := newLogFixture(t)
	if err := syscall.Mkfifo(f.path, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.loadErr(&Cursor{}); !errors.Is(err, ErrRefused) {
		t.Fatalf("FIFO: err %v; want a refusal (and no hang)", err)
	}
	if _, err := f.src.List(); !errors.Is(err, ErrRefused) {
		t.Fatalf("FIFO List: err %v", err)
	}
	if err := os.Remove(f.path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(f.path, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := f.loadErr(&Cursor{}); !errors.Is(err, ErrRefused) {
		t.Fatalf("directory: err %v; want a refusal", err)
	}
}

func TestLogMissingFileIsNotExist(t *testing.T) {
	f := newLogFixture(t)
	if _, err := f.loadErr(&Cursor{}); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Load err = %v; want one wrapping fs.ErrNotExist", err)
	}
	if _, err := f.src.List(); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("List err = %v; want one wrapping fs.ErrNotExist", err)
	}
	// A missing directory is the same.
	src, err := NewLogSource(filepath.Join(f.dir, "gone", "x.jsonl"), DefaultLogKeys)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := src.List(); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing dir: err = %v", err)
	}
}

// A file past 32 MiB is read up to the cap and the run is marked partial. The
// file is sparse, so the test writes almost nothing.
func TestLogFileOverTheCapIsPartial(t *testing.T) {
	f := newLogFixture(t)
	f.write(logLine("start", "fetch"))
	if err := os.Truncate(f.path, maxFileBytes+1<<20); err != nil {
		t.Fatal(err)
	}
	cur := &Cursor{}
	d := f.load(cur)
	if !d.Partial || len(d.Events) != 1 || cur.Offset != maxFileBytes {
		t.Fatalf("partial %v, events %d, offset %d", d.Partial, len(d.Events), cur.Offset)
	}
	if d := f.load(cur); !d.Partial || len(d.Events) != 0 {
		t.Fatalf("second load: partial %v, events %d", d.Partial, len(d.Events))
	}
	// A file under the cap is not partial.
	g := newLogFixture(t)
	g.write(logLine("a", ""))
	if d := g.load(&Cursor{}); d.Partial {
		t.Errorf("a small file is Partial")
	}
}

func TestLogEventsPastTheRunCapAreCounted(t *testing.T) {
	f := newLogFixture(t)
	var b strings.Builder
	for range maxRunEvents + 7 {
		b.WriteString(`{"event":"tick"}` + "\n")
	}
	f.write(b.String())
	cur := &Cursor{}
	d := f.load(cur)
	if len(d.Events) != maxRunEvents || d.Dropped != 7 {
		t.Fatalf("events %d, dropped %d; want %d and 7", len(d.Events), d.Dropped, maxRunEvents)
	}
	f.appendTo(`{"event":"more"}` + "\n")
	if d := f.load(cur); len(d.Events) != 0 || d.Dropped != 8 {
		t.Fatalf("after one more: events %d dropped %d; want 0 and 8", len(d.Events), d.Dropped)
	}
}

// Every string a Delta carries is inert, whichever field the hostile text
// arrived in.
func TestHostileLogTextIsInert(t *testing.T) {
	f := newLogFixture(t)
	h := termsafetest.Hostile("x")
	qb, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	q := string(qb)
	f.write(fmt.Sprintf(`{"event":%s,"step":%s,%s:%s,"time":%s}`+"\n", q, q, q, q, q))
	d := f.load(&Cursor{})
	if len(d.Events) != 1 {
		t.Fatalf("events %d; want the hostile text kept, made inert", len(d.Events))
	}
	assertDeltaInert(t, d)
}

// A hostile file name does not reach List's strings raw.
func TestHostileLogPathErrorIsInert(t *testing.T) {
	dir := t.TempDir()
	src, err := NewLogSource(filepath.Join(dir, "x\x1b[2Jy.jsonl"), DefaultLogKeys)
	if err != nil {
		t.Fatal(err)
	}
	_, err = src.List()
	if err == nil {
		t.Fatal("List of a missing file succeeded")
	}
	termsafetest.AssertInert(t, "error", err.Error())
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
	for _, def := range d.Defs {
		check("def id", def.ID)
		check("def note", def.Note)
		for _, a := range def.After {
			check("def after", a)
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
