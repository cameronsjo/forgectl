//go:build unix

package desk

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// Ported from run-and-watch's tests/blocks-batch.sh and blocks-batch2.sh.

type batchOut struct {
	res    BatchResult
	log    string
	events []string
	dir    string
}

func (o batchOut) logLines() []string { return strings.Split(o.log, "\n") }

func (o batchOut) file(t *testing.T, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(o.dir, rel)) //nolint:gosec // G304: a file the test batch wrote under its temp dir
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// runBatch runs text to completion, bounded so a hung run fails the test
// instead of the suite. during, when set, runs alongside the batch with it.
func runBatch(t *testing.T, text string, opts BatchOptions, during func(*Batch, string)) batchOut {
	t.Helper()
	m, err := ParseManifest(text, "t.manifest")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	dir := filepath.Join(t.TempDir(), "t.d")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	var log bytes.Buffer
	var events []string
	opts.ID, opts.Dir, opts.Log = "t", dir, &log
	opts.Event = func(l string) { events = append(events, l) }
	b := NewBatch(m, opts)
	done := make(chan BatchResult, 1)
	go func() { done <- b.Run() }()
	if during != nil {
		during(b, dir)
	}
	select {
	case res := <-done:
		return batchOut{res: res, log: log.String(), events: events, dir: dir}
	case <-time.After(30 * time.Second):
		b.Interrupt()
		b.Interrupt()
		t.Fatal("batch did not finish in 30s")
		return batchOut{}
	}
}

func lineIndex(lines []string, prefix string) int {
	return slices.IndexFunc(lines, func(l string) bool { return strings.HasPrefix(l, prefix) })
}

func TestBatchOrderRunsDependentsAfterTheirDependency(t *testing.T) {
	t.Parallel()
	o := runBatch(t, "a -- echo A; sleep 0.3\nb after=a -- echo B\n", BatchOptions{}, nil)
	if o.res != (BatchResult{RC: 0, Reason: "ok", OK: 2}) {
		t.Fatalf("result = %+v", o.res)
	}
	endA, startB := lineIndex(o.events, "STEP-END id=a "), lineIndex(o.events, "STEP-START id=b ")
	if endA < 0 || startB < endA {
		t.Errorf("b started (%d) before a ended (%d): %q", startB, endA, o.events)
	}
	for _, e := range o.events {
		if strings.HasPrefix(e, "[") {
			t.Errorf("step output reached the event stream: %q", e)
		}
	}
	if !slices.Contains(o.logLines(), "[a] A") || !slices.Contains(o.logLines(), "   order: a -> b") {
		t.Errorf("combined log = %q", o.log)
	}
}

func TestBatchRunsIndependentStepsInParallel(t *testing.T) {
	t.Parallel()
	const waitForPeer = `touch "$RUN_BATCH_DIR/$RUN_STEP_ID"; for i in $(seq 50); do [ -e "$RUN_BATCH_DIR/%s" ] && exit 0; sleep 0.1; done; exit 1`
	text := "a -- " + strings.ReplaceAll(waitForPeer, "%s", "b") + "\nb -- " + strings.ReplaceAll(waitForPeer, "%s", "a") + "\n"
	o := runBatch(t, text, BatchOptions{Jobs: 2}, nil)
	if o.res.RC != 0 {
		t.Errorf("parallel steps did not overlap: %+v %q", o.res, o.events)
	}
	// -j 1 serializes them, so each waits for a peer that never starts.
	o = runBatch(t, "a -- "+strings.ReplaceAll(waitForPeer, "%s", "b")+"\nb -- true\n", BatchOptions{Jobs: 1}, nil)
	if o.res.RC != 1 {
		t.Errorf("-j 1 still overlapped: %+v", o.res)
	}
}

func TestBatchFailureSkipsDependentsAndKeepsIndependentSteps(t *testing.T) {
	t.Parallel()
	o := runBatch(t, "a -- exit 3\nb -- true\nc after=a -- echo ne\"\"ver\nd after=c -- echo ne\"\"ver\n", BatchOptions{}, nil)
	if o.res != (BatchResult{RC: 1, Reason: "failed", OK: 1, Failed: 1, Skipped: 2}) {
		t.Fatalf("result = %+v", o.res)
	}
	for _, want := range []string{"STEP-SKIP id=c reason=dep-failed:a", "STEP-SKIP id=d reason=dep-failed:a"} {
		if !slices.Contains(o.events, want) {
			t.Errorf("missing %q in %q", want, o.events)
		}
	}
	if i := lineIndex(o.events, "STEP-END id=a rc=3 "); i < 0 || !strings.Contains(o.events[i], " reason=failed ") {
		t.Errorf("no failed STEP-END for a: %q", o.events)
	}
	if lineIndex(o.events, "STEP-END id=b rc=0 ") < 0 {
		t.Error("independent b did not run")
	}
	if strings.Contains(o.log, "never") {
		t.Error("a skipped step ran")
	}
	if !strings.Contains(o.file(t, "status.tsv"), "c\tskipped\t-\t-\t-\ta\n") {
		t.Errorf("status.tsv = %q", o.file(t, "status.tsv"))
	}
}

func TestBatchOutputsReachEveryDescendantOnly(t *testing.T) {
	t.Parallel()
	o := runBatch(t, `a -- echo img=xval42 >> "$STEP_OUT"; echo tag=t1 >> "$STEP_OUT"
b after=a -- echo "b sees [$OUT_a_img]"
c after=b -- echo "c sees [$OUT_a_img]"
x -- echo "x sees [${OUT_a_img:-none}]"
`, BatchOptions{}, nil)
	if o.res.RC != 0 {
		t.Fatalf("result = %+v", o.res)
	}
	lines := o.logLines()
	if !slices.Contains(lines, "[c] c sees [xval42]") {
		t.Error("a transitive descendant did not get OUT_a_img")
	}
	if !slices.Contains(lines, "[x] x sees [none]") {
		t.Error("a non-descendant got OUT_a_img")
	}
	if i := lineIndex(o.events, "STEP-END id=a "); i < 0 || !strings.Contains(o.events[i], " outputs=img,tag ") {
		t.Errorf("STEP-END lacks output key names: %q", o.events)
	}
	for _, e := range o.events {
		if strings.Contains(e, "xval42") {
			t.Errorf("an output value reached an event: %q", e)
		}
	}
	for _, f := range []string{"status.tsv", "summary.json"} {
		if strings.Contains(o.file(t, f), "xval42") {
			t.Errorf("an output value reached %s", f)
		}
	}
}

func TestBatchLastLineAndColorAreHandled(t *testing.T) {
	t.Parallel()
	o := runBatch(t, "a -- printf 'one\\nlast-no-newline'\ns -- printf '\\e[31mFAIL\\e[0m x\\r\\n'\n", BatchOptions{}, nil)
	lines := o.logLines()
	last, end := slices.Index(lines, "[a] last-no-newline"), lineIndex(lines, "STEP-END id=a ")
	if last < 0 || end < last {
		t.Errorf("last line (%d) not before STEP-END (%d): %q", last, end, o.log)
	}
	if !slices.Contains(lines, "[s] FAIL x") {
		t.Errorf("colorized line not normalized: %q", o.log)
	}
}

func TestBatchPrivateStepStaysOutOfTheCombinedLog(t *testing.T) {
	t.Parallel()
	o := runBatch(t, "p private -- echo private-line-$((100 + 23)); echo k=v >> \"$STEP_OUT\"\n", BatchOptions{}, nil)
	if o.res.RC != 0 {
		t.Fatalf("result = %+v", o.res)
	}
	if strings.Contains(o.log, "private-line-123") {
		t.Error("a private line reached the combined log")
	}
	if !strings.Contains(o.file(t, "steps/p.log"), "private-line-123") {
		t.Error("the private step's own log lost its output")
	}
	if i := lineIndex(o.events, "STEP-END id=p "); i < 0 || !strings.HasSuffix(o.events[i], " log=private") {
		t.Errorf("private STEP-END names its log: %q", o.events)
	}
	for _, f := range []string{"steps/p.log", "steps/p.out", "status.tsv", "summary.json"} {
		if got := perm(t, filepath.Join(o.dir, f)); got != 0o600 {
			t.Errorf("%s mode %o, want 600", f, got)
		}
	}
	if got := perm(t, filepath.Join(o.dir, "steps")); got != 0o700 {
		t.Errorf("steps/ mode %o", got)
	}
	var sum Summary
	if err := json.Unmarshal([]byte(o.file(t, "summary.json")), &sum); err != nil {
		t.Fatal(err)
	}
	if sum.Steps[0].Log != nil || !sum.Steps[0].Private {
		t.Errorf("summary for a private step = %+v", sum.Steps[0])
	}
}

func TestBatchRedactsPrivateValuesEverywhereDownstream(t *testing.T) {
	t.Parallel()
	o := runBatch(t, `token private -- echo tok=s3cr$((1 + 2))tvalue >> "$STEP_OUT"; printf "esc=\e[1mesc-s3cr$((4 + 5))t\e[0m\n" >> "$STEP_OUT"
use after=token -- env | grep "^OUT_"; echo "again $OUT_token_tok"; printf "s3cr\e[31m3tval\e[0mue\n"; echo "esc $OUT_token_esc"
`, BatchOptions{}, nil)
	if o.res.RC != 0 {
		t.Fatalf("result = %+v", o.res)
	}
	useLog := o.file(t, "steps/use.log")
	for _, secret := range []string{"s3cr3tvalue", "esc-s3cr9t"} {
		if strings.Contains(o.log, secret) || strings.Contains(useLog, secret) {
			t.Errorf("%s leaked", secret)
		}
	}
	if !slices.Contains(o.logLines(), "[use] again <redacted:OUT_token_tok>") {
		t.Errorf("no redaction mark in the combined log: %q", o.log)
	}
	if !strings.Contains(useLog, "<redacted:OUT_token_tok>") {
		t.Error("the descendant's step log is not redacted")
	}
	if strings.Count(o.log, "<redacted:OUT_token_tok>") < 3 {
		t.Errorf("the colorized copy was not redacted: %q", o.log)
	}
	if !strings.Contains(o.log, "<redacted:OUT_token_esc>") {
		t.Error("a value carrying an escape sequence was not redacted")
	}
}

// A timeout signals the step's whole process group: the shell and the
// background sleep it started both die.
func TestBatchTimeoutKillsTheStepsWholeGroup(t *testing.T) {
	t.Parallel()
	o := runBatch(t, "t timeout=1 -- echo $$ > \"$RUN_BATCH_DIR/t.pid\"; sleep 30 & wait\n", BatchOptions{}, nil)
	if o.res.RC != 1 {
		t.Errorf("result = %+v, want rc 1", o.res)
	}
	if i := lineIndex(o.events, "STEP-END id=t rc=124 "); i < 0 || !strings.Contains(o.events[i], " reason=timeout ") {
		t.Errorf("no timeout STEP-END: %q", o.events)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(o.file(t, "t.pid")))
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Kill(-pid, 0); !errors.Is(err, unix.ESRCH) {
		_ = unix.Kill(-pid, unix.SIGKILL)
		t.Errorf("processes survived in group %d (kill -0: %v)", pid, err)
	}
}

func TestBatchFailFastLaunchesNothingNew(t *testing.T) {
	t.Parallel()
	o := runBatch(t, "a -- exit 1\nb -- true\nc -- true\n", BatchOptions{Jobs: 1, FailFast: true}, nil)
	if o.res.RC != 1 {
		t.Errorf("result = %+v", o.res)
	}
	for _, want := range []string{"STEP-SKIP id=b reason=fail-fast", "STEP-SKIP id=c reason=fail-fast"} {
		if !slices.Contains(o.events, want) {
			t.Errorf("missing %q in %q", want, o.events)
		}
	}
}

// Interrupt (Ctrl-C at the supervisor): rc 130, running steps cancelled,
// unstarted ones skipped, and no group survives.
func TestBatchInterruptCancelsEverything(t *testing.T) {
	t.Parallel()
	text := "a -- echo $$ > \"$RUN_BATCH_DIR/a.pid\"; sleep 30\nb -- echo $$ > \"$RUN_BATCH_DIR/b.pid\"; sleep 30\nc after=a -- true\n"
	o := runBatch(t, text, BatchOptions{Jobs: 2}, func(b *Batch, dir string) {
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			a := readFileOr(filepath.Join(dir, "a.pid"))
			bb := readFileOr(filepath.Join(dir, "b.pid"))
			if len(a) > 0 && len(bb) > 0 {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		b.Interrupt()
	})
	if o.res.RC != 130 || o.res.Reason != "interrupted" {
		t.Fatalf("result = %+v, want 130 interrupted", o.res)
	}
	st := o.file(t, "status.tsv")
	for _, want := range []string{"a\tcancelled\t", "b\tcancelled\t", "c\tskipped\t"} {
		if !strings.Contains(st, want) {
			t.Errorf("status.tsv lacks %q: %q", want, st)
		}
	}
	for _, f := range []string{"a.pid", "b.pid"} {
		pid, _ := strconv.Atoi(strings.TrimSpace(o.file(t, f)))
		if err := unix.Kill(-pid, 0); !errors.Is(err, unix.ESRCH) {
			_ = unix.Kill(-pid, unix.SIGKILL)
			t.Errorf("step group %d survived", pid)
		}
	}
}

func TestBatchWarnsOnABadOutputLineAndKeepsGoing(t *testing.T) {
	t.Parallel()
	o := runBatch(t, "a -- echo 1bad=x >> \"$STEP_OUT\"; echo good=y >> \"$STEP_OUT\"; echo novalue >> \"$STEP_OUT\"\n", BatchOptions{}, nil)
	if o.res.RC != 0 {
		t.Fatalf("a bad output key failed the run: %+v", o.res)
	}
	if lineIndex(o.events, "STEP-WARN id=a msg=STEP_OUT line 1 has a bad key") < 0 {
		t.Errorf("no STEP-WARN for the bad key: %q", o.events)
	}
	if i := lineIndex(o.events, "STEP-END id=a rc=0 "); i < 0 || !strings.Contains(o.events[i], " outputs=good ") {
		t.Errorf("good key not kept: %q", o.events)
	}
}

// A step whose shell exits while a process that left its group (setsid) holds
// the pipe still ends, with a STEP-WARN, instead of hanging the batch.
func TestBatchStopsReadingAPipeHeldOutsideTheGroup(t *testing.T) {
	t.Parallel()
	if _, err := os.Stat("/usr/bin/perl"); err != nil {
		t.Skip("needs perl for setsid")
	}
	start := time.Now()
	o := runBatch(t, `a -- perl -e 'use POSIX; if (fork) { exit 0 } POSIX::setsid(); open(F, ">", "$ENV{RUN_BATCH_DIR}/held.pid"); print F $$; close F; sleep 30'`+"\n", BatchOptions{Grace: time.Second}, nil)
	if pid, err := strconv.Atoi(strings.TrimSpace(o.file(t, "held.pid"))); err == nil {
		_ = unix.Kill(pid, unix.SIGKILL)
	}
	if time.Since(start) > 15*time.Second {
		t.Errorf("the batch waited %v on a held pipe", time.Since(start))
	}
	if lineIndex(o.events, "STEP-WARN id=a msg=output pipe still open") < 0 {
		t.Errorf("no held-pipe STEP-WARN: %q", o.events)
	}
}

func TestBatchSummaryShape(t *testing.T) {
	t.Parallel()
	o := runBatch(t, "a -- echo k=v >> \"$STEP_OUT\"\nb after=a -- exit 2\nc after=b -- true\n", BatchOptions{}, nil)
	var sum Summary
	if err := json.Unmarshal([]byte(o.file(t, "summary.json")), &sum); err != nil {
		t.Fatal(err)
	}
	if sum.ID != "t" || sum.RC != 1 || sum.OK != 1 || sum.Failed != 1 || sum.Skipped != 1 || len(sum.Steps) != 3 {
		t.Fatalf("summary = %+v", sum)
	}
	a, b, c := sum.Steps[0], sum.Steps[1], sum.Steps[2]
	if a.State != "ok" || *a.RC != 0 || !slices.Equal(a.Outputs, []string{"k"}) || a.Log == nil || a.Duration == nil {
		t.Errorf("a = %+v", a)
	}
	if b.State != "failed" || *b.RC != 2 || b.Reason != "failed" {
		t.Errorf("b = %+v", b)
	}
	if c.State != "skipped" || c.RC != nil || c.Log != nil || c.Duration != nil || c.Reason != "dep-failed:b" || c.Outputs == nil {
		t.Errorf("c = %+v", c)
	}
	if !regexp.MustCompile(`"outputs": \[\]`).MatchString(o.file(t, "summary.json")) {
		t.Error("an empty outputs list is not rendered as []")
	}
}
