// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

//go:build unix

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/cameronsjo/forgectl/internal/desk"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/runview"
	"github.com/cameronsjo/forgectl/internal/termsafe/termsafetest"
	"github.com/cameronsjo/forgectl/internal/tui"
)

// The desk command: the dashboard's terminal refusal and loop, the agent
// verbs over a fixture spec and a temp desk, and the supervisor's argv.

// deskSpecBody is a run spec in the neutral example vocabulary. %s is the
// source name; %s the meaning of exit 3.
const deskSpecBody = `name = %q
root_env = "PIPELINE_ROOT"
runs = "runs/*"

[log]
path = "out/events.jsonl"
time = "at"
event = "kind"
step = "stage"

[[steps]]
id = "fetch"
[[steps]]
id = "build"

[gate]
label = "accepted?"
max_rejects = 3

[on]
stage_started = "start"
stage_done = "close"
stage_refused = "reject"
run_finished = "end"

[end]
exit_field = "rc"

[[checks]]
id = "audit"
event = "audit_result"
field = "rc"
label = %q

[exits]
"3" = %q
`

// deskEvents is alpha-7's log: fetch, build rejected once and closed, an
// audit that fails, and an end with exit 3. Seven events.
var deskEvents = []string{
	`{"at":"2026-10-05T14:00:00Z","kind":"stage_started","stage":"fetch"}`,
	`{"at":"2026-10-05T14:00:40Z","kind":"stage_done","stage":"fetch"}`,
	`{"at":"2026-10-05T14:01:00Z","kind":"stage_started","stage":"build"}`,
	`{"at":"2026-10-05T14:01:30Z","kind":"stage_refused","stage":"build"}`,
	`{"at":"2026-10-05T14:02:00Z","kind":"stage_done","stage":"build"}`,
	`{"at":"2026-10-05T14:02:05Z","kind":"audit_result","rc":1}`,
	`{"at":"2026-10-05T14:02:11Z","kind":"run_finished","rc":3}`,
}

type deskFixture struct {
	home, deskDir, root string
	spec                string // the "pipeline" spec
}

// newDeskFixture points HOME, TMPDIR, DESK_DIR and PIPELINE_ROOT at temp
// dirs and writes one run, alpha-7, and a spec for it.
func newDeskFixture(t *testing.T) deskFixture {
	t.Helper()
	f := deskFixture{home: t.TempDir(), root: t.TempDir()}
	f.deskDir = filepath.Join(t.TempDir(), "desk")
	t.Setenv("HOME", f.home)
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("DESK_DIR", f.deskDir)
	t.Setenv("CLAUDE_DESK_DIR", "")
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("PIPELINE_ROOT", f.root)
	logDir := filepath.Join(f.root, "runs", "alpha-7", "out")
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(logDir, "events.jsonl"), []byte(strings.Join(deskEvents, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.spec = f.writeSpec(t, "pipeline.toml", fmt.Sprintf(deskSpecBody, "pipeline", "audit ok?", "audit rejected"))
	return f
}

func (f deskFixture) writeSpec(t *testing.T, name, body string) string {
	t.Helper()
	p := filepath.Join(f.home, name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// queue adds a background script to the desk and scans it, so it waits.
func (f deskFixture) queue(t *testing.T, body string) desk.Added {
	t.Helper()
	d, err := desk.Open(f.deskDir)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close() //nolint:errcheck // test cleanup
	src := filepath.Join(t.TempDir(), "job.sh")
	if err := os.WriteFile(src, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := d.Add(src, "print a line", "a fixture", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Scan(); err != nil {
		t.Fatal(err)
	}
	return a
}

// runDeskRoot runs args through the real root command and returns stdout,
// stderr and the exit code main would use.
func runDeskRoot(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	root := newRoot(module.Deps{})
	var out, errOut bytes.Buffer
	root.SetIn(strings.NewReader(""))
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs(args)
	err := root.Execute()
	code := 0
	if err != nil {
		code = ExitCode(err)
		if !errors.As(err, new(*silentCodedError)) {
			errOut.WriteString(err.Error())
		}
	}
	return out.String(), errOut.String(), code
}

// --- the dashboard ---

type fakeDesk struct {
	calls   []tui.DeskOptions
	results []tui.DeskResult
	errs    []error
	ran     []string
	fgErr   error
}

func (f *fakeDesk) runtime(stdinTTY, stdoutTTY bool) deskRuntime {
	rt := deskDefaults()
	rt.stdinIsTerminal = func(io.Reader) bool { return stdinTTY }
	rt.stdoutIsTerminal = func(io.Writer) bool { return stdoutTTY }
	rt.runDesk = func(_ context.Context, o tui.DeskOptions) (tui.DeskResult, error) {
		i := len(f.calls)
		f.calls = append(f.calls, o)
		var err error
		if i < len(f.errs) {
			err = f.errs[i]
		}
		if i < len(f.results) {
			return f.results[i], err
		}
		return tui.DeskResult{}, err
	}
	rt.foreground = func(_ *desk.Desk, name, sha string) error {
		f.ran = append(f.ran, name+"@"+sha)
		return f.fgErr
	}
	rt.waitKey = func() error { return nil }
	return rt
}

func runDeskCmd(t *testing.T, rt deskRuntime, args ...string) (string, string, error) {
	t.Helper()
	cmd := newDeskCmdWith(module.Deps{}, rt)
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var out, errOut bytes.Buffer
	cmd.SetIn(strings.NewReader(""))
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), errOut.String(), err
}

// TestDeskRefusesWithoutATerminal: either stream off a terminal refuses with
// exit 1 and the exact line, before the desk directory is opened (a scan
// changes the queue). Both on a terminal opens the dashboard.
func TestDeskRefusesWithoutATerminal(t *testing.T) {
	for _, tc := range []struct {
		name          string
		stdin, stdout bool
	}{
		{"neither", false, false},
		{"stdin only", true, false},
		{"stdout only", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newDeskFixture(t)
			fake := &fakeDesk{}
			out, errOut, err := runDeskCmd(t, fake.runtime(tc.stdin, tc.stdout))
			if ExitCode(err) != 1 || err == nil {
				t.Fatalf("exit = %d (%v), want 1", ExitCode(err), err)
			}
			if errOut != deskNoTTY+"\n" {
				t.Fatalf("stderr = %q, want the refusal line", errOut)
			}
			if out != "" || len(fake.calls) != 0 {
				t.Fatalf("stdout %q, dashboard calls %d; want nothing", out, len(fake.calls))
			}
			if _, err := os.Stat(f.deskDir); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("desk dir was touched: %v", err)
			}
		})
	}
	t.Run("both", func(t *testing.T) {
		newDeskFixture(t)
		fake := &fakeDesk{}
		if _, errOut, err := runDeskCmd(t, fake.runtime(true, true)); err != nil || len(fake.calls) != 1 {
			t.Fatalf("err %v, stderr %q, calls %d; want one dashboard run", err, errOut, len(fake.calls))
		}
	})
	t.Run("real root, piped", func(t *testing.T) {
		f := newDeskFixture(t)
		out, errOut, code := runDeskRoot(t, "desk")
		if code != 1 || errOut != deskNoTTY+"\n" || out != "" {
			t.Fatalf("code %d, stdout %q, stderr %q", code, out, errOut)
		}
		if _, err := os.Stat(f.deskDir); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("desk dir was touched: %v", err)
		}
	})
}

// TestTheDashboardReopensOnTheRunItRan: a TTY item the dashboard hands back
// runs in the foreground, then the dashboard opens again with the view it
// had and that run selected.
func TestTheDashboardReopensOnTheRunItRan(t *testing.T) {
	f := newDeskFixture(t)
	fake := &fakeDesk{results: []tui.DeskResult{
		{Action: tui.DeskRun{Name: "01-job", SHA: "abc"}, State: tui.DeskViewState{Source: "pipeline", Run: "alpha-7", View: 3, Filter: "al"}},
	}}
	if _, _, err := runDeskCmd(t, fake.runtime(true, true), "--spec", f.spec); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(fake.ran, []string{"01-job@abc"}) {
		t.Fatalf("ran %v, want 01-job@abc", fake.ran)
	}
	if len(fake.calls) != 2 {
		t.Fatalf("dashboard opened %d times, want 2", len(fake.calls))
	}
	if fake.calls[0].Restore != nil {
		t.Fatalf("first open restores %+v", fake.calls[0].Restore)
	}
	r := fake.calls[1].Restore
	if r == nil || r.Source != "desk" || r.Run != "01-job" || r.View != 3 || r.Filter != "al" {
		t.Fatalf("second open restores %+v", r)
	}
	if n := len(fake.calls[1].Sources); n != 2 {
		t.Fatalf("sources = %d, want the desk and the spec", n)
	}
}

func TestTheDashboardEndsWhenThePaneIsGoneOrInterrupted(t *testing.T) {
	newDeskFixture(t)
	gone := &fakeDesk{results: []tui.DeskResult{{Action: tui.DeskRun{Name: "01-job", SHA: "abc"}}}, fgErr: ErrPaneGone}
	if _, _, err := runDeskCmd(t, gone.runtime(true, true)); err != nil || len(gone.calls) != 1 {
		t.Fatalf("pane gone: err %v, opens %d; want nil and 1", err, len(gone.calls))
	}
	refused := &fakeDesk{results: []tui.DeskResult{{Action: tui.DeskRun{Name: "01-job", SHA: "abc"}}}, fgErr: desk.ErrChanged}
	if _, _, err := runDeskCmd(t, refused.runtime(true, true)); err != nil || len(refused.calls) != 2 {
		t.Fatalf("run refused: err %v, opens %d; want nil and a reopen", err, len(refused.calls))
	}
	sigint := &fakeDesk{errs: []error{tea.ErrInterrupted}}
	if _, _, err := runDeskCmd(t, sigint.runtime(true, true)); err != nil {
		t.Fatalf("interrupted: err %v, want exit 0", err)
	}
	broken := &fakeDesk{errs: []error{errors.New("no screen")}}
	if _, _, err := runDeskCmd(t, broken.runtime(true, true)); err == nil {
		t.Fatal("a dashboard error exited 0")
	}
}

// TestABadSpecReachesTheDashboardAsAFailure: the spec that failed is in
// Failed with its path; the desk still loads.
func TestABadSpecReachesTheDashboardAsAFailure(t *testing.T) {
	f := newDeskFixture(t)
	bad := f.writeSpec(t, "bad.toml", "name = \"x\"\nbogus = 1\n")
	fake := &fakeDesk{}
	if _, _, err := runDeskCmd(t, fake.runtime(true, true), "--spec", bad, "--spec", f.spec); err != nil {
		t.Fatal(err)
	}
	o := fake.calls[0]
	if len(o.Failed) != 1 || o.Failed[0].Name != bad || !strings.Contains(o.Failed[0].Err.Error(), "bogus") {
		t.Fatalf("Failed = %+v", o.Failed)
	}
	if len(o.Sources) != 2 || o.Sources[0].Name() != "desk" || o.Sources[1].Name() != "pipeline" {
		t.Fatalf("sources = %d", len(o.Sources))
	}
}

// --- desk runs ---

// TestDeskRunsJSON: one array of {source, name, kind, live, updated, exit,
// partial}, the spec run and a skipped desk item both listed. A waiting item
// is not a run yet, so it is not listed.
func TestDeskRunsJSON(t *testing.T) {
	f := newDeskFixture(t)
	a := f.queue(t, "echo hi\n")
	waiting := f.queue(t, "echo later\n")
	d, err := desk.Open(f.deskDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Skip(a.Name, "operator"); err != nil {
		t.Fatal(err)
	}
	_ = d.Close()
	out, errOut, code := runDeskRoot(t, "desk", "runs", "--json", "--spec", f.spec)
	if code != 0 {
		t.Fatalf("code %d, stderr %q", code, errOut)
	}
	var rows []map[string]any
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("not one JSON array: %v\n%s", err, out)
	}
	want := []string{"exit", "kind", "live", "name", "partial", "source", "updated"}
	byName := map[string]map[string]any{}
	for _, r := range rows {
		keys := make([]string, 0, len(r))
		for k := range r {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		if !slices.Equal(keys, want) {
			t.Fatalf("keys %v, want %v", keys, want)
		}
		byName[r["name"].(string)] = r
	}
	run := byName["alpha-7"]
	if run == nil || run["source"] != "pipeline" || run["kind"] != "log" || run["live"] != "ended" ||
		run["exit"] != float64(3) || run["updated"] != "2026-10-05T14:02:11Z" || run["partial"] != false {
		t.Fatalf("alpha-7 = %v", run)
	}
	item := byName[a.Name]
	if item == nil {
		t.Fatalf("%s missing from\n%s", a.Name, out)
	}
	if item["source"] != "desk" || item["kind"] != "desk" || item["live"] != "skipped" || item["exit"] != nil {
		t.Fatalf("%s = %v", a.Name, item)
	}
	if byName[waiting.Name] != nil || len(rows) != 2 {
		t.Fatalf("listed %d runs, want alpha-7 and %s only:\n%s", len(rows), a.Name, out)
	}
}

func TestDeskRunsText(t *testing.T) {
	f := newDeskFixture(t)
	out, errOut, code := runDeskRoot(t, "desk", "runs", "--no-icons", "--spec", f.spec)
	if code != 0 {
		t.Fatalf("code %d, stderr %q", code, errOut)
	}
	if !strings.HasPrefix(out, "x 3       alpha-7 ") {
		t.Fatalf("row = %q", out)
	}
}

// TestABadSpecFailsOnItsOwn: the other sources are still listed, the bad
// spec is named on stderr with its path and key, and the exit is 1.
func TestABadSpecFailsOnItsOwn(t *testing.T) {
	f := newDeskFixture(t)
	bad := f.writeSpec(t, "bad.toml", strings.Replace(fmt.Sprintf(deskSpecBody, "other", "", ""), `runs = "runs/*"`, `runs = "/abs/*"`, 1))
	for _, asJSON := range []bool{false, true} {
		args := []string{"desk", "runs", "--spec", bad, "--spec", f.spec}
		if asJSON {
			args = append(args, "--json")
		}
		out, errOut, code := runDeskRoot(t, args...)
		if code != 1 {
			t.Fatalf("json=%v: code %d, want 1", asJSON, code)
		}
		if !strings.Contains(out, "alpha-7") {
			t.Fatalf("json=%v: the good spec's run is missing: %q", asJSON, out)
		}
		if !strings.Contains(errOut, `run spec "`+bad+`": runs: `) {
			t.Fatalf("json=%v: stderr %q does not name the path and key", asJSON, errOut)
		}
		if asJSON {
			var rows []map[string]any
			if err := json.Unmarshal([]byte(out), &rows); err != nil || len(rows) != 1 {
				t.Fatalf("stdout is not the array of what loaded: %v %q", err, out)
			}
		}
	}
}

// --- desk show ---

func showJSON(t *testing.T, args ...string) (map[string]any, string, int) {
	t.Helper()
	out, errOut, code := runDeskRoot(t, append([]string{"desk", "show"}, args...)...)
	var v map[string]any
	if code == 0 {
		if err := json.Unmarshal([]byte(out), &v); err != nil {
			t.Fatalf("not JSON: %v\n%s", err, out)
		}
	}
	return v, errOut, code
}

func TestDeskShowJSONAndReplay(t *testing.T) {
	f := newDeskFixture(t)
	full, errOut, code := showJSON(t, "alpha-7", "--json", "--spec", f.spec)
	if code != 0 {
		t.Fatalf("code %d: %s", code, errOut)
	}
	keys := make([]string, 0, len(full))
	for k := range full {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	want := []string{"checks", "counts", "edges", "exit", "exit_meaning", "last_event", "live", "name", "partial", "restarts", "source", "steps"}
	if !slices.Equal(keys, want) {
		t.Fatalf("keys %v, want %v", keys, want)
	}
	if full["exit"] != float64(3) || full["exit_meaning"] != "audit rejected" || full["live"] != "ended" {
		t.Fatalf("show = %v", full)
	}
	steps := full["steps"].([]any)
	build := steps[1].(map[string]any)
	if build["id"] != "build" || build["status"] != "closed" || build["rejects"] != float64(1) || build["end"] != "2026-10-05T14:02:00Z" {
		t.Fatalf("build = %v", build)
	}
	check := full["checks"].([]any)[0].(map[string]any)
	if check["seen"] != true || check["passed"] != false {
		t.Fatalf("check = %v", check)
	}
	if c := full["counts"].(map[string]any); c["events"] != float64(len(deskEvents)) {
		t.Fatalf("counts = %v", c)
	}

	at, _, code := showJSON(t, "alpha-7", "--at", "3", "--json", "--spec", f.spec)
	if code != 0 {
		t.Fatal(code)
	}
	b := at["steps"].([]any)[1].(map[string]any)
	if at["exit"] != nil || b["status"] != "running" || b["end"] != nil || at["counts"].(map[string]any)["events"] != float64(3) {
		t.Fatalf("--at 3 = %v", at)
	}
	past, _, _ := showJSON(t, "alpha-7", "--at", "999", "--json", "--spec", f.spec)
	if past["counts"].(map[string]any)["events"] != float64(len(deskEvents)) || past["exit"] != float64(3) {
		t.Fatalf("--at past the end = %v", past)
	}
	if _, _, code := runDeskRoot(t, "desk", "show", "alpha-7", "--at", "-1", "--spec", f.spec); code != 2 {
		t.Fatalf("--at -1 exit %d, want 2", code)
	}
}

func TestDeskShowText(t *testing.T) {
	f := newDeskFixture(t)
	out, errOut, code := runDeskRoot(t, "desk", "show", "alpha-7", "--no-icons", "--spec", f.spec)
	if code != 0 {
		t.Fatalf("code %d: %s", code, errOut)
	}
	for _, want := range []string{
		"pipeline/alpha-7 · ended · exit 3 (audit rejected)\n",
		"  + build  closed · attempt 1 · rej 1/3 · 1m0s\n",
		"  check audit ok?: failed\n",
		"7 events\n",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in\n%s", want, out)
		}
	}
}

// TestASharedNameNeedsFrom: two sources holding alpha-7 exit 2 without
// --from, and --from picks one.
func TestASharedNameNeedsFrom(t *testing.T) {
	f := newDeskFixture(t)
	other := f.writeSpec(t, "other.toml", fmt.Sprintf(deskSpecBody, "other", "audit ok?", "audit rejected"))
	out, errOut, code := runDeskRoot(t, "desk", "show", "alpha-7", "--spec", f.spec, "--spec", other)
	if code != 2 || out != "" {
		t.Fatalf("code %d, stdout %q; want 2 and nothing", code, out)
	}
	if !strings.Contains(errOut, `run "alpha-7" is in sources pipeline and other; pass --from`) {
		t.Fatalf("stderr = %q", errOut)
	}
	v, errOut, code := showJSON(t, "alpha-7", "--from", "other", "--json", "--spec", f.spec, "--spec", other)
	if code != 0 || v["source"] != "other" {
		t.Fatalf("--from other: code %d, %v, %s", code, v, errOut)
	}
	_, errOut, code = runDeskRoot(t, "desk", "show", "alpha-7", "--json", "--spec", f.spec, "--spec", other)
	var obj jsonFailureObject
	if code != 2 || json.Unmarshal([]byte(errOut), &obj) != nil || !strings.Contains(obj.Error, "pass --from") {
		t.Fatalf("--json: code %d, stderr %q; want one failure object", code, errOut)
	}
	if _, _, code := runDeskRoot(t, "desk", "show", "alpha-7", "--from", "nowhere", "--spec", f.spec); code != 2 {
		t.Fatalf("unknown --from exit %d, want 2", code)
	}
	// Runs with the same name in two sources carry @source in the list.
	out, _, _ = runDeskRoot(t, "desk", "runs", "--no-icons", "--spec", f.spec, "--spec", other)
	if !strings.Contains(out, "alpha-7@pipeline") || !strings.Contains(out, "alpha-7@other") {
		t.Fatalf("runs = %q", out)
	}
}

func TestAnUnknownRunExits1(t *testing.T) {
	f := newDeskFixture(t)
	_, errOut, code := runDeskRoot(t, "desk", "show", "nope", "--spec", f.spec)
	if code != 1 || !strings.Contains(errOut, `no run named "nope"`) {
		t.Fatalf("code %d, stderr %q", code, errOut)
	}
	_, errOut, code = runDeskRoot(t, "desk", "show", termsafetest.Hostile("nope"), "--spec", f.spec)
	if code != 1 {
		t.Fatalf("hostile name: code %d", code)
	}
	termsafetest.AssertInert(t, "unknown run", errOut)
}

// TestHostileSpecTextIsEscaped: the spec is trusted, but its free text
// (an exit meaning, a check label, the source name) still reaches stdout
// escaped, in JSON and in text.
func TestHostileSpecTextIsEscaped(t *testing.T) {
	f := newDeskFixture(t)
	h := termsafetest.Hostile("x")
	spec := f.writeSpec(t, "hostile.toml", fmt.Sprintf(deskSpecBody, "pipeline"+h, "label"+h, "meaning"+h))
	for _, args := range [][]string{
		{"desk", "show", "alpha-7", "--json", "--spec", spec},
		{"desk", "show", "alpha-7", "--spec", spec},
		{"desk", "runs", "--json", "--spec", spec},
		{"desk", "runs", "--spec", spec},
	} {
		out, errOut, code := runDeskRoot(t, args...)
		if code != 0 {
			t.Fatalf("%v: code %d: %s", args, code, errOut)
		}
		if !strings.Contains(out, "alpha-7") {
			t.Fatalf("%v: run missing: %q", args, out)
		}
		termsafetest.AssertInert(t, strings.Join(args[1:3], " "), out)
	}
}

// --- the supervisor's argv ---

// TestTheSupervisorArgvResolves: the argv desk.Launch builds names the
// hidden desk _supervise command, parsed through the real root.
func TestTheSupervisorArgvResolves(t *testing.T) {
	root := newRoot(module.Deps{})
	argv := []string{"desk", "_supervise", "--dir", "/tmp/d", "--sha", strings.Repeat("a", 64), "--kind", "script", "01-job"}
	cmd, rest, err := root.Find(argv)
	if err != nil {
		t.Fatal(err)
	}
	if cmd.CommandPath() != "forgectl desk _supervise" || !cmd.Hidden {
		t.Fatalf("resolved %q (hidden %v)", cmd.CommandPath(), cmd.Hidden)
	}
	if err := cmd.ParseFlags(rest); err != nil {
		t.Fatal(err)
	}
	if got := cmd.Flags().Args(); !slices.Equal(got, []string{"01-job"}) {
		t.Fatalf("args = %v", got)
	}
}

const superviseProbeEnv = "FORGECTL_TEST_SUPERVISE_PROBE"

func init() { testHelperMains = append(testHelperMains, superviseProbe) }

// superviseProbe is this binary started by desk.Launch, which runs
// os.Executable() with the supervisor's argv. It records which command the
// real root resolves that argv to, then runs it.
func superviseProbe() (int, bool) {
	probe := os.Getenv(superviseProbeEnv)
	if probe == "" || len(os.Args) < 3 || os.Args[1] != "desk" {
		return 0, false
	}
	root := newRoot(module.Deps{})
	found, _, err := root.Find(os.Args[1:])
	line := fmt.Sprintf("find err=%v", err)
	if err == nil {
		line = fmt.Sprintf("path=%s hidden=%v", found.CommandPath(), found.Hidden)
	}
	root.SetArgs(os.Args[1:])
	rc := 0
	if runErr := root.Execute(); runErr != nil {
		rc = ExitCode(runErr)
	}
	line += fmt.Sprintf(" rc=%d\n", rc)
	_ = os.WriteFile(probe, []byte(line), 0o600) //nolint:gosec // G306: a test marker
	return 0, true
}

// TestTheDeskLaunchesTheRegisteredSupervisor: a real Launch re-executes this
// binary with the argv desk builds; the root resolves it to the hidden
// command, which runs the item to done/.
func TestTheDeskLaunchesTheRegisteredSupervisor(t *testing.T) {
	f := newDeskFixture(t)
	probe := filepath.Join(t.TempDir(), "probe")
	t.Setenv(superviseProbeEnv, probe)
	a := f.queue(t, "echo supervised\n")
	d, err := desk.Open(f.deskDir)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close() //nolint:errcheck // test cleanup
	c, err := d.Claim(a.Name, a.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Launch(c); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 20*time.Second, func() bool { return strings.Contains(readOr(probe), "rc=") })
	if got := readOr(probe); got != "path=forgectl desk _supervise hidden=true rc=0\n" {
		t.Fatalf("probe = %q", got)
	}
	snap, err := d.Scan()
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(snap.Done, func(it desk.Item) bool { return it.Name == a.Name })
	if i < 0 || snap.Done[i].ExitCode == nil || *snap.Done[i].ExitCode != 0 {
		t.Fatalf("item not done with exit 0: %+v", snap.Done)
	}
}

// TestTheRunsWriterEscapesWhatItIsGiven: every source today cleans its names
// at ingest, so the list path cannot carry a control character to the writer.
// The writer escapes one anyway, so a source that forgets cannot reach stdout.
func TestTheRunsWriterEscapesWhatItIsGiven(t *testing.T) {
	h := termsafetest.Hostile("x")
	var out bytes.Buffer
	runs := []*deskRun{{ref: runview.RunRef{Source: h, Name: h, Kind: runview.KindLog}, live: runview.LiveEnded}}
	if err := writeDeskRunsJSON(&out, runs); err != nil {
		t.Fatal(err)
	}
	termsafetest.AssertInert(t, "desk runs --json", out.String())
}
