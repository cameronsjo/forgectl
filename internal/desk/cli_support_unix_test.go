//go:build unix

package desk

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A resume point at or past RUN-END (a watcher that already printed every
// line) still ends the watch. Before the fix the skipped RUN-END was never
// seen, the item was found in done/ with events, and the run read as lost.
func TestWatchResumedPastRunEndStillEnds(t *testing.T) {
	d := openDesk(t)
	c := queue(t, d, "done.sh", "echo hi\n")
	run, err := d.BeginRun(c.Name, os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Finish(3, "exit"); err != nil {
		t.Fatal(err)
	}
	for _, skip := range []int{2, 5} {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		var lines []string
		state, seen, err := d.Watch(ctx, c.Name, skip, 20*time.Millisecond, func(l string) { lines = append(lines, l) })
		cancel()
		if err != nil || state != WatchEnded {
			t.Errorf("skip %d: state %s err %v lines %q, want ended", skip, state, err, lines)
		}
		if len(lines) != 0 || seen != 2 {
			t.Errorf("skip %d: lines %q seen %d, want none and 2", skip, lines, seen)
		}
	}
}

// SkipNoted picks the reason from where the item is and keeps the note; an
// operator skip can be undone (which clears the note), a lost run cannot.
func TestSkipNoted(t *testing.T) {
	d := openDesk(t)
	a := addScript(t, d, "wait.sh", "echo hi\n")
	scan(t, d)
	if _, err := d.SkipNoted(a.Name, "two\nlines"); err == nil {
		t.Error("a two-line note was accepted")
	}
	reason, err := d.SkipNoted(a.Name, "not today")
	if err != nil || reason != SkipOperator {
		t.Fatalf("SkipNoted = %q, %v; want operator", reason, err)
	}
	s := scan(t, d)
	if len(s.Skipped) != 1 || s.Skipped[0].Meta.SkipNote != "not today" || s.Skipped[0].Meta.SkipReason != SkipOperator {
		t.Fatalf("skipped = %+v", s.Skipped)
	}
	if err := d.Unskip(a.Name); err != nil {
		t.Fatal(err)
	}
	if s := scan(t, d); len(s.Pending) != 1 || s.Pending[0].Meta.SkipNote != "" {
		t.Fatalf("after unskip: pending %+v", s.Pending)
	}

	// A lost run: claimed, owner recorded, owner dead, no lock held.
	c := queue(t, d, "lost.sh", "echo hi\n")
	if err := d.writeMeta(DirRunning, c.Name, Meta{SHA256: c.SHA256, PID: deadPID(t)}); err != nil {
		t.Fatal(err)
	}
	if reason, err := d.SkipNoted(c.Name, "supervisor died"); err != nil || reason != SkipLost {
		t.Fatalf("SkipNoted(lost) = %q, %v; want lost", reason, err)
	}
	if err := d.Unskip(c.Name); err == nil {
		t.Error("a lost run was re-armed")
	}
}

// A meta whose sha256 is not 64 lowercase hex characters is refused, and the
// item reads as refused rather than being compared or printed. A skip still
// works and replaces the bad meta.
func TestReadMetaRefusesAMalformedHash(t *testing.T) {
	d := openDesk(t)
	dropPending(t, d, "01-x.sh", script)
	writeFile(t, filepath.Join(d.Path(), DirPending, "01-x.meta.json"), `{"sha256":"\u001b]0;T\u0007`+strings.Repeat("a", 50)+`"}`, 0o600)
	if _, _, err := d.readMeta(DirPending, "01-x"); !errors.Is(err, ErrRefused) {
		t.Fatalf("readMeta = %v, want ErrRefused", err)
	}
	s := scan(t, d)
	if len(s.Pending) != 1 || s.Pending[0].State != StateRefused || s.Pending[0].Meta.SHA256 != "" {
		t.Fatalf("pending = %+v, want one refused item with no hash", s.Pending)
	}
	if _, err := d.SkipNoted("01-x", "bad meta"); err != nil {
		t.Fatalf("SkipNoted: %v", err)
	}
	m, ok, err := d.readMeta(DirSkipped, "01-x")
	if err != nil || !ok || m.SkippedBy != SkippedByCLI || m.SkippedAt == nil {
		t.Errorf("skipped meta = %+v, %v, %v", m, ok, err)
	}
}

// ReadSummary refuses a summary.json naming a step that is not a step id.
func TestReadSummaryRefusesABadStepID(t *testing.T) {
	d := openDesk(t)
	writeFile(t, filepath.Join(d.Path(), DirDone, "01-b.d", "summary.json"), `{"id":"01-b","steps":[{"id":"ok1"},{"id":"\u001b[2J"}]}`, 0o600)
	if _, err := d.ReadSummary("01-b"); !errors.Is(err, ErrRefused) {
		t.Errorf("ReadSummary = %v, want ErrRefused", err)
	}
	writeFile(t, filepath.Join(d.Path(), DirDone, "01-b.d", "summary.json"), `{"id":"01-b","steps":[{"id":"ok1"}]}`, 0o600)
	if _, err := d.ReadSummary("01-b"); err != nil {
		t.Errorf("ReadSummary of a good summary: %v", err)
	}
}

// plantBeforeRunStart writes files into done/ at the moment between
// BeginRun's done/ check and its events file: another run of the same name
// arriving in that window.
func plantBeforeRunStart(t *testing.T, d *Desk, files map[string]string) {
	t.Helper()
	prev := beforeRunStart
	beforeRunStart = func(string) error {
		for name, body := range files {
			writeFile(t, filepath.Join(d.Path(), DirDone, name), body, 0o600)
		}
		return nil
	}
	t.Cleanup(func() { beforeRunStart = prev })
}

// Another run's events file appearing in that window: BeginRun refuses
// before writing anything, and the other run's events are untouched.
func TestBeginRunNeverWritesIntoAnotherRunsEvents(t *testing.T) {
	d := openDesk(t)
	c := queue(t, d, "race.sh", "true\n")
	theirs := "RUN-START id=" + c.Name + " pid=1\nRUN-END rc=0 reason=ok\n"
	plantBeforeRunStart(t, d, map[string]string{c.Name + ".events": theirs})
	if _, err := d.BeginRun(c.Name, os.Getpid()); !errors.Is(err, ErrRefused) {
		t.Fatalf("BeginRun = %v, want ErrRefused", err)
	}
	if got := readFile(t, d.EventsPath(c.Name)); got != theirs {
		t.Errorf("the other run's events were written: %q", got)
	}
}

// Another run's log appearing in that window: the supervisor abandons the
// run as name-reused, removes the events file it created, and leaves the
// other run's log alone.
func TestAbandonLeavesNoEventsInDone(t *testing.T) {
	d := openDesk(t)
	c := queue(t, d, "race.sh", "true\n")
	plantBeforeRunStart(t, d, map[string]string{c.Name + ".log": "theirs\nEXIT=0\n"})
	if rc, err := d.supervise(c.Name, c.SHA256, c.Kind); rc != 2 || err == nil {
		t.Fatalf("supervise = %d, %v; want 2 and the log's EEXIST", rc, err)
	}
	if got := readFile(t, d.LogPath(c.Name)); got != "theirs\nEXIT=0\n" {
		t.Errorf("the other run's log was written: %q", got)
	}
	if _, err := os.Stat(d.EventsPath(c.Name)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the abandoned run left done/%s.events: %v", c.Name, err)
	}
	m, ok, err := d.readMeta(DirSkipped, c.Name)
	if err != nil || !ok || m.SkipReason != SkipReused {
		t.Errorf("skipped meta = %+v, %v, %v; want name-reused", m, ok, err)
	}
}

// legacyDone lays out a legacy done/: 40 protocol logs that end in EXIT=,
// and four old logs whose names have no NN- number and that never recorded
// an exit, in the four shapes a real desk holds.
func legacyDone(t *testing.T, d *Desk) []string {
	t.Helper()
	done := filepath.Join(d.Path(), DirDone)
	for i := 1; i <= 40; i++ {
		writeFile(t, filepath.Join(done, fmt.Sprintf("%02d-job.log", i)), "ran\nEXIT=0\n", 0o600)
	}
	legacy := []string{"07b-cleanup", "operator-grow", "nightly-t4-grow", "host.restart_x"}
	writeFile(t, filepath.Join(done, "07b-cleanup.sh"), "#!/bin/bash\n# WHAT: tidy\necho tidy\n", 0o600)
	for _, n := range legacy {
		writeFile(t, filepath.Join(done, n+".log"), "ran, then the old desk stopped\n", 0o600)
	}
	return legacy
}

// Every legacy done/ log shows in history: 44 rows, 40 with an exit and the
// four legacy ones as no exit recorded. Nothing that acts on an item accepts
// a legacy name, and prune clears them.
func TestLegacyDoneNamesShowButNeverAct(t *testing.T) {
	d := openDesk(t)
	legacy := legacyDone(t, d)
	s := scan(t, d)
	if len(s.Done) != 44 {
		t.Fatalf("history has %d rows, want 44", len(s.Done))
	}
	withExit, legacyRows := 0, map[string]bool{}
	for _, it := range s.Done {
		if it.ExitCode != nil {
			withExit++
		}
		if it.Legacy {
			legacyRows[it.Name] = true
			if it.ExitCode != nil || it.Number != -1 {
				t.Errorf("%s: exit %v number %d, want no exit and no number", it.Name, it.ExitCode, it.Number)
			}
		}
	}
	if withExit != 40 || len(legacyRows) != 4 {
		t.Fatalf("%d rows with an exit and %d legacy, want 40 and 4", withExit, len(legacyRows))
	}
	for _, n := range legacy {
		if !legacyRows[n] {
			t.Errorf("%s is not shown as legacy", n)
		}
		if ValidName(n) {
			t.Errorf("%s passes ValidName", n)
		}
		if err := d.Skip(n, SkipOperator); err == nil {
			t.Errorf("Skip(%s) was accepted", n)
		}
		if err := d.Unskip(n); err == nil {
			t.Errorf("Unskip(%s) was accepted", n)
		}
		if _, err := d.Claim(n, anySHA); err == nil {
			t.Errorf("Claim(%s) was accepted", n)
		}
		if _, err := d.BeginRun(n, os.Getpid()); err == nil {
			t.Errorf("BeginRun(%s) was accepted", n)
		}
		if _, err := d.Launch(&Claimed{Name: n, Kind: KindScript, SHA256: anySHA}); err == nil {
			t.Errorf("Launch(%s) was accepted", n)
		}
		if _, err := d.NewWatcher(n, 0); err == nil {
			t.Errorf("NewWatcher(%s) was accepted", n)
		}
		if rc := RunSupervisor(d.Path(), n, anySHA, string(KindScript)); rc == 0 {
			t.Errorf("RunSupervisor(%s) returned 0", n)
		}
	}

	old := time.Now().Add(-90 * 24 * time.Hour)
	for _, n := range legacy {
		_ = os.Chtimes(filepath.Join(d.Path(), DirDone, n+".log"), old, old)
	}
	_ = os.Chtimes(filepath.Join(d.Path(), DirDone, "07b-cleanup.sh"), old, old)
	removed, err := d.Prune(30)
	if err != nil || removed != 4 {
		t.Fatalf("Prune = %d, %v; want the 4 legacy items", removed, err)
	}
	if s := scan(t, d); len(s.Done) != 40 {
		t.Errorf("after prune history has %d rows, want 40", len(s.Done))
	}
	if _, err := os.Stat(filepath.Join(d.Path(), DirDone, "07b-cleanup.sh")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("prune left 07b-cleanup.sh: %v", err)
	}
}
