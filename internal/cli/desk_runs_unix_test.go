// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

//go:build unix

package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// batchRun queues a batch manifest and records a run of it by hand: the
// event lines the batch runner writes, ending with rc.
func batchRun(t *testing.T, dir string, lines []string, rc int) string {
	t.Helper()
	out, _, err := deskRun(t, deskDeps(), "add", writeTemp(t, "pipe.manifest",
		"fetch -- true\nbuild after=fetch -- exit 3\nstage after=build -- true\ncheck after=fetch -- true\n"),
		"--what", "a pipeline", "--why", "a test", "--json")
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	var a deskAddJSON
	if err := json.Unmarshal([]byte(out), &a); err != nil {
		t.Fatal(err)
	}
	d := openTestDesk(t, dir)
	run := startRun(t, d, a.Name, a.SHA256, os.Getpid())
	for _, l := range lines {
		if err := run.Events.Emit(l); err != nil {
			t.Fatal(err)
		}
	}
	if err := run.Finish(rc, "failed"); err != nil {
		t.Fatal(err)
	}
	return a.Name
}

var pipeEvents = []string{
	"STEP-START id=fetch deps=",
	"STEP-END id=fetch rc=0 dur=0.5 reason=ok outputs= log=/x/fetch.log",
	"STEP-START id=build deps=fetch",
	"STEP-START id=check deps=fetch",
	"STEP-END id=build rc=3 dur=1.0 reason=failed outputs= log=/x/build.log",
	"STEP-SKIP id=stage reason=dep-failed:build",
	"STEP-END id=check rc=0 dur=0.2 reason=ok outputs= log=/x/check.log",
}

func TestDeskShow_BatchFlowAndReplay(t *testing.T) {
	dir := newDeskDir(t)
	name := batchRun(t, dir, pipeEvents, 1)

	out, _, err := deskRunASCII(t, "show", name)
	wantExit(t, err, 0)
	for _, want := range []string{
		"desk/" + name + " · exit 1",
		"+ fetch  done",
		"x build  failed",
		"- stage  skipped",
		"+ check  done",
		"after fetch",
		"record: forgectl desk status " + name,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("show is missing %q:\n%s", want, out)
		}
	}
	// show is the flow, not the record: what status NAME prints stays there.
	for _, absent := range []string{"sha256", "what=", "a pipeline"} {
		if strings.Contains(out, absent) {
			t.Errorf("show repeats the record (%q):\n%s", absent, out)
		}
	}

	// RUN-START, then the seven step lines: after three events only fetch
	// has finished and build is running.
	out, _, err = deskRunASCII(t, "show", name, "--at", "4", "--events")
	wantExit(t, err, 0)
	for _, want := range []string{"replay 4/", "+ fetch  done", "* build  running", ". stage  pending", "#4", "STEP-START step=build"} {
		if !strings.Contains(out, want) {
			t.Errorf("replay is missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "#5") {
		t.Errorf("replay printed an event past --at:\n%s", out)
	}
}

// A step with no deps that is not first has nothing to say after "after":
// the flow line omits the segment instead of printing a dangling one.
func TestDeskShow_NoDepsStepHasNoAfter(t *testing.T) {
	dir := newDeskDir(t)
	out, _, err := deskRun(t, deskDeps(), "add", writeTemp(t, "roots.manifest",
		"alpha -- true\nbeta -- true\ngamma after=alpha -- exit 1\n"),
		"--what", "two roots", "--why", "a test", "--json")
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	var a deskAddJSON
	if err := json.Unmarshal([]byte(out), &a); err != nil {
		t.Fatal(err)
	}
	run := startRun(t, openTestDesk(t, dir), a.Name, a.SHA256, os.Getpid())
	for _, l := range []string{
		"STEP-START id=alpha deps=",
		"STEP-START id=beta deps=",
		"STEP-END id=alpha rc=0 dur=0.3 reason=ok outputs= log=/x/alpha.log",
		"STEP-END id=beta rc=0 dur=0.4 reason=ok outputs= log=/x/beta.log",
		"STEP-START id=gamma deps=alpha",
		"STEP-END id=gamma rc=1 dur=0.1 reason=failed outputs= log=/x/gamma.log",
	} {
		if err := run.Events.Emit(l); err != nil {
			t.Fatal(err)
		}
	}
	if err := run.Finish(1, "failed"); err != nil {
		t.Fatal(err)
	}

	out, _, err = deskRunASCII(t, "show", a.Name)
	wantExit(t, err, 0)
	var beta, gamma string
	for _, l := range strings.Split(out, "\n") {
		switch {
		case strings.Contains(l, " beta "):
			beta = l
		case strings.Contains(l, " gamma "):
			gamma = l
		}
	}
	if beta == "" || strings.Contains(beta, "after") {
		t.Errorf("beta line = %q, want it present with no after segment:\n%s", beta, out)
	}
	if !strings.HasSuffix(gamma, "after alpha") {
		t.Errorf("gamma line = %q, want it to end in %q:\n%s", gamma, "after alpha", out)
	}
}

func TestDeskShow_JSONContract(t *testing.T) {
	dir := newDeskDir(t)
	name := batchRun(t, dir, pipeEvents, 1)
	out, _, err := deskRun(t, deskDeps(), "show", name, "--json")
	wantExit(t, err, 0)
	want := []string{"counts", "edges", "events", "events_total", "exit", "kind", "live", "name", "partial", "source", "steps", "at"}
	got := jsonKeys(t, []byte(out))
	for _, k := range want {
		if !strings.Contains(" "+strings.Join(got, " ")+" ", " "+k+" ") {
			t.Errorf("show --json lacks %q; keys %v", k, got)
		}
	}
	var res deskShowJSON
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatal(err)
	}
	if res.Live != "ended" || res.Exit == nil || *res.Exit != 1 || res.At != nil {
		t.Errorf("live=%s exit=%v at=%v, want ended 1 null", res.Live, res.Exit, res.At)
	}
	if len(res.Events) != res.EventsTotal || res.EventsTotal != len(pipeEvents)+2 {
		t.Errorf("events = %d of %d, want all %d", len(res.Events), res.EventsTotal, len(pipeEvents)+2)
	}
	status := map[string]string{}
	for _, s := range res.Steps {
		status[s.ID] = s.Status
	}
	if want := map[string]string{"fetch": "closed", "build": "failed", "stage": "skipped", "check": "closed"}; !reflect.DeepEqual(status, want) {
		t.Errorf("steps = %v, want %v", status, want)
	}
}

func TestDeskRuns_ListsProgressAndSkipsPending(t *testing.T) {
	dir := newDeskDir(t)
	name := batchRun(t, dir, pipeEvents, 1)
	waiting, _ := queueItem(t, "later.sh", "echo later\n")
	script, sha := queueItem(t, "ok.sh", "echo ok\n")
	finishRun(t, openTestDesk(t, dir), script, sha, 0)

	out, _, err := deskRunASCII(t, "runs")
	wantExit(t, err, 0)
	if !strings.Contains(out, name) || !strings.Contains(out, "2/4 steps, 1 failed") {
		t.Errorf("runs lacks the batch's progress:\n%s", out)
	}
	if !strings.Contains(out, script) || !strings.Contains(out, "1/1 steps") {
		t.Errorf("runs lacks the script:\n%s", out)
	}
	if strings.Contains(out, waiting) {
		t.Errorf("runs lists the pending item %s:\n%s", waiting, out)
	}

	out, _, err = deskRun(t, deskDeps(), "runs", "--json")
	wantExit(t, err, 0)
	var rows []deskRunJSON
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("runs --json: %v\n%s", err, out)
	}
	if len(rows) != 2 {
		t.Fatalf("runs --json = %d rows, want 2:\n%s", len(rows), out)
	}
}

func TestDeskShow_Log(t *testing.T) {
	newDeskDir(t)
	p := writeTemp(t, "events.jsonl", strings.Join([]string{
		`{"kind":"boot","at":"2026-10-06T10:00:00Z"}`,
		`{"kind":"stage_started","stage":"fetch","at":"2026-10-06T10:00:01Z"}`,
		`not json`,
		`{"kind":"warn","msg":"\u001b[2Jcleared","at":1791280000}`,
	}, "\n")+"\n")
	out, _, err := deskRun(t, deskDeps(), "show", "--log", p, "--event-key", "kind", "--step-key", "stage", "--time-key", "at", "--events")
	wantExit(t, err, 0)
	for _, want := range []string{"log/events.jsonl · log", "a log has no step model", "stage_started step=fetch", "3 events · 1 line dropped"} {
		if !strings.Contains(out, want) {
			t.Errorf("show --log is missing %q:\n%s", want, out)
		}
	}
	if strings.ContainsRune(out, 0x1b) {
		t.Errorf("show --log printed a raw ESC:\n%q", out)
	}
	if strings.Contains(out, "record:") {
		t.Errorf("a log has no desk record to point to:\n%s", out)
	}

	out, _, err = deskRun(t, deskDeps(), "runs", "--log", p, "--event-key", "kind")
	wantExit(t, err, 0)
	if !strings.Contains(out, "log:events.jsonl") {
		t.Errorf("runs --log lacks the log:\n%s", out)
	}
}

func TestDeskShow_Usage(t *testing.T) {
	newDeskDir(t)
	p := writeTemp(t, "e.jsonl", "{}\n")
	for _, args := range [][]string{
		{"show"},
		{"show", "01-x", "--log", p},
		{"show", "not a name"},
		{"show", "01-x", "--at", "-1"},
		{"show", "--log", p, "--event-key", ""},
	} {
		_, _, err := deskRun(t, deskDeps(), args...)
		wantExit(t, err, 2)
	}
	_, _, err := deskRun(t, deskDeps(), "show", "07-missing")
	wantExit(t, err, 1)
}

func TestDeskShow_LogRefusesASymlink(t *testing.T) {
	newDeskDir(t)
	target := writeTemp(t, "real.jsonl", `{"event":"x"}`+"\n")
	link := filepath.Join(t.TempDir(), "link.jsonl")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	_, _, err := deskRun(t, deskDeps(), "show", "--log", link)
	wantExit(t, err, 1)
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Errorf("the refusal should name the symlink: %v", err)
	}
}

// deskRunASCII is deskRun with the root's --no-icons set, so the text
// asserts on the ASCII legend. The test tree has no root, so the flag is
// added here as the root would.
func deskRunASCII(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	for _, k := range []string{"HERDR_ENV", "HERDR_SOCKET_PATH", "HERDR_PANE_ID"} {
		t.Setenv(k, "")
	}
	var out, errOut bytes.Buffer
	cmd := newDeskCmd(deskDeps())
	cmd.PersistentFlags().Bool("no-icons", false, "")
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(append(args, "--no-icons"))
	err := cmd.ExecuteContext(t.Context())
	return out.String(), errOut.String(), err
}

// A replayed log stays "log": with no step model the fold cannot know it
// ended, and calling it live would contradict the run without --at.
func TestDeskShow_LogReplayStaysUnknownAndHeldLineIsNamed(t *testing.T) {
	newDeskDir(t)
	p := writeTemp(t, "e.jsonl", `{"event":"a"}`+"\n"+`{"event":"b"}`+"\n"+`{"event":"end"}`)
	out, _, err := deskRun(t, deskDeps(), "show", "--log", p, "--at", "1")
	wantExit(t, err, 0)
	if !strings.Contains(out, "· log · replay 1/2") {
		t.Errorf("a replayed log should read log:\n%s", out)
	}
	if !strings.Contains(out, "last line has no newline yet: not read") {
		t.Errorf("the unterminated last line is not named:\n%s", out)
	}
	_, _, err = deskRun(t, deskDeps(), "runs", "--log", p, "--event-key", "")
	wantExit(t, err, 2)
}

// A source that cannot be read is named on stderr and exits 1, and the
// sources that could be read are still listed.
func TestDeskRuns_OneBadSourceStillListsTheRest(t *testing.T) {
	dir := newDeskDir(t)
	script, sha := queueItem(t, "ok.sh", "echo ok\n")
	finishRun(t, openTestDesk(t, dir), script, sha, 0)
	out, errOut, err := deskRun(t, deskDeps(), "runs", "--log", filepath.Join(t.TempDir(), "missing.jsonl"))
	wantExit(t, err, 1)
	if !strings.Contains(out, script) {
		t.Errorf("the desk's run is not listed:\n%s", out)
	}
	if !strings.Contains(errOut, "missing.jsonl") {
		t.Errorf("stderr does not name the bad source:\n%s", errOut)
	}
}

// A log past the 32 MiB cap is shown, says so, and exits 1: a partial
// result is not success.
func TestDeskShow_LogPastTheCapExits1(t *testing.T) {
	newDeskDir(t)
	p := filepath.Join(t.TempDir(), "big.jsonl")
	line := []byte(`{"event":"tick","pad":"` + strings.Repeat("x", 1000) + `"}` + "\n")
	f, err := os.Create(p) //nolint:gosec // G304: a test file under t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	for written := 0; written <= 33<<20; written += len(line) {
		if _, err := f.Write(line); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	out, _, err := deskRun(t, deskDeps(), "show", "--log", p)
	wantExit(t, err, 1)
	if !strings.Contains(out, "partial: past the 32 MiB cap") || strings.Contains(out, "no newline") {
		t.Errorf("show should name the cap, and not call the cut line held:\n%s", out)
	}
	_, _, err = deskRun(t, deskDeps(), "runs", "--log", p)
	wantExit(t, err, 1)
}

// A waiting item has no run yet: show reads it as waiting, with no events.
func TestDeskShow_AWaitingItem(t *testing.T) {
	newDeskDir(t)
	name, _ := queueItem(t, "later.sh", "echo later\n")
	out, _, err := deskRunASCII(t, "show", name)
	wantExit(t, err, 0)
	if !strings.Contains(out, "· waiting") || !strings.Contains(out, "0 events") {
		t.Errorf("a waiting item should show as waiting with no events:\n%s", out)
	}
}

// A log's #N numbers its events, not its file lines, so #N and --at N agree.
func TestDeskShow_LogEventsAreNumberedWithoutGaps(t *testing.T) {
	newDeskDir(t)
	p := writeTemp(t, "e.jsonl", `{"event":"a"}`+"\n\nnot json\n"+`{"event":"b"}`+"\n"+`{"event":"c"}`+"\n")
	out, _, err := deskRun(t, deskDeps(), "show", "--log", p, "--events", "--at", "2")
	wantExit(t, err, 0)
	if !strings.Contains(out, "#1    a") || !strings.Contains(out, "#2    b") || strings.Contains(out, "#3") {
		t.Errorf("events should be #1 and #2 at --at 2:\n%s", out)
	}
}

func TestDeskLogKeyFlagsNeedALog(t *testing.T) {
	newDeskDir(t)
	for _, args := range [][]string{{"runs", "--event-key", "kind"}, {"show", "01-x", "--time-key", "at"}} {
		_, _, err := deskRun(t, deskDeps(), args...)
		wantExit(t, err, 2)
	}
}
