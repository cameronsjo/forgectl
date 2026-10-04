package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	osexec "os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/doctor"
	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/resume"
)

// init keeps every in-process cli test off this machine's launchd: the
// doctor tests build `launch doctor`, whose update-hooks row would otherwise
// run `launchctl print` against the real gui domain.
func init() {
	hooksDoctorProbe = func(context.Context) (hooksDoctorFacts, error) {
		return hooksDoctorFacts{Agent: resume.AgentStatus{AgentState: resume.AgentState{Runs: -1}}}, nil
	}
}

// hooksFixture points every `resume hooks` seam at a temp tree.
type hooksFixture struct {
	root, state, agents string
	version             string
	runner              *exec.FakeRunner
	launchdLoaded       bool
	env                 map[string]string
}

func newHooksFixture(t *testing.T) *hooksFixture {
	t.Helper()
	f := &hooksFixture{root: t.TempDir(), version: "2.1.285"}
	f.state = filepath.Join(f.root, "state")
	f.agents = filepath.Join(f.root, "LaunchAgents")
	f.runner = &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		if name != "launchctl" {
			return "", nil
		}
		switch args[0] {
		case "print":
			if !f.launchdLoaded {
				return "", &exec.CommandError{Name: name, ExitCode: 113, Err: errors.New("exit status 113")}
			}
			return "state = not running\nruns = 1\nlast exit code = 0\n", nil
		case "bootstrap":
			f.launchdLoaded = true
		case "bootout":
			f.launchdLoaded = false
		}
		return "", nil
	}}
	claude := filepath.Join(f.root, "bin", "claude")
	if err := os.MkdirAll(filepath.Dir(claude), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(claude, []byte("#!/bin/sh\n"), 0o700); err != nil { // #nosec G306 -- a fake executable in a test temp dir
		t.Fatal(err)
	}
	t.Setenv("FORGECTL_CLAUDE_BIN", claude)

	prev := struct {
		dir, agents func() (string, error)
		exe         func() (string, error)
		look        func(string) (string, error)
		env         func(string) (string, bool)
		uid         func() int
		goos        string
		settle      time.Duration
		installed   func(context.Context, module.Deps) (string, error)
		signals     bool
	}{hooksDir, hooksAgentsDir, hooksExecutable, hooksLookPath, hooksLookupEnv, hooksUID, hooksGOOS, hooksSettle, installedVersionFn, hooksHandleSignals}
	hooksDir = func() (string, error) { return f.state, nil }
	hooksAgentsDir = func() (string, error) { return f.agents, nil }
	hooksExecutable = func() (string, error) { return "/opt/homebrew/bin/forgectl", nil }
	hooksLookPath = func(string) (string, error) { return "/opt/tools/bin/herdr", nil }
	f.env = map[string]string{}
	hooksLookupEnv = func(k string) (string, bool) {
		v, ok := f.env[k]
		return v, ok
	}
	hooksHandleSignals = false
	hooksUID = func() int { return 501 }
	hooksGOOS = "darwin"
	hooksSettle = time.Millisecond
	installedVersionFn = func(context.Context, module.Deps) (string, error) { return f.version, nil }
	t.Cleanup(func() {
		hooksDir, hooksAgentsDir, hooksExecutable, hooksLookPath, hooksLookupEnv = prev.dir, prev.agents, prev.exe, prev.look, prev.env
		hooksUID, hooksGOOS, hooksSettle, installedVersionFn, hooksHandleSignals = prev.uid, prev.goos, prev.settle, prev.installed, prev.signals
	})
	return f
}

func (f *hooksFixture) run(t *testing.T, cfgTOML string, args ...string) (string, error) {
	t.Helper()
	cfg, err := config.DecodeStrict([]byte(cfgTOML))
	if err != nil {
		t.Fatal(err)
	}
	cmd := newResumeHooksCmd(module.Deps{Cfg: cfg, Runner: f.runner})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err = cmd.Execute()
	return out.String(), err
}

const commandHookTOML = "[[resume.on_update]]\nharness = \"claude\"\ncommand = [\"/usr/local/bin/notify\", \"--loud\"]\n"

func (f *hooksFixture) commandCalls() []exec.Call {
	var out []exec.Call
	for _, c := range f.runner.Calls {
		if c.Name != "launchctl" {
			out = append(out, c)
		}
	}
	return out
}

func TestResumeHooksRunBaselineThenChange(t *testing.T) {
	f := newHooksFixture(t)
	out, err := f.run(t, commandHookTOML, "run")
	if err != nil || !strings.Contains(out, "baseline") || len(f.commandCalls()) != 0 {
		t.Fatalf("first run: %v, %q, calls %d", err, out, len(f.commandCalls()))
	}
	f.version = "2.1.286"
	out, err = f.run(t, commandHookTOML, "run")
	if err != nil {
		t.Fatalf("second run: %v\n%s", err, out)
	}
	calls := f.commandCalls()
	if len(calls) != 1 || calls[0].Env[resume.HookEnvNewVersion] != "2.1.286" || calls[0].Env[resume.HookEnvOldVersion] != "2.1.285" {
		t.Fatalf("calls %+v", calls)
	}
	out, err = f.run(t, commandHookTOML, "run")
	if err != nil || !strings.Contains(out, "unchanged") || len(f.commandCalls()) != 1 {
		t.Fatalf("third run fired again: %v %q", err, out)
	}
	status, err := f.run(t, commandHookTOML, "status", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var st hooksStatusDTO
	if err := json.Unmarshal([]byte(status), &st); err != nil {
		t.Fatalf("status --json: %v\n%s", err, status)
	}
	if st.Hooks != 1 || st.Recorded["claude"].Version != "2.1.286" || len(st.Runs) != 1 || st.Runs[0].Hook != "#1 command=notify" || st.Runs[0].Trigger != "manual" {
		t.Fatalf("status %+v", st)
	}
}

func TestResumeHooksRunFailingHookExits1(t *testing.T) {
	f := newHooksFixture(t)
	if _, err := f.run(t, commandHookTOML, "run"); err != nil {
		t.Fatal(err)
	}
	f.version = "2.1.286"
	f.runner.RunFunc = func(name string, _ []string) (string, error) {
		return "", &exec.CommandError{Name: name, ExitCode: 2, Stderr: "nope", Err: errors.New("exit status 2")}
	}
	out, err := f.run(t, commandHookTOML, "run")
	if ExitCode(err) != 1 || !strings.Contains(out, "failed (exit 2") {
		t.Fatalf("exit %d, %q", ExitCode(err), out)
	}
}

func TestResumeHooksRunRejectsBadConfig(t *testing.T) {
	f := newHooksFixture(t)
	_, err := f.run(t, "[[resume.on_update]]\nharness = \"codex\"\naction = \"restart\"\n", "run")
	if err == nil || !strings.Contains(err.Error(), "not supported yet") {
		t.Fatalf("err = %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(f.state, "state.json")); !os.IsNotExist(statErr) {
		t.Fatal("a bad config still recorded a version")
	}
}

// TestResumeHooksRestartAction is the done-when: with a restart hook
// configured, an update restarts the idle outdated session with nothing
// typed (every process and pane action is the fake's).
func TestResumeHooksRestartAction(t *testing.T) {
	env := &cliRestartEnv{}
	restartFixture(t, env)
	f := newHooksFixture(t)
	cfg := "[[resume.on_update]]\nharness = \"claude\"\naction = \"restart\"\n"
	f.version = "2.1.99"
	if _, err := f.run(t, cfg, "run"); err != nil {
		t.Fatal(err)
	}
	if env.terminated != 0 {
		t.Fatal("the baseline run restarted a session")
	}
	f.version = "2.1.100"
	out, err := f.run(t, cfg, "run")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if env.terminated != 1 || env.relaunched != 1 || !strings.Contains(out, "resumed") {
		t.Fatalf("terminated %d relaunched %d\n%s", env.terminated, env.relaunched, out)
	}
}

// A session whose herdr pane cannot be found makes the restart hook
// incomplete, so the watcher's bounded retries come back for it.
func TestResumeHooksRestartRetriesAGonePane(t *testing.T) {
	env := &cliRestartEnv{pane: "w0:p999", gone: map[string]bool{"w0:p999": true}}
	restartFixture(t, env)
	f := newHooksFixture(t)
	cfg := "[[resume.on_update]]\nharness = \"claude\"\naction = \"restart\"\n"
	f.version = "2.1.99"
	if _, err := f.run(t, cfg, "run"); err != nil {
		t.Fatal(err)
	}
	f.version = "2.1.100"
	out, err := f.run(t, cfg, "run")
	if err == nil || env.terminated != 0 || !strings.Contains(out, "pane-gone") {
		t.Fatalf("err %v terminated %d\n%s", err, env.terminated, out)
	}
	st, _, lerr := resume.FileHookStore{Dir: f.state}.Load("claude")
	if lerr != nil || len(st.Pending) == 0 {
		t.Fatalf("state %+v, %v; want the restart pending for a retry", st, lerr)
	}
}

func TestResumeHooksDryRunWritesNothing(t *testing.T) {
	f := newHooksFixture(t)
	out, err := f.run(t, commandHookTOML, "run", "--dry-run")
	if err != nil || !strings.Contains(out, "a real run records") || !strings.Contains(out, "would not fire hook #1 command=notify") {
		t.Fatalf("%v %q", err, out)
	}
	if _, err := os.Stat(f.state); !os.IsNotExist(err) {
		t.Fatal("dry run created the state directory")
	}
}

func TestResumeHooksInstall(t *testing.T) {
	f := newHooksFixture(t)
	out, err := f.run(t, commandHookTOML, "install")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	plist := resume.AgentPlistPath(f.agents, resume.HooksAgentLabel)
	data, err := os.ReadFile(plist) // #nosec G304 -- test temp path
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	for _, want := range []string{
		"<string>/opt/homebrew/bin/forgectl</string>",
		"<string>" + filepath.Join(f.root, "bin", "claude") + "</string>",
		filepath.Join(f.root, "bin") + ":/opt/tools/bin:/opt/homebrew/bin:/usr/bin",
		"<key>FORGECTL_CLAUDE_BIN</key>\n    <string>" + filepath.Join(f.root, "bin", "claude") + "</string>",
		"<key>FORGECTL_HERDR_BIN</key>\n    <string>/opt/tools/bin/herdr</string>",
		"<key>StartInterval</key>",
		"<string>" + f.state + "</string>",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("plist lacks %q\n%s", want, s)
		}
	}
	if strings.Contains(s, "HERDR_SOCKET_PATH") {
		t.Error("an unset variable was passed through")
	}
	if !f.launchdLoaded {
		t.Fatal("not loaded")
	}
	if runtime.GOOS == "darwin" {
		if plutil, err := osexec.LookPath("plutil"); err == nil {
			// plutil -lint only reads the file.
			if out, err := osexec.CommandContext(t.Context(), plutil, "-lint", plist).CombinedOutput(); err != nil { // #nosec G204 -- fixed tool, temp path
				t.Fatalf("plutil -lint: %v: %s", err, out)
			}
		}
	}
	out, err = f.run(t, commandHookTOML, "install")
	if err != nil || !strings.Contains(out, "already installed") {
		t.Fatalf("repeat install: %v %q", err, out)
	}
	out, err = f.run(t, commandHookTOML, "uninstall")
	if err != nil || !strings.Contains(out, "removed") || f.launchdLoaded {
		t.Fatalf("uninstall: %v %q", err, out)
	}
	out, err = f.run(t, commandHookTOML, "uninstall")
	if err != nil || !strings.Contains(out, "not installed") {
		t.Fatalf("repeat uninstall: %v %q", err, out)
	}
}

func TestResumeHooksInstallRefusesGoBuildBinary(t *testing.T) {
	f := newHooksFixture(t)
	hooksExecutable = func() (string, error) { return "/var/folders/x/T/go-build42/b001/exe/forgectl", nil }
	_, err := f.run(t, commandHookTOML, "install")
	if err == nil || !strings.Contains(err.Error(), "temporary `go run` build") {
		t.Fatalf("err = %v", err)
	}
	if len(f.runner.Calls) != 0 {
		t.Fatalf("launchctl was called: %+v", f.runner.Calls)
	}
	if _, err := os.Stat(f.agents); !os.IsNotExist(err) {
		t.Fatal("a plist was written")
	}
}

func TestResumeHooksInstallRefusesOffMacOS(t *testing.T) {
	f := newHooksFixture(t)
	hooksGOOS = "linux"
	if _, err := f.run(t, commandHookTOML, "install"); err == nil || !strings.Contains(err.Error(), "only on macOS") {
		t.Fatalf("err = %v", err)
	}
}

func TestResumeHooksInstallDryRun(t *testing.T) {
	f := newHooksFixture(t)
	out, err := f.run(t, commandHookTOML, "install", "--dry-run")
	if err != nil || !strings.Contains(out, "<plist version=\"1.0\">") {
		t.Fatalf("%v %q", err, out)
	}
	if len(f.runner.Calls) != 0 {
		t.Fatal("dry run called launchctl")
	}
	if _, err := os.Stat(f.agents); !os.IsNotExist(err) {
		t.Fatal("dry run wrote a plist")
	}
}

func TestHooksDoctorRow(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	loaded := resume.AgentStatus{Installed: true, AgentState: resume.AgentState{Loaded: true, State: "not running", Runs: 2, LastExit: "0"}}
	running := loaded
	running.State = "running"
	ok := hooksDoctorFacts{Agent: loaded, Now: now, HaveState: true, State: resume.HarnessState{Version: "2.1.285"}}
	withLast := func(outcome string) hooksDoctorFacts {
		f := ok
		f.LastRun, f.HaveLastRun = resume.HookRun{Hook: "#2 command=x", New: "2.1.285", Outcome: outcome}, true
		return f
	}
	pending := ok
	pending.State.Pending, pending.State.Attempts = []string{"#1 action=restart"}, 1
	stuck := ok
	stuck.Agent, stuck.InFlight, stuck.InFlightSince = running, true, now.Add(-2*time.Hour)
	busyNotStuck := stuck
	busyNotStuck.InFlightSince = now.Add(-5 * time.Minute)
	staleMarker := stuck
	staleMarker.Agent = loaded // not running: a SIGKILLed run's leftover marker
	cases := []struct {
		name       string
		configured int
		cfgErr     error
		f          hooksDoctorFacts
		probeErr   error
		want       doctor.State
		detail     string
	}{
		{"nothing configured or installed", 0, nil, hooksDoctorFacts{}, nil, doctor.StateOK, "no [[resume.on_update]] hooks configured"},
		{"hooks but no watcher", 1, nil, hooksDoctorFacts{}, nil, doctor.StateWarn, "not installed"},
		{"installed not loaded", 1, nil, hooksDoctorFacts{Agent: resume.AgentStatus{Installed: true}}, nil, doctor.StateWarn, "not loaded"},
		{"healthy", 1, nil, ok, nil, doctor.StateOK, "installed and loaded"},
		{"last hook ok", 1, nil, withLast(resume.OutcomeOK), nil, doctor.StateOK, "installed and loaded"},
		{"last hook failed", 1, nil, withLast(resume.OutcomeFailed), nil, doctor.StateWarn, "ended failed"},
		{"restart pending", 1, nil, pending, nil, doctor.StateWarn, "incomplete (1 of 3"},
		{"stuck run", 1, nil, stuck, nil, doctor.StateWarn, "may be stuck"},
		{"long but inside the timeout", 1, nil, busyNotStuck, nil, doctor.StateOK, "running"},
		{"stale marker, not running", 1, nil, staleMarker, nil, doctor.StateOK, "installed and loaded"},
		{"bad config", 0, errors.New("[[resume.on_update]] #1: bad"), ok, nil, doctor.StateWarn, "invalid"},
		{"probe failed", 1, nil, hooksDoctorFacts{}, errors.New("no home"), doctor.StateWarn, "could not check"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state, detail := hooksDoctorRow(tc.configured, 30*time.Minute, tc.cfgErr, tc.f, tc.probeErr)
			if state != tc.want || !strings.Contains(detail, tc.detail) {
				t.Fatalf("= %s %q, want %s containing %q", state, detail, tc.want, tc.detail)
			}
		})
	}
}

func TestResumeHooksInstallWarnings(t *testing.T) {
	f := newHooksFixture(t)
	hooksStat = func(p string) (fs.FileInfo, error) {
		if p == "/opt/tools/bin" {
			return fakeFileInfo{mode: fs.ModeDir | 0o777}, nil
		}
		return nil, fs.ErrNotExist
	}
	t.Cleanup(func() { hooksStat = os.Stat })
	out, err := f.run(t, commandHookTOML, "install", "--dry-run")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "not a symlink into a versions directory") {
		t.Errorf("no warning for a claude that is not a versions link:\n%s", out)
	}
	if !strings.Contains(out, "/opt/tools/bin can be changed by another user") {
		t.Errorf("no warning for a world-writable baked dir:\n%s", out)
	}
	if !strings.Contains(out, "watching claude at") {
		t.Errorf("install did not say which claude it watches:\n%s", out)
	}
}

func TestHooksTrigger(t *testing.T) {
	f := newHooksFixture(t)
	for _, tc := range []struct {
		env  map[string]string
		want string
	}{
		{map[string]string{}, "manual"},
		{map[string]string{"XPC_SERVICE_NAME": "local.forgectl.resume-hooks"}, "launchd"},
		{map[string]string{"XPC_SERVICE_NAME": "com.other"}, "manual"},
		{map[string]string{"CLAUDECODE": "1"}, "agent"},
		{map[string]string{"CLAUDE_CODE_SESSION_ID": "abc"}, "agent"},
		{map[string]string{"CLAUDECODE": ""}, "manual"},
	} {
		f.env = tc.env
		if got := hooksTrigger(); got != tc.want {
			t.Errorf("env %v: trigger %q, want %q", tc.env, got, tc.want)
		}
	}
}

func TestWritableByOthers(t *testing.T) {
	cases := []struct {
		mode        fs.FileMode
		owner, uid  int
		known, want bool
	}{
		{0o755, 0, 501, true, false},   // root's, not writable by others
		{0o755, 501, 501, true, false}, // ours, not writable by others
		{0o775, 501, 501, true, true},  // group-writable though ours (Homebrew's admin-group layout)
		{0o775, 0, 501, true, true},
		{0o757, 0, 501, true, true},
		{0o755, 502, 501, true, true},  // another user's: they can replace it
		{0o755, 501, 501, false, true}, // owner unreadable: warn rather than assume
	}
	for _, tc := range cases {
		if got := writableByOthers(tc.mode, tc.owner, tc.uid, tc.known); got != tc.want {
			t.Errorf("writableByOthers(%v, owner %d, uid %d, known %v) = %v", tc.mode, tc.owner, tc.uid, tc.known, got)
		}
	}
}

type fakeFileInfo struct{ mode fs.FileMode }

func (f fakeFileInfo) Name() string       { return "x" }
func (f fakeFileInfo) Size() int64        { return 0 }
func (f fakeFileInfo) Mode() fs.FileMode  { return f.mode }
func (f fakeFileInfo) ModTime() time.Time { return time.Time{} }
func (f fakeFileInfo) IsDir() bool        { return f.mode.IsDir() }
func (f fakeFileInfo) Sys() any           { return nil }

func TestResumeHooksAuditsInstallAndTrigger(t *testing.T) {
	f := newHooksFixture(t)
	if _, err := f.run(t, commandHookTOML, "install"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.run(t, commandHookTOML, "uninstall"); err != nil {
		t.Fatal(err)
	}
	f.env["XPC_SERVICE_NAME"] = resume.HooksAgentLabel
	if _, err := f.run(t, commandHookTOML, "run"); err != nil {
		t.Fatal(err)
	}
	f.version = "2.1.286"
	if _, err := f.run(t, commandHookTOML, "run"); err != nil {
		t.Fatal(err)
	}
	runs, err := resume.FileHookStore{Dir: f.state}.RecentRuns(10)
	if err != nil || len(runs) != 3 {
		t.Fatalf("runs %+v %v", runs, err)
	}
	if runs[0].Hook != "watcher install" || runs[1].Hook != "watcher uninstall" || runs[2].Trigger != "launchd" {
		t.Fatalf("audit %+v", runs)
	}
}

func TestResumeHooksRejectsTopLevelOnUpdate(t *testing.T) {
	f := newHooksFixture(t)
	_, err := f.run(t, "[[on_update]]\nharness = \"claude\"\naction = \"restart\"\n", "run")
	if err == nil || !strings.Contains(err.Error(), "[[resume.on_update]]") {
		t.Fatalf("err = %v", err)
	}
}

// A restart the watcher could not finish posts a notification naming the
// by-hand command; a resumed or waiting session posts nothing.
func TestResumeHooksRestartFailureNotifies(t *testing.T) {
	env := &cliRestartEnv{relaunchErr: errors.New("herdr call timed out")}
	restartFixture(t, env)
	var posted []string
	prev := hookNotify
	hookNotify = func(_ context.Context, _ module.Deps, title, body string) error {
		posted = append(posted, title+" | "+body)
		return nil
	}
	t.Cleanup(func() { hookNotify = prev })
	f := newHooksFixture(t)
	cfg := "[[resume.on_update]]\nharness = \"claude\"\naction = \"restart\"\n"
	f.version = "2.1.99"
	if _, err := f.run(t, cfg, "run"); err != nil {
		t.Fatal(err)
	}
	f.version = "2.1.100"
	if _, err := f.run(t, cfg, "run"); err == nil {
		t.Fatal("a failed relaunch must make the run incomplete")
	}
	if len(posted) != 1 || !strings.Contains(posted[0], "not restarted") || !strings.Contains(posted[0], "Run: forgectl resume ") {
		t.Fatalf("notifications = %q; want one naming the by-hand command", posted)
	}
}

func TestNotifyRestartFailureIgnoresOtherStates(t *testing.T) {
	called := 0
	prev := hookNotify
	hookNotify = func(context.Context, module.Deps, string, string) error { called++; return nil }
	t.Cleanup(func() { hookNotify = prev })
	for _, st := range []resume.RestartState{resume.StateResumed, resume.StateWaiting, resume.StateSkipped, resume.StateLeft} {
		notifyRestartFailure(context.Background(), module.Deps{}, resume.RestartEvent{State: st})
	}
	if called != 0 {
		t.Fatalf("posted %d notifications for non-failed states", called)
	}
}
