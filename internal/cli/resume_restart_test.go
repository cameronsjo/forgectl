package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/resume"
)

const (
	restartSID       = "aaaa1111"
	restartPane      = "w9Z:p1"
	restartProcStart = "Wed Sep 30 01:41:04 2026"
)

// cliRestartEnv is a RestartEnv whose reads describe one ready session and
// whose actions are counted.
type cliRestartEnv struct {
	pid                              int
	pane                             string
	ancestors                        map[int]bool
	relaunchErr                      error
	stopped                          bool
	terminated, relaunched, prepared int
}

func (f *cliRestartEnv) ReadEntry(int) (resume.RegistryEntry, bool) {
	if f.stopped {
		return resume.RegistryEntry{}, false
	}
	return resume.RegistryEntry{Pid: f.pid, SessionID: restartSID, Status: "idle", ProcStart: restartProcStart}, true
}
func (f *cliRestartEnv) Alive(int) bool { return !f.stopped }
func (f *cliRestartEnv) Identity(int) (resume.ProcIdentity, error) {
	start, err := resume.ParseProcStart(restartProcStart)
	return resume.ProcIdentity{ExecPath: "/u/.local/bin/claude", Start: start}, err
}
func (f *cliRestartEnv) Pane(context.Context, string) (resume.PaneState, error) {
	st := resume.PaneState{Agent: "claude", AgentSession: restartSID, ForegroundPGID: f.pid, ShellPID: 7, ForegroundPIDs: []int{f.pid}}
	if f.stopped {
		st.ForegroundPGID = st.ShellPID
	}
	return st, nil
}
func (f *cliRestartEnv) Screen(context.Context, string) (string, error) {
	rule := strings.Repeat("─", 40)
	return "out\n" + rule + "\n❯\n" + rule + "\nstatus", nil
}
func (f *cliRestartEnv) Prepare(string) error                     { f.prepared++; return nil }
func (f *cliRestartEnv) ClearInput(context.Context, string) error { return nil }
func (f *cliRestartEnv) Terminate(int) error                      { f.terminated++; f.stopped = true; return nil }
func (f *cliRestartEnv) Relaunch(context.Context, string, string) error {
	f.relaunched++
	return f.relaunchErr
}
func (f *cliRestartEnv) LiveSession(string) (resume.RegistryEntry, bool) {
	if f.relaunched == 0 || f.relaunchErr != nil {
		return resume.RegistryEntry{}, false
	}
	return resume.RegistryEntry{Pid: 999, SessionID: restartSID, Version: "2.1.100", Live: true}, true
}

// restartFixture writes one idle outdated session (our own pid, so it is live)
// and stubs every seam the verb reaches.
func restartFixture(t *testing.T, env *cliRestartEnv) (storeDir string) {
	t.Helper()
	root := t.TempDir()
	sessions := filepath.Join(root, ".claude", "sessions")
	if err := os.MkdirAll(sessions, 0o700); err != nil {
		t.Fatal(err)
	}
	self := os.Getpid()
	env.pid = self
	if env.pane == "" {
		env.pane = restartPane
	}
	body := `{"pid":` + itoaCLI(self) + `,"sessionId":"` + restartSID + `","cwd":"/w/a","version":"2.1.99","status":"idle","procStart":"` + restartProcStart + `"}`
	if err := os.WriteFile(filepath.Join(sessions, itoaCLI(self)+".json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	prevPaths, prevInstalled, prevOverride := resumePaths, installedVersionFn, restartOverride
	storeDir = filepath.Join(root, "store")
	resumePaths = func() (resume.Paths, error) {
		return resume.Paths{ClaudeHome: filepath.Join(root, ".claude"), StoreDir: storeDir}, nil
	}
	installedVersionFn = func(context.Context, module.Deps) (string, error) { return "2.1.100", nil }
	restartOverride = func(r *resume.RestartRequest) {
		r.Env = func(string) (resume.RestartEnv, error) { return env, nil }
		r.Binary = func() (string, error) { return "/x/forgectl", nil }
		r.Lookup = func() resume.PaneLookup { return func(int) string { return env.pane } }
		r.Ancestors = func() map[int]bool { return env.ancestors }
		// The fixture pid is this test process; the real signal handling
		// would install process-wide handlers under `go test`.
		r.HandleSignals = false
	}
	t.Cleanup(func() {
		resumePaths, installedVersionFn, restartOverride = prevPaths, prevInstalled, prevOverride
	})
	return storeDir
}

func runRestartCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := newResumeRestartCmd(module.Deps{Runner: &exec.FakeRunner{}})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func TestResumeRestart_UsageErrors(t *testing.T) {
	env := &cliRestartEnv{}
	restartFixture(t, env)
	for name, args := range map[string][]string{
		"no selector":         {},
		"flag-shaped session": {"--outdated", "--session", "-rf"},
		"non-hex session":     {"--outdated", "--session", "zzzz"},
		"zero timeout":        {"--outdated", "--timeout", "0s"},
	} {
		if _, err := runRestartCmd(t, args...); ExitCode(err) != 2 {
			t.Errorf("%s: exit %d (%v), want 2", name, ExitCode(err), err)
		}
	}
	if env.terminated+env.relaunched+env.prepared != 0 {
		t.Fatal("a usage error acted")
	}
}

func TestResumeRestart_DryRunTouchesNothing(t *testing.T) {
	env := &cliRestartEnv{}
	restartFixture(t, env)
	out, err := runRestartCmd(t, "--outdated", "--dry-run")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "restart ") || !strings.Contains(out, restartSID) || !strings.Contains(out, "pane "+restartPane) {
		t.Errorf("dry-run output = %q", out)
	}
	if env.terminated+env.relaunched+env.prepared != 0 {
		t.Fatalf("dry-run acted: terminated=%d relaunched=%d prepared=%d", env.terminated, env.relaunched, env.prepared)
	}
}

func TestResumeRestart_RestartsAndExitsZero(t *testing.T) {
	env := &cliRestartEnv{}
	restartFixture(t, env)
	out, err := runRestartCmd(t, "--outdated", "--session", restartSID)
	if err != nil {
		t.Fatalf("err = %v\n%s", err, out)
	}
	if env.terminated != 1 || env.relaunched != 1 || !strings.Contains(out, "resumed") {
		t.Fatalf("terminated=%d relaunched=%d out=%q", env.terminated, env.relaunched, out)
	}
}

func TestResumeRestart_FailedRelaunchExitsOne(t *testing.T) {
	env := &cliRestartEnv{relaunchErr: errors.New("herdr down")}
	restartFixture(t, env)
	out, err := runRestartCmd(t, "--outdated")
	if ExitCode(err) != 1 {
		t.Fatalf("exit %d (%v), want 1", ExitCode(err), err)
	}
	if !strings.Contains(out, "failed") || !strings.Contains(out, "by hand: forgectl resume "+restartSID) {
		t.Errorf("out = %q; a failure names the by-hand command", out)
	}
}

func TestResumeRestart_SkipsAreNotErrors(t *testing.T) {
	env := &cliRestartEnv{}
	restartFixture(t, env)
	env.pane = ""
	out, err := runRestartCmd(t, "--outdated", "--timeout", time.Minute.String())
	if err != nil {
		t.Fatalf("a pane-less session is a skip, not a failure: %v", err)
	}
	if !strings.Contains(out, "skipped") || env.terminated != 0 {
		t.Errorf("out = %q, terminated = %d", out, env.terminated)
	}
}

// I-3: a run inside the session it would restart skips it — in the plan and
// in a real run.
func TestResumeRestart_NeverStopsItsOwnAncestor(t *testing.T) {
	env := &cliRestartEnv{}
	restartFixture(t, env)
	env.ancestors = map[int]bool{env.pid: true}
	out, err := runRestartCmd(t, "--outdated", "--dry-run")
	if err != nil || !strings.HasPrefix(out, "skip ") || !strings.Contains(out, "this run is inside it") {
		t.Fatalf("dry-run: err=%v out=%q", err, out)
	}
	out, err = runRestartCmd(t, "--outdated")
	if err != nil || env.terminated != 0 || !strings.Contains(out, "this run is inside it") {
		t.Fatalf("run: err=%v terminated=%d out=%q", err, env.terminated, out)
	}
}
