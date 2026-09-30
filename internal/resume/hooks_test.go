package resume

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/exec"
)

func TestDecide(t *testing.T) {
	cases := []struct {
		name               string
		recorded           string
		have, settledOK    bool
		settled            string
		want               ChangeKind
		wantOld, wantNewer string
	}{
		{"first run is a baseline", "", false, true, "2.1.285", ChangeBaseline, "", "2.1.285"},
		{"same version", "2.1.285", true, true, "2.1.285", ChangeNone, "2.1.285", "2.1.285"},
		{"new version", "2.1.284", true, true, "2.1.285", ChangeUpdated, "2.1.284", "2.1.285"},
		{"downgrade is a change too", "2.1.285", true, true, "2.1.284", ChangeUpdated, "2.1.285", "2.1.284"},
		{"unsettled fires nothing", "2.1.284", true, false, "", ChangeUnsettled, "2.1.284", ""},
		{"unsettled beats baseline", "", false, false, "", ChangeUnsettled, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := Decide(HarnessState{Version: tc.recorded}, tc.have, tc.settled, tc.settledOK)
			if d.Kind != tc.want || d.Old != tc.wantOld || d.New != tc.wantNewer {
				t.Fatalf("Decide = %+v, want {%s %q %q}", d, tc.want, tc.wantOld, tc.wantNewer)
			}
		})
	}
}

// reads returns a reader that yields vs in order, repeating the last.
func reads(vs ...string) (func(context.Context) (string, error), *int) {
	n := 0
	return func(context.Context) (string, error) {
		v := vs[min(n, len(vs)-1)]
		n++
		if v == "ERR" {
			return "", errors.New("symlink target missing")
		}
		return v, nil
	}, &n
}

func noSleep(context.Context, time.Duration) error { return nil }

func TestSettleVersion(t *testing.T) {
	cases := []struct {
		name   string
		reads  []string
		want   string
		wantOK bool
		errs   bool
	}{
		{"stable", []string{"B", "B"}, "B", true, false},
		{"moves once then settles", []string{"B", "C", "C"}, "C", true, false},
		{"reverts inside the window", []string{"B", "A", "A"}, "A", true, false},
		{"never settles", []string{"A", "B", "A", "B", "A", "B", "A"}, "", false, false},
		{"read error mid-update", []string{"B", "ERR"}, "", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			read, _ := reads(tc.reads...)
			got, ok, err := SettleVersion(context.Background(), read, noSleep, time.Second, 5)
			if (err != nil) != tc.errs || got != tc.want || ok != tc.wantOK {
				t.Fatalf("SettleVersion = %q, %v, %v; want %q, %v, err=%v", got, ok, err, tc.want, tc.wantOK, tc.errs)
			}
		})
	}
}

// hookFixture is a RunHooks request over a temp-dir store and fakes.
type hookFixture struct {
	dir      string
	runner   *exec.FakeRunner
	restarts int
	req      HooksRequest
}

func newHookFixture(t *testing.T, hooks []config.OnUpdateHook, versionReads ...string) *hookFixture {
	t.Helper()
	f := &hookFixture{dir: t.TempDir(), runner: &exec.FakeRunner{}}
	read, _ := reads(versionReads...)
	f.req = HooksRequest{
		Harness: "claude", Hooks: HookSpecs(hooks), Dir: f.dir,
		ReadVersion: read, Runner: f.runner, Sleep: noSleep,
		Restart: func(context.Context, time.Duration) (RestartResult, error) {
			f.restarts++
			return RestartResult{Finals: []RestartEvent{{State: StateResumed}}}, nil
		},
	}
	return f
}

func (f *hookFixture) record(t *testing.T, v string) {
	t.Helper()
	if err := (FileHookStore{Dir: f.dir}).Save("claude", HarnessState{Version: v}); err != nil {
		t.Fatal(err)
	}
}

func (f *hookFixture) state(t *testing.T) HarnessState {
	t.Helper()
	st, _, err := FileHookStore{Dir: f.dir}.Load("claude")
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func (f *hookFixture) recorded(t *testing.T) string {
	t.Helper()
	return f.state(t).Version
}

var (
	restartHook = config.OnUpdateHook{Harness: "claude", Action: "restart"}
	notifyHook  = config.OnUpdateHook{Harness: "claude", Command: []string{"/usr/local/bin/notify", "--title", "updated"}}
)

func TestRunHooksBaselineFiresNothing(t *testing.T) {
	f := newHookFixture(t, []config.OnUpdateHook{restartHook, notifyHook}, "2.1.285")
	res, err := RunHooks(context.Background(), f.req)
	if err != nil {
		t.Fatal(err)
	}
	if res.Decision.Kind != ChangeBaseline || len(res.Runs) != 0 || f.restarts != 0 || len(f.runner.Calls) != 0 {
		t.Fatalf("baseline run fired: decision %+v, runs %d, restarts %d, calls %d", res.Decision, len(res.Runs), f.restarts, len(f.runner.Calls))
	}
	if got := f.recorded(t); got != "2.1.285" {
		t.Fatalf("baseline not recorded: %q", got)
	}
}

func TestRunHooksChangeFiresEveryHook(t *testing.T) {
	f := newHookFixture(t, []config.OnUpdateHook{restartHook, notifyHook, {Harness: "claude", Command: []string{"/bin/other"}}}, "2.1.285")
	f.record(t, "2.1.284")
	res, err := RunHooks(context.Background(), f.req)
	if err != nil {
		t.Fatal(err)
	}
	if res.Decision.Kind != ChangeUpdated || f.restarts != 1 || len(f.runner.Calls) != 2 || len(res.Runs) != 3 {
		t.Fatalf("decision %+v, restarts %d, calls %d, runs %d", res.Decision, f.restarts, len(f.runner.Calls), len(res.Runs))
	}
	if got := f.recorded(t); got != "2.1.285" {
		t.Fatalf("new version not recorded: %q", got)
	}
	runs, err := FileHookStore{Dir: f.dir}.RecentRuns(10)
	if err != nil || len(runs) != 3 {
		t.Fatalf("audit trail = %d records, %v; want 3", len(runs), err)
	}
	for _, r := range runs {
		if r.Old != "2.1.284" || r.New != "2.1.285" || r.Harness != "claude" || r.Outcome != OutcomeOK || r.Time.Location() != time.UTC {
			t.Fatalf("audit record %+v", r)
		}
	}
}

// TestRunHooksEnvNotArgv pins the MUST: versions reach a command hook through
// its environment, and its argv is the configured one, byte for byte.
func TestRunHooksEnvNotArgv(t *testing.T) {
	f := newHookFixture(t, []config.OnUpdateHook{notifyHook}, "2.1.285")
	f.record(t, "2.1.284")
	if _, err := RunHooks(context.Background(), f.req); err != nil {
		t.Fatal(err)
	}
	if len(f.runner.Calls) != 1 {
		t.Fatalf("calls = %d", len(f.runner.Calls))
	}
	c := f.runner.Calls[0]
	if c.Name != notifyHook.Command[0] || !slices.Equal(c.Args, notifyHook.Command[1:]) {
		t.Fatalf("argv = %q %q, want the configured argv unchanged", c.Name, c.Args)
	}
	for _, a := range append([]string{c.Name}, c.Args...) {
		if strings.Contains(a, "2.1.28") || strings.Contains(a, "claude") {
			t.Fatalf("argv element %q carries a version or harness", a)
		}
	}
	want := map[string]string{HookEnvHarness: "claude", HookEnvOldVersion: "2.1.284", HookEnvNewVersion: "2.1.285"}
	for k, v := range want {
		if c.Env[k] != v {
			t.Fatalf("env %s = %q, want %q (env %v)", k, c.Env[k], v, c.Env)
		}
	}
}

func TestRunHooksRevertInsideSettleFiresNothing(t *testing.T) {
	// The symlink moves to 2.1.285 and back to 2.1.284 before it settles.
	f := newHookFixture(t, []config.OnUpdateHook{restartHook, notifyHook}, "2.1.285", "2.1.284", "2.1.284")
	f.record(t, "2.1.284")
	res, err := RunHooks(context.Background(), f.req)
	if err != nil {
		t.Fatal(err)
	}
	if res.Decision.Kind != ChangeNone || f.restarts != 0 || len(f.runner.Calls) != 0 {
		t.Fatalf("a reverted change fired: decision %+v, restarts %d, calls %d", res.Decision, f.restarts, len(f.runner.Calls))
	}
}

func TestRunHooksUnsettledFiresNothing(t *testing.T) {
	// A version that never stops moving: every pass exhausts its settle
	// rounds, and the run gives up after maxHookPasses without firing.
	var vs []string
	for i := range 40 {
		vs = append(vs, "2.1."+strconv.Itoa(300+i))
	}
	f := newHookFixture(t, []config.OnUpdateHook{notifyHook}, vs...)
	f.record(t, "2.1.284")
	res, err := RunHooks(context.Background(), f.req)
	if err != nil {
		t.Fatal(err)
	}
	if res.Decision.Kind != ChangeUnsettled || len(f.runner.Calls) != 0 || f.recorded(t) != "2.1.284" {
		t.Fatalf("unsettled run acted: %+v, calls %d, recorded %q", res.Decision, len(f.runner.Calls), f.recorded(t))
	}
}

// crashStore fails Record the first time, as a run killed between its hooks
// and its record would.
type crashStore struct {
	FileHookStore
	crashed bool
}

func (s *crashStore) Save(harness string, st HarnessState) error {
	if !s.crashed {
		s.crashed = true
		return errors.New("killed")
	}
	return s.FileHookStore.Save(harness, st)
}

func TestRunHooksCrashBeforeRecordRefires(t *testing.T) {
	f := newHookFixture(t, []config.OnUpdateHook{notifyHook}, "2.1.285")
	f.record(t, "2.1.284")
	f.req.Store = &crashStore{FileHookStore: FileHookStore{Dir: f.dir}}
	if _, err := RunHooks(context.Background(), f.req); err == nil {
		t.Fatal("first run: want the injected record failure")
	}
	if len(f.runner.Calls) != 1 {
		t.Fatalf("first run calls = %d, want 1", len(f.runner.Calls))
	}
	res, err := RunHooks(context.Background(), f.req)
	if err != nil {
		t.Fatal(err)
	}
	if res.Decision.Kind != ChangeUpdated || len(f.runner.Calls) != 2 {
		t.Fatalf("second run did not re-fire: %+v, calls %d", res.Decision, len(f.runner.Calls))
	}
	if f.recorded(t) != "2.1.285" {
		t.Fatalf("recorded %q after the re-fire", f.recorded(t))
	}
}

func TestRunHooksFailingHookDoesNotStopOthers(t *testing.T) {
	f := newHookFixture(t, []config.OnUpdateHook{
		{Harness: "claude", Command: []string{"/bin/fails"}},
		restartHook,
		notifyHook,
	}, "2.1.285")
	f.record(t, "2.1.284")
	f.runner.RunFunc = func(name string, _ []string) (string, error) {
		if name == "/bin/fails" {
			return "stdout secret", &exec.CommandError{Name: name, Stderr: "boom: bad thing", ExitCode: 3, Err: errors.New("exit status 3")}
		}
		return "", nil
	}
	f.req.Restart = func(context.Context, time.Duration) (RestartResult, error) {
		f.restarts++
		return RestartResult{}, ErrRestartBusy
	}
	res, err := RunHooks(context.Background(), f.req)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Runs) != 3 || len(f.runner.Calls) != 2 || f.restarts != 1 {
		t.Fatalf("runs %d, calls %d, restarts %d; every hook must run", len(res.Runs), len(f.runner.Calls), f.restarts)
	}
	got := []string{res.Runs[0].Outcome, res.Runs[1].Outcome, res.Runs[2].Outcome}
	if !slices.Equal(got, []string{OutcomeFailed, OutcomeFailed, OutcomeOK}) {
		t.Fatalf("outcomes %v", got)
	}
	if res.Runs[0].Exit != 3 || res.Runs[0].Detail != "boom: bad thing" {
		t.Fatalf("failed command record %+v", res.Runs[0])
	}
	if strings.Contains(res.Runs[0].Detail, "stdout secret") {
		t.Fatal("stdout reached the audit trail")
	}
	if !res.Failed() {
		t.Fatal("Failed() = false with failed hooks")
	}
	if f.recorded(t) != "2.1.285" {
		t.Fatal("a run whose hooks all ran must record the version even when some failed")
	}
}

func TestRunHooksRestartIncomplete(t *testing.T) {
	f := newHookFixture(t, []config.OnUpdateHook{restartHook}, "2.1.285")
	f.record(t, "2.1.284")
	f.req.Restart = func(_ context.Context, timeout time.Duration) (RestartResult, error) {
		if timeout != DefaultRestartTimeout {
			t.Errorf("restart timeout = %s, want the restart default", timeout)
		}
		return RestartResult{Finals: []RestartEvent{{}, {}, {}}, Failed: 1, Left: 1}, nil
	}
	res, err := RunHooks(context.Background(), f.req)
	if err != nil {
		t.Fatal(err)
	}
	if r := res.Runs[0]; r.Outcome != OutcomeIncomplete || r.Exit != 1 || !strings.Contains(r.Detail, "1 failed, 1 left") {
		t.Fatalf("record %+v", r)
	}
}

// ctxRunner blocks each command until its context ends.
type ctxRunner struct{ exec.FakeRunner }

func (r *ctxRunner) RunWithEnv(ctx context.Context, _ map[string]string, name string, _ ...string) (string, error) {
	<-ctx.Done()
	return "", &exec.CommandError{Name: name, ExitCode: -1, Err: ctx.Err()}
}

func TestRunHooksCommandTimeout(t *testing.T) {
	f := newHookFixture(t, []config.OnUpdateHook{{Harness: "claude", Command: []string{"/bin/sleep"}, TimeoutSeconds: 1}}, "2.1.285")
	f.record(t, "2.1.284")
	f.req.Runner = &ctxRunner{}
	res, err := RunHooks(context.Background(), f.req)
	if err != nil {
		t.Fatal(err)
	}
	if r := res.Runs[0]; r.Outcome != OutcomeTimeout || r.Exit != -1 {
		t.Fatalf("record %+v", r)
	}
}

func TestRunHooksOtherHarnessIgnored(t *testing.T) {
	f := newHookFixture(t, []config.OnUpdateHook{{Harness: "codex", Command: []string{"/bin/x"}}}, "2.1.285")
	f.record(t, "2.1.284")
	res, err := RunHooks(context.Background(), f.req)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Runs) != 0 || len(f.runner.Calls) != 0 {
		t.Fatal("a codex hook fired on a claude update")
	}
}

func TestRunHooksDryRunWritesNothing(t *testing.T) {
	f := newHookFixture(t, []config.OnUpdateHook{restartHook}, "2.1.285")
	f.record(t, "2.1.284")
	f.req.DryRun = true
	res, err := RunHooks(context.Background(), f.req)
	if err != nil {
		t.Fatal(err)
	}
	if res.Decision.Kind != ChangeUpdated || len(res.Planned) != 1 || len(res.Runs) != 0 || f.restarts != 0 {
		t.Fatalf("dry run: %+v", res)
	}
	if f.recorded(t) != "2.1.284" {
		t.Fatal("dry run recorded a version")
	}
	if _, err := os.Stat(filepath.Join(f.dir, hookRunsName)); !os.IsNotExist(err) {
		t.Fatal("dry run wrote the audit trail")
	}
	if _, err := os.Stat(filepath.Join(f.dir, hookLockName)); !os.IsNotExist(err) {
		t.Fatal("dry run took the lock")
	}
}

func TestHookIdentityOmitsArguments(t *testing.T) {
	h := HookSpecs([]config.OnUpdateHook{{Harness: "claude", Command: []string{"/opt/bin/curl", "https://hooks.example/T0KEN"}}})[0]
	if got := h.Identity(); got != "#1 command=curl" {
		t.Fatalf("Identity = %q", got)
	}
	if HookSpecs([]config.OnUpdateHook{restartHook})[0].Identity() != "#1 action=restart" {
		t.Fatal("restart identity")
	}
}

func TestOutputTail(t *testing.T) {
	if got := outputTail("fatal: could not read https://user:tok@host/x\nplain line"); strings.Contains(got, "tok") {
		t.Fatalf("credential line kept: %q", got)
	}
	if got := outputTail("a\x1b[31mred"); strings.ContainsRune(got, 0x1b) {
		t.Fatalf("escape kept: %q", got)
	}
	long := strings.Repeat("x", 1000) + "END"
	if got := outputTail(long); !strings.HasSuffix(got, "END") || len([]rune(got)) > hookTailRunes+1 {
		t.Fatalf("tail len %d, suffix ok %v", len([]rune(got)), strings.HasSuffix(got, "END"))
	}
}

func TestFileHookStore(t *testing.T) {
	dir := t.TempDir()
	s := FileHookStore{Dir: dir}
	if _, ok, err := s.Load("claude"); ok || err != nil {
		t.Fatalf("empty store: ok %v err %v", ok, err)
	}
	if err := os.WriteFile(filepath.Join(dir, hookStateName), []byte(`{"harnesses":{"claude":{"version":"2.1.x; rm"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Load("claude"); err == nil {
		t.Fatal("an unparseable recorded version was accepted")
	}
	for _, attempts := range []string{"-1000000", "-1", "4"} {
		if err := os.WriteFile(filepath.Join(dir, hookStateName), []byte(`{"harnesses":{"claude":{"version":"2.1.1","attempts":`+attempts+`}}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.Load("claude"); err == nil || !strings.Contains(err.Error(), "attempts") {
			t.Fatalf("attempts %s accepted: %v", attempts, err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, hookStateName), []byte(`{"harnesses":{"claude":{"version":"2.1.1","attempts":3}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Load("claude"); err != nil {
		t.Fatalf("attempts at the cap refused: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, hookStateName), []byte(`{`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Load("claude"); err == nil || !strings.Contains(err.Error(), "delete it") {
		t.Fatalf("corrupt state: %v", err)
	}
	if err := os.Remove(filepath.Join(dir, hookStateName)); err != nil {
		t.Fatal(err)
	}
	if err := s.Save("claude", HarnessState{Version: "2.1.1"}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dir, hookStateName))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("state file mode %v, %v", fi.Mode(), err)
	}
	for i := range 3 {
		if err := s.Append(HookRun{Hook: string(rune('a' + i))}); err != nil {
			t.Fatal(err)
		}
	}
	f, err := os.OpenFile(filepath.Join(dir, hookRunsName), os.O_APPEND|os.O_WRONLY, 0o600) // #nosec G304 -- test temp dir
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("{torn\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	runs, err := s.RecentRuns(2)
	if err != nil || len(runs) != 2 || runs[0].Hook != "b" || runs[1].Hook != "c" {
		t.Fatalf("RecentRuns = %+v, %v", runs, err)
	}
}

func TestHookRunsRotate(t *testing.T) {
	dir := t.TempDir()
	s := FileHookStore{Dir: dir}
	big := make([]byte, hookRunsMaxBytes)
	if err := os.WriteFile(filepath.Join(dir, hookRunsName), big, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(HookRun{Hook: "new"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, hookRunsName+".1")); err != nil {
		t.Fatalf("no rotation: %v", err)
	}
	runs, err := s.RecentRuns(5)
	if err != nil || len(runs) != 1 || runs[0].Hook != "new" {
		t.Fatalf("after rotation: %+v %v", runs, err)
	}
}

// TestRunHooksCancelledLeavesUnrecorded: the run's context is cancelled
// while the restart waits (in production, the run's signal context on
// SIGTERM, which the restart shares). The restart returns normally with a
// session left; RunHooks must start no further hook and record nothing.
func TestRunHooksCancelledLeavesUnrecorded(t *testing.T) {
	f := newHookFixture(t, []config.OnUpdateHook{restartHook, notifyHook}, "2.1.285")
	f.record(t, "2.1.284")
	ctx, cancel := context.WithCancel(context.Background())
	f.req.Restart = func(context.Context, time.Duration) (RestartResult, error) {
		cancel()
		return RestartResult{Finals: []RestartEvent{{State: StateLeft}}, Left: 1}, nil
	}
	_, err := RunHooks(ctx, f.req)
	if !errors.Is(err, ErrHooksInterrupted) {
		t.Fatalf("err = %v, want ErrHooksInterrupted", err)
	}
	if len(f.runner.Calls) != 0 {
		t.Fatal("a command hook started after the cancel")
	}
	if st := f.state(t); st.Version != "2.1.284" || len(st.Pending) != 0 {
		t.Fatalf("state %+v after a cancelled run", st)
	}
}

// TestRunHooksCommandArgsScrubbed runs a real child that echoes one of its
// own arguments to stderr and fails: the argument is the operator's (a token,
// say), so it must not reach the audit trail's stderr tail.
func TestRunHooksCommandArgsScrubbed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs /bin/sh")
	}
	const secret = "hook-secret-value-0123456789"
	hook := config.OnUpdateHook{Harness: "claude", Command: []string{"/bin/sh", "-c", `echo "failed with $0" >&2; exit 3`, secret}}
	f := newHookFixture(t, []config.OnUpdateHook{hook}, "2.1.285")
	f.record(t, "2.1.284")
	f.req.Runner = exec.OSRunner{}
	res, err := RunHooks(context.Background(), f.req)
	if err != nil {
		t.Fatal(err)
	}
	r := res.Runs[0]
	if r.Outcome != OutcomeFailed || r.Exit != 3 || !strings.Contains(r.Detail, "failed with") {
		t.Fatalf("record %+v", r)
	}
	if strings.Contains(r.Detail, secret) {
		t.Fatalf("an argument value reached the audit tail: %q", r.Detail)
	}
}

func TestDecideRetry(t *testing.T) {
	pending := HarnessState{Version: "2.1.285", Pending: []string{"#1 action=restart"}, Attempts: 1}
	if d := Decide(pending, true, "2.1.285", true); d.Kind != ChangeRetry || len(d.Retry) != 1 || d.Attempts != 1 {
		t.Fatalf("pending unchanged version: %+v", d)
	}
	pending.Attempts = MaxRestartAttempts
	if d := Decide(pending, true, "2.1.285", true); d.Kind != ChangeNone || len(d.GaveUp) != 1 {
		t.Fatalf("pending past the cap: %+v", d)
	}
	if d := Decide(pending, true, "2.1.286", true); d.Kind != ChangeUpdated {
		t.Fatalf("a new version must fire everything whatever is pending: %+v", d)
	}
}

func TestNextState(t *testing.T) {
	ids := map[string]bool{"#1 action=restart": true}
	ok := HookRun{Hook: "#1 action=restart", Outcome: OutcomeOK}
	bad := HookRun{Hook: "#1 action=restart", Outcome: OutcomeIncomplete}
	cmdBad := HookRun{Hook: "#2 command=x", Outcome: OutcomeFailed}
	cases := []struct {
		name string
		d    Decision
		runs []HookRun
		want HarnessState
	}{
		{"update, all ok", Decision{Kind: ChangeUpdated, New: "v2"}, []HookRun{ok}, HarnessState{Version: "v2"}},
		{"update, restart incomplete", Decision{Kind: ChangeUpdated, New: "v2"}, []HookRun{bad}, HarnessState{Version: "v2", Pending: []string{"#1 action=restart"}, Attempts: 1}},
		{"failed command never pends", Decision{Kind: ChangeUpdated, New: "v2"}, []HookRun{ok, cmdBad}, HarnessState{Version: "v2"}},
		{"retry still incomplete", Decision{Kind: ChangeRetry, New: "v2", Attempts: 1}, []HookRun{bad}, HarnessState{Version: "v2", Pending: []string{"#1 action=restart"}, Attempts: 2}},
		{"retry completes", Decision{Kind: ChangeRetry, New: "v2", Attempts: 2}, []HookRun{ok}, HarnessState{Version: "v2"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := NextState(tc.d, tc.runs, ids)
			if got.Version != tc.want.Version || got.Attempts != tc.want.Attempts || !slices.Equal(got.Pending, tc.want.Pending) {
				t.Fatalf("NextState = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestRunHooksRetriesIncompleteRestart: an incomplete restart is retried on
// later runs of the same version, the command hook is not, and a complete
// retry clears the pending entry.
func TestRunHooksRetriesIncompleteRestart(t *testing.T) {
	f := newHookFixture(t, []config.OnUpdateHook{restartHook, notifyHook}, "2.1.285")
	f.record(t, "2.1.284")
	incomplete := true
	f.req.Restart = func(context.Context, time.Duration) (RestartResult, error) {
		f.restarts++
		if incomplete {
			return RestartResult{Finals: []RestartEvent{{State: StateLeft}}, Left: 1}, nil
		}
		return RestartResult{Finals: []RestartEvent{{State: StateResumed}}}, nil
	}
	if _, err := RunHooks(context.Background(), f.req); err != nil {
		t.Fatal(err)
	}
	if st := f.state(t); st.Version != "2.1.285" || len(st.Pending) != 1 || st.Attempts != 1 {
		t.Fatalf("after the update: %+v", st)
	}
	res, err := RunHooks(context.Background(), f.req)
	if err != nil {
		t.Fatal(err)
	}
	if res.Decision.Kind != ChangeRetry || f.restarts != 2 || len(f.runner.Calls) != 1 {
		t.Fatalf("retry: %+v, restarts %d, command calls %d (the command must not re-fire)", res.Decision, f.restarts, len(f.runner.Calls))
	}
	if st := f.state(t); st.Attempts != 2 {
		t.Fatalf("after one retry: %+v", st)
	}
	incomplete = false
	if _, err := RunHooks(context.Background(), f.req); err != nil {
		t.Fatal(err)
	}
	if st := f.state(t); len(st.Pending) != 0 || st.Attempts != 0 || f.restarts != 3 {
		t.Fatalf("after a complete retry: %+v, restarts %d", st, f.restarts)
	}
	if res, _ := RunHooks(context.Background(), f.req); res.Decision.Kind != ChangeNone || f.restarts != 3 {
		t.Fatalf("a cleared state retried again: %+v", res.Decision)
	}
}

func TestRunHooksRetryGivesUp(t *testing.T) {
	f := newHookFixture(t, []config.OnUpdateHook{restartHook}, "2.1.285")
	f.record(t, "2.1.284")
	f.req.Restart = func(context.Context, time.Duration) (RestartResult, error) {
		f.restarts++
		return RestartResult{}, ErrRestartBusy
	}
	for range MaxRestartAttempts + 2 {
		if _, err := RunHooks(context.Background(), f.req); err != nil {
			t.Fatal(err)
		}
	}
	if f.restarts != MaxRestartAttempts {
		t.Fatalf("restarts = %d, want the cap %d", f.restarts, MaxRestartAttempts)
	}
}

// TestRunHooksRereadsAfterActing: an update that lands while the hooks run
// is handled in the same run, not left for a trigger that may never come.
func TestRunHooksRereadsAfterActing(t *testing.T) {
	f := newHookFixture(t, []config.OnUpdateHook{notifyHook}, "2.1.285", "2.1.285", "2.1.286")
	f.record(t, "2.1.284")
	res, err := RunHooks(context.Background(), f.req)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.runner.Calls) != 2 || f.recorded(t) != "2.1.286" || len(res.Decisions) != 2 {
		t.Fatalf("calls %d, recorded %q, passes %d", len(f.runner.Calls), f.recorded(t), len(res.Decisions))
	}
	if got := f.runner.Calls[1].Env[HookEnvOldVersion]; got != "2.1.285" {
		t.Fatalf("second pass old version = %q", got)
	}
}

func TestRunHooksPassesBounded(t *testing.T) {
	var vs []string
	for i := range 40 {
		vs = append(vs, "2.1."+strconv.Itoa(300+i/2))
	}
	f := newHookFixture(t, []config.OnUpdateHook{notifyHook}, vs...)
	f.record(t, "2.1.284")
	res, err := RunHooks(context.Background(), f.req)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Decisions) != maxHookPasses {
		t.Fatalf("passes = %d, want the bound %d", len(res.Decisions), maxHookPasses)
	}
}

func TestHookLockWaitsAndMarks(t *testing.T) {
	s := FileHookStore{Dir: t.TempDir()}
	release, err := s.Lock(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if pid, since, ok := s.InFlight(); !ok || pid != os.Getpid() || time.Since(since) > time.Minute {
		t.Fatalf("InFlight = %d %v %v", pid, since, ok)
	}
	waited := 0
	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel()
	if _, err := s.Lock(ctx, func() { waited++ }); err == nil {
		t.Fatal("a second lock was taken while the first was held")
	}
	if waited != 1 {
		t.Fatalf("onWait called %d times, want 1", waited)
	}
	release()
	if _, _, ok := s.InFlight(); ok {
		t.Fatal("marker left after release")
	}
}

// TestRunHooksTimeoutKillsHelpers: a command hook that times out takes the
// helpers it forked with it (its own process group is killed).
func TestRunHooksTimeoutKillsHelpers(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs /bin/sh and process groups")
	}
	pidFile := filepath.Join(t.TempDir(), "pid")
	hook := config.OnUpdateHook{Harness: "claude", TimeoutSeconds: 1,
		Command: []string{"/bin/sh", "-c", `sleep 30 >/dev/null 2>&1 & echo $! > "$0"; wait`, pidFile}}
	f := newHookFixture(t, []config.OnUpdateHook{hook}, "2.1.285")
	f.record(t, "2.1.284")
	f.req.Runner = exec.OSRunner{}
	res, err := RunHooks(context.Background(), f.req)
	if err != nil {
		t.Fatal(err)
	}
	if res.Runs[0].Outcome != OutcomeTimeout {
		t.Fatalf("record %+v", res.Runs[0])
	}
	data, err := os.ReadFile(pidFile) // #nosec G304 -- test temp dir
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	if !processGone(pid, 2*time.Second) {
		t.Fatalf("helper pid %d outlived the hook's timeout", pid)
	}
}
