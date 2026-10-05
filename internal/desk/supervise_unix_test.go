//go:build unix

package desk

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// parentHelper is the argv[1] that makes the test binary act as a desk that
// launches one supervisor and then waits to be killed, like a desk pane.
const parentHelper = "desk-test-parent"

// TestMain lets the test binary stand in for forgectl: `desk _supervise`
// runs the real supervisor, and parentHelper plays the desk.
func TestMain(m *testing.M) {
	supervisorArgv = func(dir, name string) ([]string, error) {
		return []string{os.Args[0], "desk", "_supervise", "--dir", dir, name}, nil
	}
	if len(os.Args) == 6 && os.Args[1] == "desk" && os.Args[2] == "_supervise" && os.Args[3] == "--dir" {
		os.Exit(RunSupervisor(os.Args[4], os.Args[5]))
	}
	if len(os.Args) == 4 && os.Args[1] == parentHelper {
		os.Exit(parentMain(os.Args[2], os.Args[3]))
	}
	os.Exit(m.Run())
}

func parentMain(dir, name string) int {
	d, err := Open(dir)
	if err != nil {
		fmt.Println("error", err)
		return 2
	}
	pid, err := d.Launch(name)
	if err != nil {
		fmt.Println("error", err)
		return 2
	}
	fmt.Println("launched", pid)
	time.Sleep(time.Minute) // until the test kills this "pane"
	return 0
}

// queue adds and claims an item, as a desk does on `y`.
func queue(t *testing.T, d *Desk, file, body string) *Claimed {
	t.Helper()
	a := addScript(t, d, file, body)
	scan(t, d)
	c, err := d.Claim(a.Name, a.SHA256)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	return c
}

// watchUntil follows name's events until the run ends or is lost.
func watchUntil(t *testing.T, d *Desk, name string, limit time.Duration) ([]string, WatchState) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), limit)
	defer cancel()
	var lines []string
	state, _, err := d.Watch(ctx, name, 0, 50*time.Millisecond, func(l string) { lines = append(lines, l) })
	if err != nil {
		t.Fatalf("Watch (%s after %v): %v; lines %q", state, limit, err, lines)
	}
	return lines, state
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	data, err := os.ReadFile(p) //nolint:gosec // G304: a test temp file
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestSupervisorRunsAScriptDetached(t *testing.T) {
	d := openDesk(t)
	c := queue(t, d, "hello.sh", "echo out; echo err >&2; [ -t 0 ] && echo HAS-TTY; exit 3\n")
	pid, err := d.Launch(c.Name)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	lines, state := watchUntil(t, d, c.Name, 20*time.Second)
	if state != WatchEnded {
		t.Fatalf("state = %s, lines %q", state, lines)
	}
	if want := []string{fmt.Sprintf("RUN-START id=%s pid=%d", c.Name, pid), "RUN-END rc=3 reason=failed"}; !slices.Equal(lines, want) {
		t.Errorf("events = %q, want %q", lines, want)
	}
	log := readFile(t, d.LogPath(c.Name))
	if !strings.Contains(log, "out\n") || !strings.Contains(log, "err\n") || !strings.HasSuffix(log, "\nEXIT=3\n") {
		t.Errorf("log = %q", log)
	}
	if strings.Contains(log, "HAS-TTY") {
		t.Error("a detached item had a terminal on stdin")
	}
	done := filepath.Join(d.Path(), DirDone)
	for _, f := range []string{c.Name + ".sh", c.Name + ".meta.json", c.Name + ".log", c.Name + ".events"} {
		if perm(t, filepath.Join(done, f)) != 0o600 {
			t.Errorf("done/%s mode %o", f, perm(t, filepath.Join(done, f)))
		}
	}
	s := scan(t, d)
	if len(s.Done) != 1 || s.Done[0].ExitCode == nil || *s.Done[0].ExitCode != 3 || s.Running != nil {
		t.Fatalf("snapshot = %+v", s)
	}
	m := s.Done[0].Meta
	if m.PID != pid || m.PIDStart == 0 && (isDarwin() || isLinux()) || m.StartedAt == nil || m.EndedAt == nil || m.SHA256 != c.SHA256 {
		t.Errorf("meta = %+v", m)
	}
}

func TestSupervisorRunsABatch(t *testing.T) {
	d := openDesk(t)
	src := filepath.Join(t.TempDir(), "demo.manifest")
	writeFile(t, src, `alpha -- echo "greeting=hello" >> "$STEP_OUT"
beta -- echo beta
gamma after=alpha -- echo "gamma got $OUT_alpha_greeting"; exit 3
delta after=gamma -- echo never
`, 0o600)
	a, err := d.Add(src, "a demo batch", "the live check's shape", false)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	scan(t, d)
	if _, err := d.Claim(a.Name, a.SHA256); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if _, err := d.Launch(a.Name); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	lines, state := watchUntil(t, d, a.Name, 20*time.Second)
	if state != WatchEnded {
		t.Fatalf("state = %s, lines %q", state, lines)
	}
	if !strings.HasPrefix(lines[0], "RUN-START id="+a.Name+" pid=") || !strings.HasSuffix(lines[0], " steps=4 jobs=4") {
		t.Errorf("RUN-START = %q", lines[0])
	}
	if last := lines[len(lines)-1]; last != "RUN-END rc=1 reason=failed ok=2 failed=1 skipped=1" {
		t.Errorf("RUN-END = %q", last)
	}
	for _, want := range []string{"STEP-SKIP id=delta reason=dep-failed:gamma"} {
		if !slices.Contains(lines, want) {
			t.Errorf("missing %q in %q", want, lines)
		}
	}
	log := readFile(t, d.LogPath(a.Name))
	if !strings.Contains(log, "[gamma] gamma got hello\n") || !strings.HasSuffix(log, "\nEXIT=1\n") {
		t.Errorf("log = %q", log)
	}
	for _, f := range []string{".manifest", ".d/summary.json", ".d/status.tsv", ".d/steps/alpha.log", ".d/steps/alpha.out"} {
		if _, err := os.Stat(filepath.Join(d.Path(), DirDone, a.Name+f)); err != nil {
			t.Errorf("done/%s%s: %v", a.Name, f, err)
		}
	}
	if s := scan(t, d); len(s.Done) != 1 || s.Done[0].Kind != KindBatch || *s.Done[0].ExitCode != 1 {
		t.Errorf("history = %+v", s.Done)
	}
}

func TestLaunchRefusesATTYItem(t *testing.T) {
	d := openDesk(t)
	src := filepath.Join(t.TempDir(), "sudo.sh")
	writeFile(t, src, "sudo -v\n", 0o600)
	a, err := d.Add(src, "refresh sudo", "needs a password", true)
	if err != nil {
		t.Fatal(err)
	}
	scan(t, d)
	if _, err := d.Claim(a.Name, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Launch(a.Name); err == nil || !strings.Contains(err.Error(), "TTY") {
		t.Fatalf("Launch = %v, want a TTY refusal", err)
	}
	if rc, err := d.supervise(a.Name); rc != 2 || err == nil {
		t.Errorf("supervise = %d, %v; want a refusal", rc, err)
	}
}

// The supervisor re-checks the running copy: bytes changed after the claim
// are skipped, never run.
func TestSupervisorRefusesARunningCopyThatChanged(t *testing.T) {
	d := openDesk(t)
	marker := filepath.Join(t.TempDir(), "ran")
	c := queue(t, d, "x.sh", "echo fine\n")
	writeFile(t, c.RecordPath, "touch "+marker+"\n", 0o600)
	if rc, err := d.supervise(c.Name); rc != 2 || !errors.Is(err, ErrChanged) {
		t.Fatalf("supervise = %d, %v; want ErrChanged", rc, err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("the changed bytes ran")
	}
	if s := scan(t, d); len(s.Skipped) != 1 || s.Skipped[0].Meta.SkipReason != SkipChanged {
		t.Errorf("skipped = %+v", s.Skipped)
	}
}

// The supervisor outlives the desk that launched it. The "desk" here is a
// helper process in its own process group; the test kills that whole group,
// as closing the desk's pane would, and the run still completes because the
// supervisor is in a session of its own.
func TestSupervisorSurvivesItsParentExiting(t *testing.T) {
	d := openDesk(t)
	pidFile := filepath.Join(t.TempDir(), "script.pid")
	c := queue(t, d, "slow.sh", "echo $$ > "+pidFile+"; sleep 2; echo finished\n")
	t.Cleanup(func() {
		if pid, err := strconv.Atoi(strings.TrimSpace(readFileOr(pidFile))); err == nil {
			_ = unix.Kill(-pid, unix.SIGKILL)
		}
	})

	parent := exec.CommandContext(t.Context(), os.Args[0], parentHelper, d.Path(), c.Name) //nolint:gosec // G204: the test binary itself
	parent.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	out, err := parent.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := parent.Start(); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil || !strings.HasPrefix(line, "launched ") {
		_ = unix.Kill(-parent.Process.Pid, unix.SIGKILL)
		_ = parent.Wait()
		t.Fatalf("parent said %q, %v", line, err)
	}
	// Let the supervisor get going, then take the "pane" down hard.
	waitFor(t, 10*time.Second, func() bool { return readFileOr(pidFile) != "" })
	if err := unix.Kill(-parent.Process.Pid, unix.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = parent.Wait()

	lines, state := watchUntil(t, d, c.Name, 20*time.Second)
	if state != WatchEnded || lines[len(lines)-1] != "RUN-END rc=0 reason=ok" {
		t.Fatalf("after the parent died: state %s, events %q", state, lines)
	}
	if log := readFile(t, d.LogPath(c.Name)); !strings.Contains(log, "finished\nEXIT=0\n") {
		t.Errorf("log = %q", log)
	}
}

// A supervisor killed mid-run leaves no RUN-END. Watch reports RUN-LOST, the
// queue shows the item as lost, and Skip clears it out of running/.
func TestWatchReportsRunLost(t *testing.T) {
	d := openDesk(t)
	pidFile := filepath.Join(t.TempDir(), "script.pid")
	c := queue(t, d, "hang.sh", "echo $$ > "+pidFile+"; sleep 30\n")
	t.Cleanup(func() {
		if pid, err := strconv.Atoi(strings.TrimSpace(readFileOr(pidFile))); err == nil {
			_ = unix.Kill(-pid, unix.SIGKILL)
		}
	})
	pid, err := d.Launch(c.Name)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, func() bool { return readFileOr(pidFile) != "" })
	if err := unix.Kill(pid, unix.SIGKILL); err != nil {
		t.Fatal(err)
	}

	lines, state := watchUntil(t, d, c.Name, 10*time.Second)
	if state != WatchLost {
		t.Fatalf("state = %s, lines %q", state, lines)
	}
	if want := fmt.Sprintf("RUN-LOST id=%s pid=%d", c.Name, pid); lines[len(lines)-1] != want {
		t.Errorf("last line = %q, want %q", lines[len(lines)-1], want)
	}
	s := scan(t, d)
	if len(s.Running) != 1 || s.Running[0].State != StateLost {
		t.Fatalf("running = %+v, want one lost item", s.Running)
	}
	if err := d.Skip(c.Name, SkipLost); err != nil {
		t.Fatalf("Skip lost: %v", err)
	}
	if s := scan(t, d); s.Running != nil || len(s.Skipped) != 1 {
		t.Errorf("after skip: running %v skipped %v", s.Running, s.Skipped)
	}
}

func TestSkipRefusesALiveRun(t *testing.T) {
	d := openDesk(t)
	c := queue(t, d, "x.sh", "echo x\n")
	if _, err := d.BeginRun(c.Name, os.Getpid()); err != nil {
		t.Fatal(err)
	}
	if err := d.Skip(c.Name, SkipLost); err == nil || !strings.Contains(err.Error(), "only a lost run") {
		t.Errorf("Skip = %v, want a refusal", err)
	}
}

// The foreground (TTY) path Task 2 drives: Claim, BeginRun with the desk's own
// pid, write the log, Finish. EXIT lands on its own line even after output
// with no trailing newline.
func TestForegroundRunRecordsLikeADetachedOne(t *testing.T) {
	d := openDesk(t)
	c := queue(t, d, "tty.sh", "echo hi\n")
	run, err := d.BeginRun(c.Name, os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, d.LogPath(c.Name), "prompted, no newline", 0o600)
	if err := run.Finish(0, "ok"); err != nil {
		t.Fatal(err)
	}
	if log := readFile(t, d.LogPath(c.Name)); log != "prompted, no newline\nEXIT=0\n" {
		t.Errorf("log = %q", log)
	}
	lines, state := watchUntil(t, d, c.Name, 5*time.Second)
	if state != WatchEnded || len(lines) != 2 {
		t.Errorf("state %s lines %q", state, lines)
	}
}

// A reused number must not run over an older run's history: BeginRun (the
// foreground path) refuses, and the old log is untouched.
func TestBeginRunRefusesANameDoneAlreadyHolds(t *testing.T) {
	d := openDesk(t)
	c := queue(t, d, "x.sh", "echo x\n")
	writeFile(t, d.LogPath(c.Name), "an older run\nEXIT=0\n", 0o600)
	if _, err := d.BeginRun(c.Name, os.Getpid()); !errors.Is(err, ErrRefused) {
		t.Fatalf("BeginRun = %v, want ErrRefused", err)
	}
	if got := readFile(t, d.LogPath(c.Name)); got != "an older run\nEXIT=0\n" {
		t.Errorf("old log changed: %q", got)
	}
	if _, err := os.Stat(d.EventsPath(c.Name)); err == nil {
		t.Error("an events file was created for the reused name")
	}
}

func TestWatchResumesFromSkip(t *testing.T) {
	d := openDesk(t)
	c := queue(t, d, "x.sh", "true\n")
	run, err := d.BeginRun(c.Name, os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Finish(0, "ok"); err != nil {
		t.Fatal(err)
	}
	var lines []string
	state, seen, err := d.Watch(t.Context(), c.Name, 1, 10*time.Millisecond, func(l string) { lines = append(lines, l) })
	if err != nil || state != WatchEnded || seen != 2 || !slices.Equal(lines, []string{"RUN-END rc=0 reason=ok"}) {
		t.Errorf("Watch = %s, %d, %v, lines %q", state, seen, err, lines)
	}
}

func TestWatchTreatsALegacyDoneItemAsEnded(t *testing.T) {
	d := openDesk(t)
	writeFile(t, filepath.Join(d.Path(), DirDone, "09-old.log"), "EXIT=0\n", 0o600)
	w, err := d.NewWatcher("09-old", 0)
	if err != nil {
		t.Fatal(err)
	}
	if lines, state, err := w.Poll(); err != nil || state != WatchEnded || lines != nil {
		t.Errorf("Poll = %q, %s, %v", lines, state, err)
	}
}

func TestProcessAliveSeesPIDReuse(t *testing.T) {
	pid := os.Getpid()
	start := ownStart(pid)
	if !processAlive(pid, start) {
		t.Fatal("this process reads as dead")
	}
	if !isDarwin() && !isLinux() {
		t.Skip("no process start time on this platform")
	}
	if start == 0 {
		t.Fatal("no start time recorded for this process")
	}
	if processAlive(pid, start+1) {
		t.Error("a pid whose start time differs reads as alive (pid reuse)")
	}
	if processAlive(0, 0) {
		t.Error("pid 0 reads as alive")
	}
}

func isDarwin() bool { return runtime.GOOS == "darwin" }
func isLinux() bool  { return runtime.GOOS == "linux" }

func readFileOr(p string) string {
	data, err := os.ReadFile(p) //nolint:gosec // G304: a test temp file
	if err != nil {
		return ""
	}
	return string(data)
}

func waitFor(t *testing.T, limit time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("condition not met in %v", limit)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// The run reads the verified bytes from a pipe, not running/<name>.sh: a
// line the item appends to its own running copy never executes.
func TestSupervisorDoesNotRunBytesWrittenToTheRunningCopy(t *testing.T) {
	d := openDesk(t)
	record := filepath.Join(d.Path(), DirRunning, "01-inject.sh")
	c := queue(t, d, "inject.sh", "echo first\necho \"zero=$0\"\necho 'echo INJECTED' >> "+record+"\necho last\n")
	if c.Name != "01-inject" {
		t.Fatalf("name = %s; the test predicts 01-inject", c.Name)
	}
	if _, err := d.Launch(c.Name); err != nil {
		t.Fatal(err)
	}
	if _, state := watchUntil(t, d, c.Name, 20*time.Second); state != WatchEnded {
		t.Fatalf("state = %s", state)
	}
	log := readFile(t, d.LogPath(c.Name))
	if strings.Contains(log, "INJECTED\n") {
		t.Errorf("a line appended to the running copy executed: %q", log)
	}
	if !strings.Contains(log, "first\n") || !strings.Contains(log, "last\nEXIT=0\n") {
		t.Errorf("log = %q", log)
	}
	if !strings.Contains(log, "zero="+ScriptFDPath+"\n") {
		t.Errorf("$0 is not %s: %q", ScriptFDPath, log)
	}
}

// A process the script left behind is stopped when the script exits, so it
// cannot append a late EXIT= line, and the rc comes from meta regardless.
func TestSupervisorEndsLeftoversSoTheyCannotRewriteTheExitCode(t *testing.T) {
	d := openDesk(t)
	c := queue(t, d, "leaky.sh", "(sleep 1; echo EXIT=0) &\necho failing\nexit 1\n")
	if _, err := d.Launch(c.Name); err != nil {
		t.Fatal(err)
	}
	if _, state := watchUntil(t, d, c.Name, 20*time.Second); state != WatchEnded {
		t.Fatalf("state = %s", state)
	}
	time.Sleep(1500 * time.Millisecond) // past the leftover's write, had it lived
	if log := readFile(t, d.LogPath(c.Name)); !strings.HasSuffix(log, "failing\nEXIT=1\n") {
		t.Errorf("the leftover wrote after the run ended: %q", log)
	}
	s := scan(t, d)
	if len(s.Done) != 1 || s.Done[0].ExitCode == nil || *s.Done[0].ExitCode != 1 {
		t.Errorf("history rc = %v, want 1", s.Done)
	}
}

// The rc recorded in meta outranks the log's last line; the line is only the
// fallback for a legacy item with no rc in meta.
func TestHistoryReadsTheExitCodeFromMetaFirst(t *testing.T) {
	d := openDesk(t)
	c := queue(t, d, "x.sh", "exit 1\n")
	run, err := d.BeginRun(c.Name, os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, d.LogPath(c.Name), "out\n", 0o600)
	if err := run.Finish(1, stepFailed); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(d.LogPath(c.Name), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("EXIT=0\n") // a late writer
	_ = f.Close()
	s := scan(t, d)
	if len(s.Done) != 1 || s.Done[0].ExitCode == nil || *s.Done[0].ExitCode != 1 {
		t.Fatalf("history rc = %+v, want 1 from meta", s.Done)
	}
	if s.Done[0].Meta.ExitCode == nil || *s.Done[0].Meta.ExitCode != 1 {
		t.Errorf("meta rc = %v", s.Done[0].Meta.ExitCode)
	}
}

// A second signal ends the grace period instead of re-arming it: a script
// that ignores SIGTERM is killed at once.
func TestSupervisorSecondSignalKillsAtOnce(t *testing.T) {
	d := openDesk(t)
	pidFile := filepath.Join(t.TempDir(), "script.pid")
	c := queue(t, d, "stubborn.sh", "trap '' TERM\necho $$ > "+pidFile+"\nsleep 30\n")
	t.Cleanup(func() {
		if pid, err := strconv.Atoi(strings.TrimSpace(readFileOr(pidFile))); err == nil {
			_ = unix.Kill(-pid, unix.SIGKILL)
		}
	})
	pid, err := d.Launch(c.Name)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, func() bool { return readFileOr(pidFile) != "" })
	if err := unix.Kill(pid, unix.SIGTERM); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	start := time.Now()
	if err := unix.Kill(pid, unix.SIGTERM); err != nil {
		t.Fatal(err)
	}
	lines, state := watchUntil(t, d, c.Name, 20*time.Second)
	if state != WatchEnded || !strings.HasSuffix(lines[len(lines)-1], "reason=interrupted") {
		t.Fatalf("state %s, events %q", state, lines)
	}
	if waited := time.Since(start); waited > 3*time.Second {
		t.Errorf("the second signal waited %v; it should kill at once", waited)
	}
}
