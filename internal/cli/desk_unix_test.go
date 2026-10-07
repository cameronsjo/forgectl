// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

//go:build unix

package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	osexec "os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/desk"
	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/theme"
	"github.com/cameronsjo/forgectl/internal/tui"
)

// deskRun runs `desk ARGS` against a fresh command tree and returns stdout,
// stderr and the error (ExitCode(err) is the process exit code).
func deskRun(t *testing.T, deps module.Deps, args ...string) (string, string, error) {
	t.Helper()
	var out, errOut bytes.Buffer
	err := deskRunTo(t, deps, &out, &errOut, args...)
	return out.String(), errOut.String(), err
}

// deskRunTo is deskRun with the writers given. It never lets a test reach a
// real herdr: both runners must be fakes, and the herdr session variables a
// test process may inherit from a herdr pane are blanked, so a layout test
// that forgets its stub is refused rather than splitting a live tab.
func deskRunTo(t *testing.T, deps module.Deps, out, errOut io.Writer, args ...string) error {
	t.Helper()
	if deps.Runner == nil {
		deps.Runner = &exec.FakeRunner{}
	}
	if deps.SensitiveRunner == nil {
		deps.SensitiveRunner = &exec.FakeSensitiveRunner{}
	}
	if _, ok := deps.Runner.(*exec.FakeRunner); !ok {
		t.Fatalf("desk tests must use exec.FakeRunner, got %T", deps.Runner)
	}
	if _, ok := deps.SensitiveRunner.(*exec.FakeSensitiveRunner); !ok {
		t.Fatalf("desk tests must use exec.FakeSensitiveRunner, got %T", deps.SensitiveRunner)
	}
	for _, k := range []string{"HERDR_ENV", "HERDR_SOCKET_PATH", "HERDR_PANE_ID"} {
		t.Setenv(k, "")
	}
	cmd := newDeskCmd(deps)
	cmd.SetOut(out)
	cmd.SetErr(errOut)
	cmd.SetArgs(args)
	return cmd.ExecuteContext(t.Context())
}

func deskDeps() module.Deps { return module.Deps{Theme: theme.Default()} }

// newDeskDir is an empty desk directory; DESK_DIR names it, so commands run
// without --dir resolve to it.
func newDeskDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "desk")
	stubDeskEnv(t, map[string]string{"DESK_DIR": dir})
	return dir
}

func stubDeskEnv(t *testing.T, env map[string]string) {
	t.Helper()
	prevGet, prevHome := deskGetenv, deskUserHome
	deskGetenv = func(k string) string { return env[k] }
	deskUserHome = func() (string, error) { return "/home/nobody", nil }
	t.Cleanup(func() { deskGetenv, deskUserHome = prevGet, prevHome })
}

func writeTemp(t *testing.T, name, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func openTestDesk(t *testing.T, dir string) *desk.Desk {
	t.Helper()
	d, err := desk.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

// queueItem adds a script through the CLI and returns its name and hash. It
// passes --allow-duplicate: tests queue several items with one filler body and
// need each to be its own item; duplicate detection has its own tests.
func queueItem(t *testing.T, file, body string) (name, sha string) {
	t.Helper()
	out, _, err := deskRun(t, deskDeps(), "add", writeTemp(t, file, body), "--what", "a test item", "--why", "a test", "--json", "--allow-duplicate")
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	var a deskAddJSON
	if err := json.Unmarshal([]byte(out), &a); err != nil {
		t.Fatalf("add --json: %v\n%s", err, out)
	}
	return a.Name, a.SHA256
}

// finishRun claims name and records a run that ended with rc, in process.
func finishRun(t *testing.T, d *desk.Desk, name, sha string, rc int) {
	t.Helper()
	run := startRun(t, d, name, sha, os.Getpid())
	if err := run.Finish(rc, "exit"); err != nil {
		t.Fatal(err)
	}
}

func startRun(t *testing.T, d *desk.Desk, name, sha string, pid int) *desk.Run {
	t.Helper()
	if _, err := d.Scan(); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Claim(name, sha); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	run, err := d.BeginRun(name, pid)
	if err != nil {
		t.Fatalf("BeginRun: %v", err)
	}
	return run
}

// markLost claims name and records a dead process as its owner, the way a
// supervisor that was killed leaves it: no lock held, no RUN-END.
func markLost(t *testing.T, d *desk.Desk, dir, name, sha string) {
	t.Helper()
	if _, err := d.Scan(); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Claim(name, sha); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	p := filepath.Join(dir, desk.DirRunning, name+".meta.json")
	data, err := os.ReadFile(p) //nolint:gosec // G304: a test desk under t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	var m desk.Meta
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	m.PID = deadPID(t)
	if data, err = json.Marshal(m); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func deadPID(t *testing.T) int {
	t.Helper()
	p, err := os.StartProcess("/bin/sh", []string{"sh", "-c", "exit 0"}, &os.ProcAttr{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Wait(); err != nil {
		t.Fatal(err)
	}
	return p.Pid
}

func wantExit(t *testing.T, err error, code int) {
	t.Helper()
	got := 0
	if err != nil {
		got = ExitCode(err)
	}
	if got != code {
		t.Fatalf("exit = %d (err %v), want %d", got, err, code)
	}
}

func jsonKeys(t *testing.T, raw []byte) []string {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("not one JSON object: %v\n%s", err, raw)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

var sha256Re = regexp.MustCompile(`^[0-9a-f]{64}$`)

func TestDeskAdd_RefusesMissingOrEmptyWhatAndWhy(t *testing.T) {
	dir := newDeskDir(t)
	src := writeTemp(t, "x.sh", "echo hi\n")
	for _, args := range [][]string{
		{"add", src, "--why", "y"},
		{"add", src, "--what", "   ", "--why", "y"},
		{"add", src, "--what", "w"},
		{"add", src, "--what", "w", "--why", ""},
	} {
		_, _, err := deskRun(t, deskDeps(), args...)
		wantExit(t, err, deskExitUsage)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a refused add touched the desk dir: %v", err)
	}
}

func TestDeskAdd_PrintsNameAndHash(t *testing.T) {
	newDeskDir(t)
	out, _, err := deskRun(t, deskDeps(), "add", writeTemp(t, "merge.sh", "#!/bin/bash\necho hi\n"), "--what", "merge it", "--why", "you own merges")
	wantExit(t, err, 0)
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 4 || lines[0] != "name=01-merge" || lines[1] != "kind=script" || !sha256Re.MatchString(strings.TrimPrefix(lines[2], "sha256=")) || lines[3] != "duplicate=false" {
		t.Fatalf("output = %q", out)
	}
	name, sha := queueItem(t, "second.sh", "echo two\n")
	if name != "02-second" || !sha256Re.MatchString(sha) {
		t.Errorf("second add = %s %s", name, sha)
	}
}

func TestDeskAdd_JSONContract(t *testing.T) {
	newDeskDir(t)
	out, _, err := deskRun(t, deskDeps(), "add", writeTemp(t, "a.sh", "echo a\n"), "--what", "w", "--why", "y", "--json")
	wantExit(t, err, 0)
	if got, want := jsonKeys(t, []byte(out)), []string{"duplicate", "kind", "name", "path", "sha256", "signal", "warnings"}; !reflect.DeepEqual(got, want) {
		t.Errorf("keys = %v, want %v", got, want)
	}
}

func TestDeskAdd_Stdin(t *testing.T) {
	newDeskDir(t)
	prevIn, prevTTY := deskStdin, deskStdinIsTerminal
	t.Cleanup(func() { deskStdin, deskStdinIsTerminal = prevIn, prevTTY })
	deskStdinIsTerminal = func() bool { return false }

	deskStdin = strings.NewReader("echo from stdin\n")
	out, _, err := deskRun(t, deskDeps(), "add", "-", "--name", "piped.sh", "--what", "w", "--why", "y")
	wantExit(t, err, 0)
	if !strings.HasPrefix(out, "name=01-piped\n") {
		t.Errorf("output = %q", out)
	}
	for _, args := range [][]string{
		{"add", "-", "--what", "w", "--why", "y"},                                         // no --name
		{"add", "-", "--name", "../x.sh", "--what", "w", "--why", "y"},                    // a path
		{"add", writeTemp(t, "f.sh", "x"), "--name", "f.sh", "--what", "w", "--why", "y"}, // --name without stdin
	} {
		_, _, err := deskRun(t, deskDeps(), args...)
		wantExit(t, err, deskExitUsage)
	}
	deskStdinIsTerminal = func() bool { return true }
	_, _, err = deskRun(t, deskDeps(), "add", "-", "--name", "t.sh", "--what", "w", "--why", "y")
	wantExit(t, err, deskExitUsage)
}

func TestDeskAdd_ManifestIsPlannedFirst(t *testing.T) {
	dir := newDeskDir(t)
	_, errOut, err := deskRun(t, deskDeps(), "add", writeTemp(t, "b.manifest", "a -- echo $OUT_b_k\nb -- true\n"), "--what", "w", "--why", "y")
	wantExit(t, err, 0)
	if !strings.Contains(errOut, "warning: a uses OUT_b_k but b is not an ancestor") {
		t.Errorf("stderr = %q, want the planner's warning", errOut)
	}
	_, _, err = deskRun(t, deskDeps(), "add", writeTemp(t, "cyc.manifest", "a after=b -- true\nb after=a -- true\n"), "--what", "w", "--why", "y")
	wantExit(t, err, 1)
	entries, _ := os.ReadDir(filepath.Join(dir, desk.DirPending))
	for _, e := range entries {
		if strings.Contains(e.Name(), "cyc") {
			t.Errorf("a manifest with a cycle was queued: %s", e.Name())
		}
	}
}

func TestDeskPlan(t *testing.T) {
	newDeskDir(t)
	src := writeTemp(t, "demo.manifest", "a -- echo k=v >> \"$STEP_OUT\"\nb -- true\nc after=a -- echo $OUT_a_k $OUT_b_x\n")
	out, _, err := deskRun(t, deskDeps(), "plan", src)
	wantExit(t, err, 0)
	for _, want := range []string{"name=demo.manifest\n", "steps=3\n", "order=a,b -> c\n", "warning: c uses OUT_b_x"} {
		if !strings.Contains(out, want) {
			t.Errorf("plan output %q lacks %q", out, want)
		}
	}
	if !regexp.MustCompile(`(?m)^sha256=[0-9a-f]{64}$`).MatchString(out) {
		t.Errorf("plan output %q has no full sha256 line", out)
	}

	name, _ := queueItemManifest(t, "q.manifest", "x -- true\ny after=x -- true\n")
	out, _, err = deskRun(t, deskDeps(), "plan", name, "--json")
	wantExit(t, err, 0)
	if got, want := jsonKeys(t, []byte(out)), []string{"name", "order", "sha256", "steps", "warnings", "waves"}; !reflect.DeepEqual(got, want) {
		t.Errorf("plan --json keys = %v, want %v", got, want)
	}
	var plan deskPlanJSON
	if err := json.Unmarshal([]byte(out), &plan); err != nil || plan.Order != "x -> y" || len(plan.Steps) != 2 || plan.Steps[1].After[0] != "x" {
		t.Errorf("plan --json = %s (%v)", out, err)
	}

	script, _ := queueItem(t, "s.sh", "true\n")
	_, _, err = deskRun(t, deskDeps(), "plan", script)
	wantExit(t, err, 1)
	_, _, err = deskRun(t, deskDeps(), "plan", "99-nothing")
	wantExit(t, err, 1)
}

func queueItemManifest(t *testing.T, file, body string) (string, string) {
	t.Helper()
	return queueItem(t, file, body)
}

func TestDeskStatus(t *testing.T) {
	dir := newDeskDir(t)
	waiting, sha := queueItem(t, "wait.sh", "echo wait\n")
	done, doneSHA := queueItem(t, "done.sh", "echo done\n")
	finishRun(t, openTestDesk(t, dir), done, doneSHA, 3)

	out, _, err := deskRun(t, deskDeps(), "status")
	wantExit(t, err, 0)
	if !strings.Contains(out, ": 1 waiting, 0 running, 1 done, 0 skipped\n") {
		t.Errorf("status header: %q", out)
	}
	if !regexp.MustCompile(`(?m)^waiting  ` + waiting + `  age=\d+s  sha256=` + sha[:12] + `  what="a test item"$`).MatchString(out) {
		t.Errorf("status lacks the waiting line: %q", out)
	}
	if !regexp.MustCompile(`(?m)^done     ` + done + `  age=\d+s  exit=3  took=\d+s$`).MatchString(out) {
		t.Errorf("status lacks the done line: %q", out)
	}

	out, _, err = deskRun(t, deskDeps(), "status", "--json")
	wantExit(t, err, 0)
	if got, want := jsonKeys(t, []byte(out)), []string{"dir", "done", "pending", "running", "skipped", "taken"}; !reflect.DeepEqual(got, want) {
		t.Errorf("status --json keys = %v, want %v", got, want)
	}
	var snap deskStatusJSON
	if err := json.Unmarshal([]byte(out), &snap); err != nil {
		t.Fatal(err)
	}
	if len(snap.Pending) != 1 || snap.Pending[0].SHA256 != sha || len(snap.Done) != 1 || snap.Done[0].ExitCode == nil || *snap.Done[0].ExitCode != 3 {
		t.Errorf("status --json = %+v", snap)
	}

	out, _, err = deskRun(t, deskDeps(), "status", waiting)
	wantExit(t, err, 0)
	for _, want := range []string{"name=" + waiting + "\n", "state=waiting\n", "sha256=" + sha + "\n", "what=a test item\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("detail %q lacks %q", out, want)
		}
	}
	out, _, err = deskRun(t, deskDeps(), "status", done, "--json")
	wantExit(t, err, 0)
	if got, want := jsonKeys(t, []byte(out)), []string{"events", "item", "log", "record", "steps", "summary"}; !reflect.DeepEqual(got, want) {
		t.Errorf("detail --json keys = %v, want %v", got, want)
	}
	_, _, err = deskRun(t, deskDeps(), "status", "77-missing")
	wantExit(t, err, 1)
	_, _, err = deskRun(t, deskDeps(), "status", "not a name")
	wantExit(t, err, deskExitUsage)
}

func fastWatch(t *testing.T) {
	t.Helper()
	prev := deskWatchInterval
	deskWatchInterval = 20 * time.Millisecond
	t.Cleanup(func() { deskWatchInterval = prev })
}

func TestDeskWatch_ExitsWithTheRun(t *testing.T) {
	fastWatch(t)
	dir := newDeskDir(t)
	d := openTestDesk(t, dir)
	for _, tc := range []struct {
		rc, exit int
	}{{0, 0}, {3, 1}} {
		name, sha := queueItem(t, "run.sh", "true\n")
		finishRun(t, d, name, sha, tc.rc)
		out, _, err := deskRun(t, deskDeps(), "watch", name)
		wantExit(t, err, tc.exit)
		lines := strings.Split(strings.TrimSpace(out), "\n")
		if len(lines) != 2 || !strings.HasPrefix(lines[0], "RUN-START id="+name) || !strings.HasPrefix(lines[1], "RUN-END rc=") {
			t.Errorf("rc %d: lines %q", tc.rc, lines)
		}
		// Resumed past RUN-END, the recorded exit is still the outcome.
		out, _, err = deskRun(t, deskDeps(), "watch", name, "--skip", "2")
		wantExit(t, err, tc.exit)
		if out != "" {
			t.Errorf("resumed watch printed %q", out)
		}
	}
}

func TestDeskWatch_RunLostExits1(t *testing.T) {
	fastWatch(t)
	dir := newDeskDir(t)
	name, sha := queueItem(t, "lost.sh", "true\n")
	markLost(t, openTestDesk(t, dir), dir, name, sha)
	out, _, err := deskRun(t, deskDeps(), "watch", name)
	wantExit(t, err, 1)
	if !strings.Contains(out, "RUN-LOST id="+name) {
		t.Errorf("output %q lacks RUN-LOST", out)
	}
}

func TestDeskWatch_DeadlineExits75WithAResumeLine(t *testing.T) {
	fastWatch(t)
	newDeskDir(t)
	name, _ := queueItem(t, "wait.sh", "true\n")
	start := time.Now()
	out, _, err := deskRun(t, deskDeps(), "watch", name, "--deadline", "1")
	wantExit(t, err, deskExitTempFail)
	if time.Since(start) > 10*time.Second {
		t.Errorf("watch took %v", time.Since(start))
	}
	if want := "resume=forgectl desk watch " + name + " --skip 0 --deadline 1\n"; out != want {
		t.Errorf("output = %q, want %q", out, want)
	}
}

func TestDeskWatch_SkippedAndMissing(t *testing.T) {
	fastWatch(t)
	newDeskDir(t)
	name, _ := queueItem(t, "skip.sh", "true\n")
	_, _, err := deskRun(t, deskDeps(), "skip", name, "--reason", "not now")
	wantExit(t, err, 0)
	_, _, err = deskRun(t, deskDeps(), "watch", name)
	wantExit(t, err, 1)
	_, _, err = deskRun(t, deskDeps(), "watch", "55-none")
	wantExit(t, err, 1)
	_, _, err = deskRun(t, deskDeps(), "watch", "bad name")
	wantExit(t, err, deskExitUsage)
	_, _, err = deskRun(t, deskDeps(), "watch", name, "--deadline", "-1")
	wantExit(t, err, deskExitUsage)
}

// deskWatchHelperEnv makes the test binary run one desk command as a child
// process, with the real stdout: the args are its JSON value.
const deskWatchHelperEnv = "FORGECTL_DESK_CLI_HELPER"

func TestDeskCLIHelperProcess(t *testing.T) {
	raw := os.Getenv(deskWatchHelperEnv)
	if raw == "" {
		t.Skip("runs only as a child of a desk CLI test")
	}
	var args []string
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		t.Fatal(err)
	}
	err := deskRunTo(t, deskDeps(), os.Stdout, os.Stderr, args...)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(ExitCode(err))
	}
	os.Exit(0)
}

// A watch whose reader closed the pipe stops at once with 141 and names the
// resume point on stderr, even though the run it watches is still going.
// The child writes to a real closed pipe on fd 1, the case where Go would
// otherwise kill it with SIGPIPE before it said anything.
func TestDeskWatch_ClosedPipeExits141(t *testing.T) {
	dir := newDeskDir(t)
	name, sha := queueItem(t, "live.sh", "true\n")
	run := startRun(t, openTestDesk(t, dir), name, sha, os.Getpid())
	t.Cleanup(func() { _ = run.Finish(0, "ok") })

	args, err := json.Marshal([]string{"watch", "--dir", dir, name})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	cmd := osexec.CommandContext(ctx, os.Args[0], "-test.run=^TestDeskCLIHelperProcess$") //nolint:gosec // G204: the test binary itself
	cmd.Env = append(os.Environ(), deskWatchHelperEnv+"="+string(args))
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	first, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || !strings.HasPrefix(first, "RUN-START id="+name) {
		t.Fatalf("first line %q, %v", first, err)
	}
	if err := stdout.Close(); err != nil { // the monitor goes away
		t.Fatal(err)
	}
	if err := run.Events.Emit("STEP-WARN id=x msg=after the reader left"); err != nil {
		t.Fatal(err)
	}
	err = cmd.Wait()
	var ee *osexec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("wait = %v, want an exit status", err)
	}
	if got := ee.ExitCode(); got != deskExitBrokenPipe {
		t.Fatalf("exit = %d (%v), want %d; stderr %q", got, ee, deskExitBrokenPipe, stderr.String())
	}
	if want := "resume with forgectl desk watch " + name + " --skip 1"; !strings.Contains(stderr.String(), want) {
		t.Errorf("stderr = %q, want %q", stderr.String(), want)
	}
}

// A FIFO given to add or plan is refused at once, never read (a blocking
// read would hang the agent's tool call).
func TestDeskAddAndPlanRefuseAFIFO(t *testing.T) {
	newDeskDir(t)
	tmp := t.TempDir()
	for _, f := range []string{"pipe.sh", "pipe.manifest"} {
		if err := syscall.Mkfifo(filepath.Join(tmp, f), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{
		{"add", filepath.Join(tmp, "pipe.sh"), "--what", "w", "--why", "y"},
		{"plan", filepath.Join(tmp, "pipe.manifest")},
	} {
		done := make(chan error, 1)
		go func() { _, _, err := deskRun(t, deskDeps(), args...); done <- err }()
		select {
		case err := <-done:
			wantExit(t, err, 1)
			if !errors.Is(err, desk.ErrRefused) {
				t.Errorf("%v: err = %v, want ErrRefused", args, err)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("%v hung on a FIFO", args)
		}
	}
}

// Text that becomes a script header or a skip note is plain: a control or
// bidi character exits 2, CR included.
func TestDeskFlagTextRefusesControls(t *testing.T) {
	newDeskDir(t)
	src := writeTemp(t, "x.sh", "true\n")
	for _, bad := range []string{"a\x1b[31mred", "a\rb", "a\nb", "a\u0085b", "a\u202eb"} {
		_, _, err := deskRun(t, deskDeps(), "add", src, "--what", bad, "--why", "y")
		wantExit(t, err, deskExitUsage)
		_, _, err = deskRun(t, deskDeps(), "add", src, "--what", "w", "--why", bad)
		wantExit(t, err, deskExitUsage)
	}
	name, _ := queueItem(t, "s.sh", "true\n")
	_, _, err := deskRun(t, deskDeps(), "skip", name, "--reason", "x\x1b]0;t\a")
	wantExit(t, err, deskExitUsage)
}

// A --dir that cannot be one shell word is refused before the watch starts,
// rather than dropped from the resume= line.
func TestDeskWatch_RefusesADirItCannotResume(t *testing.T) {
	stubDeskEnv(t, map[string]string{})
	_, _, err := deskRun(t, deskDeps(), "watch", "01-x", "--dir", filepath.Join(t.TempDir(), "desk\x01e"), "--deadline", "1")
	wantExit(t, err, deskExitUsage)
	if !strings.Contains(err.Error(), "resume= line") {
		t.Errorf("err = %v", err)
	}
}

// The review's probe: a meta file whose sha256 carries a terminal escape.
// readMeta refuses it, so `desk status` never prints it, in the list or the
// detail, whether the item is pending or skipped.
func TestDeskStatus_EscapeInSHAIsNeverPrinted(t *testing.T) {
	dir := newDeskDir(t)
	openTestDesk(t, dir) // lay out the protocol dirs
	hostile := `{"sha256":"\u001b]0;PWNED\u0007` + strings.Repeat("a", 40) + `","kind":"script"}`
	for _, sub := range []string{desk.DirPending, desk.DirSkipped} {
		name := map[string]string{desk.DirPending: "01-evil", desk.DirSkipped: "02-evil"}[sub]
		if err := os.WriteFile(filepath.Join(dir, sub, name+".sh"), []byte("# WHAT: x\n# WHY: y\ntrue\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, sub, name+".meta.json"), []byte(hostile), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"status"}, {"status", "01-evil"}, {"status", "02-evil"}, {"status", "--json"}} {
		out, _, err := deskRun(t, deskDeps(), args...)
		wantExit(t, err, 0)
		if strings.ContainsAny(out, "\x1b\a") || strings.Contains(out, "PWNED") {
			t.Errorf("%v printed the hostile hash: %q", args, out)
		}
	}
	out, _, _ := deskRun(t, deskDeps(), "status", "01-evil")
	if !strings.Contains(out, "state=refused\n") || !strings.Contains(out, "refusal=its meta's sha256 is not 64 lowercase hex characters\n") {
		t.Errorf("the item with the hostile meta should read as refused: %q", out)
	}
}

// A legacy done/: 40 protocol logs with an exit, and four old logs whose
// names have no NN- number. status and the frame show all 44, the four as no
// exit recorded; the verbs that act on an item refuse the legacy names.
func TestDeskLegacyDoneNames(t *testing.T) {
	dir := newDeskDir(t)
	openTestDesk(t, dir)
	done := filepath.Join(dir, desk.DirDone)
	for i := 1; i <= 40; i++ {
		if err := os.WriteFile(filepath.Join(done, fmt.Sprintf("%02d-job.log", i)), []byte("EXIT=0\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	legacy := []string{"07b-cleanup", "operator-grow", "nightly-t4-grow", "host.restart_x"}
	for _, n := range legacy {
		if err := os.WriteFile(filepath.Join(done, n+".log"), []byte("ran\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	out, _, err := deskRun(t, deskDeps(), "status", "--json")
	wantExit(t, err, 0)
	var snap deskStatusJSON
	if err := json.Unmarshal([]byte(out), &snap); err != nil {
		t.Fatal(err)
	}
	legacyRows := 0
	for _, it := range snap.Done {
		if it.Legacy {
			legacyRows++
			if it.Number != nil || it.ExitCode != nil {
				t.Errorf("%s: number %v exit %v, want both null", it.Name, it.Number, it.ExitCode)
			}
		}
	}
	if len(snap.Done) != 44 || legacyRows != 4 {
		t.Fatalf("status --json: %d done, %d legacy; want 44 and 4", len(snap.Done), legacyRows)
	}

	stubDeskEnv(t, map[string]string{"DESK_DIR": dir, "COLUMNS": "100", "LINES": "200"})
	frame, _, err := deskRun(t, deskDeps(), "--frame")
	wantExit(t, err, 0)
	rows := 0 // timeline entries: "│ ended with no exit recorded"
	for _, l := range strings.Split(frame, "\n") {
		if strings.Contains(l, "ended with no exit recorded") {
			rows++
		}
	}
	if rows != 4 {
		t.Errorf("the timeline shows %d no-exit rows, want 4:\n%s", rows, frame)
	}
	for _, n := range []string{"07b-cleanup", "operator-grow"} {
		if !strings.Contains(frame, n) {
			t.Errorf("the frame does not show %s whole:\n%s", n, frame)
		}
	}

	for _, args := range [][]string{
		{"watch", "07b-cleanup"},
		{"skip", "operator-grow", "--reason", "x"},
		{"status", "07b-cleanup"},
		{"_supervise", "--sha", strings.Repeat("0", 64), "--kind", "script", "operator-grow"},
	} {
		_, _, err := deskRun(t, deskDeps(), args...)
		if err == nil {
			t.Errorf("%v accepted a legacy name", args)
		}
	}
}

func TestDeskSkip(t *testing.T) {
	dir := newDeskDir(t)
	d := openTestDesk(t, dir)
	waiting, _ := queueItem(t, "w.sh", "true\n")
	_, _, err := deskRun(t, deskDeps(), "skip", waiting)
	wantExit(t, err, deskExitUsage)
	out, _, err := deskRun(t, deskDeps(), "skip", waiting, "--reason", "superseded by 03")
	wantExit(t, err, 0)
	if out != "skipped="+waiting+" reason=operator note=\"superseded by 03\"\n" {
		t.Errorf("output = %q", out)
	}

	lost, lostSHA := queueItem(t, "l.sh", "true\n")
	markLost(t, d, dir, lost, lostSHA)
	out, _, err = deskRun(t, deskDeps(), "skip", lost, "--reason", "supervisor died")
	wantExit(t, err, 0)
	if out != "skipped="+lost+" reason=lost note=\"supervisor died\"\n" {
		t.Errorf("output = %q", out)
	}

	live, liveSHA := queueItem(t, "r.sh", "true\n")
	startRun(t, d, live, liveSHA, os.Getpid())
	_, _, err = deskRun(t, deskDeps(), "skip", live, "--reason", "x")
	wantExit(t, err, 1)
	_, _, err = deskRun(t, deskDeps(), "skip", "66-none", "--reason", "x")
	wantExit(t, err, 1)

	out, _, err = deskRun(t, deskDeps(), "status", waiting)
	wantExit(t, err, 0)
	if !strings.Contains(out, "skip_reason=operator\nskipped_by=cli\nskip_note=superseded by 03\n") || !strings.Contains(out, "\nskipped_at=") {
		t.Errorf("detail = %q", out)
	}
}

func TestDeskPrune(t *testing.T) {
	newDeskDir(t)
	out, _, err := deskRun(t, deskDeps(), "prune", "--json")
	wantExit(t, err, 0)
	if got, want := jsonKeys(t, []byte(out)), []string{"days", "found", "removed"}; !reflect.DeepEqual(got, want) {
		t.Errorf("prune --json keys = %v, want %v", got, want)
	}
	out, _, err = deskRun(t, deskDeps(), "prune", "--days", "7")
	wantExit(t, err, 0)
	if out != "pruned=0 days=7\n" {
		t.Errorf("output = %q", out)
	}
	_, _, err = deskRun(t, deskDeps(), "prune", "--days", "0")
	wantExit(t, err, deskExitUsage)
}

func TestDeskDashboard_RefusesWithoutATerminal(t *testing.T) {
	dir := newDeskDir(t)
	prev := deskHasTerminal
	deskHasTerminal = func() bool { return false }
	t.Cleanup(func() { deskHasTerminal = prev })
	_, _, err := deskRun(t, deskDeps())
	wantExit(t, err, deskExitUsage)
	if !errors.Is(err, tui.ErrDeskNeedsTerminal) {
		t.Errorf("err = %v, want ErrDeskNeedsTerminal", err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the refusal created the desk dir: %v", err)
	}
}

func TestDeskFrame(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "desk")
	stubDeskEnv(t, map[string]string{"DESK_DIR": dir, "COLUMNS": "100", "LINES": "30"})
	t.Setenv("NO_COLOR", "1")
	queueItem(t, "frame.sh", "echo frame\n")
	out, _, err := deskRun(t, deskDeps(), "--frame")
	wantExit(t, err, 0)
	if strings.Contains(out, "\x1b") {
		t.Error("the frame carries escape sequences under NO_COLOR on a pipe")
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) > 30 {
		t.Errorf("frame has %d lines, want at most 30", len(lines))
	}
	for _, l := range lines {
		if n := utf8.RuneCountInString(l); n > 100 {
			t.Errorf("line is %d columns wide: %q", n, l)
		}
	}
	if !strings.Contains(out, "frame") || !strings.Contains(out, "queue") {
		t.Errorf("frame lacks the item or the queue panel:\n%s", out)
	}
}

// The core refuses off Unix; this build has it, so the gate must be open.
func TestDeskSupported(t *testing.T) {
	if !deskSupported {
		t.Fatal("deskSupported is false on a Unix build")
	}
}

// --no-icons draws the desk in ASCII (#1107): before the fix the frame was
// byte-identical with and without it. The flag is the root's persistent
// one, so the test mounts desk under a root that has it, as production does.
func TestDeskFrameNoIcons(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "desk")
	stubDeskEnv(t, map[string]string{"DESK_DIR": dir, "COLUMNS": "100", "LINES": "30"})
	t.Setenv("NO_COLOR", "1")
	queueItem(t, "frame.sh", "echo frame\n")
	for _, k := range []string{"HERDR_ENV", "HERDR_SOCKET_PATH", "HERDR_PANE_ID"} {
		t.Setenv(k, "")
	}
	frame := func(args ...string) string {
		t.Helper()
		root := &cobra.Command{Use: "forgectl"}
		root.PersistentFlags().Bool("no-icons", false, "")
		root.AddCommand(newDeskCmd(deskDeps()))
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetErr(io.Discard)
		root.SetArgs(append([]string{"desk", "--frame"}, args...))
		if err := root.ExecuteContext(t.Context()); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	icons, ascii := frame(), frame("--no-icons")
	if icons == ascii {
		t.Fatal("--no-icons changed nothing")
	}
	for _, r := range ascii {
		if r >= 0x80 && !strings.ContainsRune("·…±", r) {
			t.Errorf("--no-icons frame still draws %q", r)
			break
		}
	}
	il, al := strings.Split(icons, "\n"), strings.Split(ascii, "\n")
	if len(il) != len(al) {
		t.Fatalf("line counts differ: %d vs %d", len(il), len(al))
	}
	for i := range il {
		if utf8.RuneCountInString(il[i]) != utf8.RuneCountInString(al[i]) {
			t.Errorf("line %d changed width: %q vs %q", i, il[i], al[i])
		}
	}
}

// no_icons in the config draws the desk in ASCII too, as it does the hub.
func TestDeskNoIconsHonorsConfig(t *testing.T) {
	cmd := &cobra.Command{}
	deps := deskDeps()
	if deskNoIcons(cmd, deps) {
		t.Fatal("ASCII with neither the flag nor the config")
	}
	deps.Cfg.NoIcons = true
	if !deskNoIcons(cmd, deps) {
		t.Error("config no_icons did not select ASCII")
	}
}

// desk --help's key paragraph stays within 80 columns (#1107 review: a word
// was left alone on its own line).
func TestDeskHelpKeysParagraphWraps(t *testing.T) {
	out, _, err := deskRun(t, deskDeps(), "--help")
	wantExit(t, err, 0)
	_, para, ok := strings.Cut(out, "Dashboard keys:")
	para, _, _ = strings.Cut(para, "\n\n")
	if !ok {
		t.Fatalf("no dashboard keys paragraph:\n%s", out)
	}
	for _, l := range strings.Split(para, "\n") {
		if len(l) > 80 {
			t.Errorf("keys paragraph line is %d columns: %q", len(l), l)
		}
	}
	// Every dashboard key, including the timeline's t and the history's h.
	for _, want := range []string{"t the\ntimeline", "h the finished runs", "j/k move", "? the keys"} {
		if !strings.Contains(para, want) {
			t.Errorf("keys paragraph lacks %q:\n%s", want, para)
		}
	}
}
