//go:build unix

package desk

import (
	"context"
	"errors"
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
