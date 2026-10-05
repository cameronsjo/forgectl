// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

//go:build unix

package runview

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixture is a run root holding the example spec's runs, with
// $PIPELINE_ROOT pointing at it.
type fixture struct {
	t    *testing.T
	root string
	spec *Spec
	src  Source
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	t.Setenv("PIPELINE_ROOT", root)
	spec := loadExample(t)
	src, err := NewSpecSource(spec)
	if err != nil {
		t.Fatalf("NewSpecSource: %v", err)
	}
	return &fixture{t: t, root: root, spec: spec, src: src}
}

// write creates rel below the root, with its parent directories.
func (f *fixture) write(rel, content string) {
	f.t.Helper()
	p := filepath.Join(f.root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) appendTo(rel, content string) {
	f.t.Helper()
	fh, err := os.OpenFile(filepath.Join(f.root, rel), os.O_WRONLY|os.O_APPEND, 0) //nolint:gosec // G304: a path below the test's own temp root
	if err != nil {
		f.t.Fatal(err)
	}
	defer fh.Close() //nolint:errcheck // test
	if _, err := fh.WriteString(content); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) symlink(target, rel string) {
	f.t.Helper()
	p := filepath.Join(f.root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		f.t.Fatal(err)
	}
	if err := os.Symlink(target, p); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) load(name string, cur *Cursor) Delta {
	f.t.Helper()
	d, err := f.src.Load(RunRef{Source: f.src.Name(), Name: name, Kind: KindLog}, cur)
	if err != nil {
		f.t.Fatalf("Load(%s): %v", name, err)
	}
	return d
}

const logRel = "runs/alpha/out/events.jsonl"

// ev renders one event line in the example spec's vocabulary.
func evLine(kind, stage string) string {
	return fmt.Sprintf(`{"at":"2026-01-02T03:04:05Z","kind":%q,"stage":%q}`+"\n", kind, stage)
}

func names(evs []Event) []string {
	var out []string
	for _, e := range evs {
		out = append(out, fmt.Sprintf("%d:%s", e.Seq, e.Name))
	}
	return out
}

func TestALoadReadsOnlyWhatWasAdded(t *testing.T) {
	f := newFixture(t)
	f.write(logRel, evLine("stage_started", "fetch"))
	cur := &Cursor{}
	if d := f.load("alpha", cur); fmt.Sprint(names(d.Events)) != "[1:stage_started]" || d.Err != nil {
		t.Fatalf("first load: %v, err %v", names(d.Events), d.Err)
	}
	if d := f.load("alpha", cur); len(d.Events) != 0 || d.Reset {
		t.Fatalf("second load with nothing added: %v, reset %v", names(d.Events), d.Reset)
	}
	f.appendTo(logRel, evLine("stage_done", "fetch"))
	if d := f.load("alpha", cur); fmt.Sprint(names(d.Events)) != "[2:stage_done]" {
		t.Fatalf("third load: %v", names(d.Events))
	}
}

// The held start of a line lives in the cursor, not the source: a second
// source given the same cursor completes it.
func TestAPartialLineIsHeldInTheCursor(t *testing.T) {
	f := newFixture(t)
	whole := evLine("stage_done", "fetch")
	f.write(logRel, evLine("stage_started", "fetch")+whole[:20])
	cur := &Cursor{}
	if d := f.load("alpha", cur); fmt.Sprint(names(d.Events)) != "[1:stage_started]" {
		t.Fatalf("first load: %v", names(d.Events))
	}
	f.appendTo(logRel, whole[20:])
	other, err := NewSpecSource(f.spec)
	if err != nil {
		t.Fatal(err)
	}
	d, err := other.Load(RunRef{Source: other.Name(), Name: "alpha", Kind: KindLog}, cur)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(names(d.Events)) != "[2:stage_done]" || d.Events[0].Step != "fetch" {
		t.Fatalf("second load: %+v", d.Events)
	}
}

func TestALineOver64KiBIsDroppedAndCounted(t *testing.T) {
	f := newFixture(t)
	long := `{"kind":"stage_started","stage":"fetch","pad":"` + strings.Repeat("x", 65<<10) + `"}` + "\n"
	f.write(logRel, evLine("worker_boot", "")+long+evLine("stage_done", "fetch"))
	d := f.load("alpha", &Cursor{})
	if fmt.Sprint(names(d.Events)) != "[1:worker_boot 3:stage_done]" || d.Dropped != 1 {
		t.Fatalf("events %v, dropped %d; want the long line 2 dropped", names(d.Events), d.Dropped)
	}

	// The same line arriving in pieces, with no newline yet, is not held
	// past the cap, and is still counted once when its newline comes.
	f2 := newFixture(t)
	f2.write(logRel, long[:40<<10])
	cur := &Cursor{}
	f2.load("alpha", cur)
	f2.appendTo(logRel, long[40<<10:])
	f2.appendTo(logRel, evLine("stage_done", "fetch"))
	d = f2.load("alpha", cur)
	if fmt.Sprint(names(d.Events)) != "[2:stage_done]" || d.Dropped != 1 || len(cur.held) != 0 {
		t.Fatalf("events %v, dropped %d, held %d", names(d.Events), d.Dropped, len(cur.held))
	}
}

func TestAReplacedOrShrunkFileResets(t *testing.T) {
	for _, tc := range []struct {
		name    string
		replace func(f *fixture)
	}{
		{"replaced, larger", func(f *fixture) {
			f.write("runs/alpha/out/new.jsonl", evLine("worker_boot", "")+evLine("stage_started", "build")+evLine("stage_done", "build"))
			if err := os.Rename(filepath.Join(f.root, "runs/alpha/out/new.jsonl"), filepath.Join(f.root, logRel)); err != nil {
				f.t.Fatal(err)
			}
		}},
		{"replaced, same size", func(f *fixture) {
			f.write("runs/alpha/out/new.jsonl", evLine("worker_boot", "")+evLine("stage_started", "build"))
			if err := os.Rename(filepath.Join(f.root, "runs/alpha/out/new.jsonl"), filepath.Join(f.root, logRel)); err != nil {
				f.t.Fatal(err)
			}
		}},
		{"truncated in place", func(f *fixture) {
			if err := os.Truncate(filepath.Join(f.root, logRel), 0); err != nil {
				f.t.Fatal(err)
			}
			f.appendTo(logRel, evLine("worker_boot", ""))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			// Two lines of the same length as the replacement's first two, so
			// the same-size case really is the same size.
			f.write(logRel, evLine("worker_boot", "")+evLine("stage_started", "fetch"))
			cur := &Cursor{}
			if d := f.load("alpha", cur); len(d.Events) != 2 || d.Reset {
				t.Fatalf("first load: %v, reset %v", names(d.Events), d.Reset)
			}
			tc.replace(f)
			d := f.load("alpha", cur)
			if !d.Reset || len(d.Events) == 0 || d.Events[0].Seq != 1 || d.Events[0].Name != "worker_boot" {
				t.Fatalf("after replace: reset %v, events %v; want a reset from line 1", d.Reset, names(d.Events))
			}
		})
	}
}

// The secret lives inside the root, so os.Root alone would follow the
// symlink to it: only the per-component no-follow walk keeps it out.
func TestASymlinkedIntermediateDirYieldsNothing(t *testing.T) {
	f := newFixture(t)
	f.write("secret/events.jsonl", `{"kind":"stage_started","stage":"fetch","token":"SECRET"}`+"\n")
	f.write("runs/evil/keep", "")
	f.symlink("../../secret", "runs/evil/out")
	d := f.load("evil", &Cursor{})
	if len(d.Events) != 0 || !errors.Is(d.Err, ErrRefused) {
		t.Fatalf("events %+v, err %v; want none and a refusal", d.Events, d.Err)
	}
	if strings.Contains(fmt.Sprint(d), "SECRET") {
		t.Fatalf("the secret reached the delta: %+v", d)
	}

	// A symlinked run dir is neither listed nor loadable by name.
	f.write("elsewhere/out/events.jsonl", `{"kind":"stage_started","stage":"SECRET"}`+"\n")
	f.symlink("../elsewhere", "runs/linked")
	refs, err := f.src.List()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range refs {
		if r.Name == "linked" {
			t.Errorf("List included the symlinked run dir: %+v", refs)
		}
	}
	if d := f.load("linked", &Cursor{}); len(d.Events) != 0 || !errors.Is(d.Err, ErrRefused) {
		t.Fatalf("linked: events %+v, err %v", d.Events, d.Err)
	}
}

func TestASymlinkedLogIsRefused(t *testing.T) {
	f := newFixture(t)
	f.write("runs/alpha/out/real.jsonl", evLine("stage_started", "fetch"))
	f.symlink("real.jsonl", logRel)
	d := f.load("alpha", &Cursor{})
	if len(d.Events) != 0 || !errors.Is(d.Err, ErrRefused) {
		t.Fatalf("events %+v, err %v; want a refusal", d.Events, d.Err)
	}
}

func TestAFIFOOrDirectoryAtTheLogIsRefused(t *testing.T) {
	f := newFixture(t)
	if err := os.MkdirAll(filepath.Join(f.root, "runs/alpha/out"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := mkfifo(filepath.Join(f.root, logRel)); err != nil {
		t.Fatal(err)
	}
	if d := f.load("alpha", &Cursor{}); !errors.Is(d.Err, ErrRefused) {
		t.Fatalf("FIFO: err %v; want a refusal (and no hang)", d.Err)
	}
	f.write("runs/beta/out/events.jsonl/x", "")
	if d := f.load("beta", &Cursor{}); !errors.Is(d.Err, ErrRefused) {
		t.Fatalf("directory: err %v; want a refusal", d.Err)
	}
}

func TestAMissingLogIsNoLogYet(t *testing.T) {
	f := newFixture(t)
	f.write("runs/alpha/keep", "")
	d := f.load("alpha", &Cursor{})
	if !errors.Is(d.Err, fs.ErrNotExist) || len(d.Events) != 0 {
		t.Fatalf("err %v; want one wrapping fs.ErrNotExist", d.Err)
	}
}

// A file past 32 MiB is read up to the cap and the run is marked partial.
// The file is sparse, so the test writes almost nothing.
func TestAFileOverTheCapIsPartial(t *testing.T) {
	f := newFixture(t)
	f.write(logRel, evLine("stage_started", "fetch"))
	if err := os.Truncate(filepath.Join(f.root, logRel), maxFileBytes+1<<20); err != nil {
		t.Fatal(err)
	}
	cur := &Cursor{}
	d := f.load("alpha", cur)
	if !d.Partial || len(d.Events) != 1 || cur.Offset != maxFileBytes {
		t.Fatalf("partial %v, events %d, offset %d", d.Partial, len(d.Events), cur.Offset)
	}
	if d := f.load("alpha", cur); !d.Partial || len(d.Events) != 0 {
		t.Fatalf("second load: partial %v, events %d", d.Partial, len(d.Events))
	}
}

func TestEventsPastTheRunCapAreCounted(t *testing.T) {
	f := newFixture(t)
	var b strings.Builder
	for range maxRunEvents + 7 {
		b.WriteString(`{"kind":"tick"}` + "\n")
	}
	f.write(logRel, b.String())
	d := f.load("alpha", &Cursor{})
	if len(d.Events) != maxRunEvents || d.Dropped != 7 {
		t.Fatalf("events %d, dropped %d; want %d and 7", len(d.Events), d.Dropped, maxRunEvents)
	}
}
