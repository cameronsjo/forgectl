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
