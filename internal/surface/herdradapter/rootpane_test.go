package herdradapter

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/surface/backend"
)

// processInfoJSON is a `pane process-info` reply. foreground is the process
// group that owns the terminal; a pane is idle when it equals shell.
func processInfoJSON(foreground, shell int, names ...string) []byte {
	type proc struct {
		Name string   `json:"name"`
		PID  int      `json:"pid"`
		Argv []string `json:"argv"`
	}
	procs := make([]proc, 0, len(names))
	for i, n := range names {
		pid := foreground
		if i > 0 {
			pid = foreground + i
		}
		procs = append(procs, proc{Name: n, PID: pid, Argv: []string{"-" + n}})
	}
	raw, err := json.Marshal(map[string]any{
		"result": map[string]any{
			"type": "pane_process_info",
			"process_info": map[string]any{
				"foreground_process_group_id": foreground,
				"shell_pid":                   shell,
				"foreground_processes":        procs,
			},
		},
	})
	if err != nil {
		panic(err)
	}
	return raw
}

func bootstrapSent(run *scriptedRunner) bool {
	_, sent := commandOfKind(run.calls(), exec.KindHerdrBootstrap)
	return sent
}

// TestAnIdleRootPaneReceivesTheBootstrap: a pane whose foreground group is the
// shell alone is idle, and the inspection targets the pane the create reported.
func TestAnIdleRootPaneReceivesTheBootstrap(t *testing.T) {
	run := newRunner().on(exec.KindHerdrPaneInspect, func() (exec.SensitiveResult, error) {
		return stdout(processInfoJSON(4242, 4242, "zsh")), nil
	})
	a := newTestAdapter(t, run, nil)
	spec, _ := newSpec(t)

	res := a.Start(context.Background(), spec)

	if res.Failed() {
		t.Fatalf("an idle root pane failed the launch: %v", causeClass(res))
	}
	if !bootstrapSent(run) {
		t.Fatal("the bootstrap was not sent to an idle root pane")
	}
	inspect, ok := commandOfKind(run.calls(), exec.KindHerdrPaneInspect)
	if !ok {
		t.Fatal("no pane inspection ran before the bootstrap")
	}
	if !hasArg(inspect, exec.Opaque(paneA)) {
		t.Error("the inspection did not target the pane the create reported")
	}
}

// TestABusyRootPaneNeverReceivesTheBootstrap is the trial's herdr-plus case:
// a layout started an agent in the root pane. The bootstrap — socket path and
// nonce — must not be typed there, the result must still carry the ref so the
// workspace is closed, and the operator must be told what holds the pane.
func TestABusyRootPaneNeverReceivesTheBootstrap(t *testing.T) {
	run := newRunner().on(exec.KindHerdrPaneInspect, func() (exec.SensitiveResult, error) {
		return stdout(processInfoJSON(5000, 4242, "claude")), nil
	})
	var warnings bytes.Buffer
	a := newTestAdapter(t, run, nil, WithWarnings(&warnings))
	a.idleInterval = 0
	spec, _ := newSpec(t)

	res := a.Start(context.Background(), spec)

	if bootstrapSent(run) {
		t.Fatal("the bootstrap was typed into a pane an agent holds")
	}
	if _, ok := res.Ref(); !ok {
		t.Fatal("a busy root pane left no reference; the workspace would be stranded")
	}
	if causeClass(res) != backend.FailureTargetBusy {
		t.Errorf("class = %v, want FailureTargetBusy", causeClass(res))
	}
	if !strings.Contains(warnings.String(), `"claude"`) {
		t.Errorf("warning %q does not name the process holding the pane", warnings.String())
	}
}

// TestAnUnreadableProcessInfoFailsClosed: a reply the adapter cannot judge is
// not an idle pane.
func TestAnUnreadableProcessInfoFailsClosed(t *testing.T) {
	for name, body := range map[string][]byte{
		"not json":     []byte("zsh"),
		"no shell pid": processInfoJSON(4242, 0, "zsh"),
		"no fg group":  processInfoJSON(0, 4242),
		"empty result": []byte(`{"result":{}}`),
	} {
		t.Run(name, func(t *testing.T) {
			run := newRunner().on(exec.KindHerdrPaneInspect, func() (exec.SensitiveResult, error) {
				return stdout(body), nil
			})
			a := newTestAdapter(t, run, nil)
			a.idleInterval = 0
			spec, _ := newSpec(t)

			res := a.Start(context.Background(), spec)

			if bootstrapSent(run) {
				t.Fatal("the bootstrap was sent after an unreadable inspection")
			}
			if causeClass(res) != backend.FailureMalformedResponse {
				t.Errorf("class = %v, want FailureMalformedResponse", causeClass(res))
			}
		})
	}
}

func hasArg(cmd exec.SensitiveCommand, want exec.Arg) bool {
	for _, arg := range cmd.Args {
		if arg.Equal(want) {
			return true
		}
	}
	return false
}

// TestATransientlyBusyRootPaneStillReceivesTheBootstrap: a prompt hook can
// hold the foreground for a moment as a job of its own. The check re-reads
// the pane, so that does not refuse a healthy launch.
func TestATransientlyBusyRootPaneStillReceivesTheBootstrap(t *testing.T) {
	reads := 0
	run := newRunner().on(exec.KindHerdrPaneInspect, func() (exec.SensitiveResult, error) {
		reads++
		if reads == 1 {
			return stdout(processInfoJSON(5000, 4242, "grep")), nil
		}
		return stdout(processInfoJSON(4242, 4242, "zsh")), nil
	})
	a := newTestAdapter(t, run, nil)
	a.idleInterval = 0
	spec, _ := newSpec(t)

	res := a.Start(context.Background(), spec)

	if res.Failed() {
		t.Fatalf("a pane busy for one read failed the launch: %v", causeClass(res))
	}
	if !bootstrapSent(run) {
		t.Fatal("the bootstrap was not sent once the pane went idle")
	}
	if reads != 2 {
		t.Errorf("inspected %d times, want 2", reads)
	}
}

// TestARootPaneRunAsAnAgentNeverReceivesTheBootstrap is the polish security
// finding: herdr's shell_pid is the pane's direct child, whatever it is. An
// agent started directly in the pane leads its own foreground group, so the
// group check alone reads it as idle; the shell-name check refuses it.
func TestARootPaneRunAsAnAgentNeverReceivesTheBootstrap(t *testing.T) {
	for name, body := range map[string][]byte{
		"agent is the pane's process": processInfoJSON(4242, 4242, "claude"),
		"leader not listed":           processInfoJSON(4242, 4242),
	} {
		t.Run(name, func(t *testing.T) {
			run := newRunner().on(exec.KindHerdrPaneInspect, func() (exec.SensitiveResult, error) {
				return stdout(body), nil
			})
			a := newTestAdapter(t, run, nil)
			a.idleInterval = 0
			spec, _ := newSpec(t)

			res := a.Start(context.Background(), spec)

			if bootstrapSent(run) {
				t.Fatal("the bootstrap was typed into a pane whose leader is not a shell")
			}
			if causeClass(res) != backend.FailureTargetBusy {
				t.Errorf("class = %v, want FailureTargetBusy", causeClass(res))
			}
		})
	}
}

func TestALoginShellLeaderIsIdle(t *testing.T) {
	info := processInfo{ForegroundGroup: 7, ShellPID: 7}
	info.Processes = append(info.Processes, paneProcess{Name: "-zsh", PID: 7, Argv: []string{"-zsh"}})
	if !info.idle() {
		t.Error("a login shell (-zsh) leading its own pane is not idle")
	}
}

// TestAPromptHookChildDelaysButDoesNotRefuse pins the shape measured on herdr
// 0.9.1: a fresh pane briefly lists prompt-hook children (env, direnv) inside
// the shell's own process group. The re-read waits them out.
func TestAPromptHookChildDelaysButDoesNotRefuse(t *testing.T) {
	reads := 0
	run := newRunner().on(exec.KindHerdrPaneInspect, func() (exec.SensitiveResult, error) {
		reads++
		if reads == 1 {
			return stdout(processInfoJSON(4242, 4242, "zsh", "direnv")), nil
		}
		return stdout(processInfoJSON(4242, 4242, "zsh")), nil
	})
	a := newTestAdapter(t, run, nil)
	a.idleInterval = 0
	spec, _ := newSpec(t)

	if res := a.Start(context.Background(), spec); res.Failed() {
		t.Fatalf("a hook child that cleared failed the launch: %v", causeClass(res))
	}
	if !bootstrapSent(run) {
		t.Fatal("the bootstrap was not sent once the hook finished")
	}
}

// TestAShellWrapperRunningAnAgentNeverReceivesTheBootstrap is the second-pass
// security finding: `bash -lc 'source env.sh; claude'` has no job control, so
// the agent is a child inside the shell's own group, led by a shell name. It
// never clears, so the launch is refused and the warning names the agent.
func TestAShellWrapperRunningAnAgentNeverReceivesTheBootstrap(t *testing.T) {
	run := newRunner().on(exec.KindHerdrPaneInspect, func() (exec.SensitiveResult, error) {
		return stdout(processInfoJSON(4242, 4242, "bash", "claude")), nil
	})
	var warnings bytes.Buffer
	a := newTestAdapter(t, run, nil, WithWarnings(&warnings))
	a.idleInterval = 0
	spec, _ := newSpec(t)

	res := a.Start(context.Background(), spec)

	if bootstrapSent(run) {
		t.Fatal("the bootstrap was typed into a shell wrapper running an agent")
	}
	if causeClass(res) != backend.FailureTargetBusy {
		t.Errorf("class = %v, want FailureTargetBusy", causeClass(res))
	}
	if !strings.Contains(warnings.String(), `"claude"`) {
		t.Errorf("warning %q does not name the agent behind the wrapper", warnings.String())
	}
}

// TestAShellWrapperStillSettingUpNeverReceivesTheBootstrap is the third-pass
// security finding: before `bash -lc 'source env.sh; claude'` starts its agent,
// the shell is alone in the foreground. It never reads the pane, so a typed
// line would wait for the agent. Its argv says so.
func TestAShellWrapperStillSettingUpNeverReceivesTheBootstrap(t *testing.T) {
	body := []byte(`{"result":{"process_info":{"foreground_process_group_id":4242,"shell_pid":4242,` +
		`"foreground_processes":[{"name":"bash","pid":4242,"argv":["bash","-lc","source env.sh; claude"]}]}}}`)
	run := newRunner().on(exec.KindHerdrPaneInspect, func() (exec.SensitiveResult, error) {
		return stdout(body), nil
	})
	a := newTestAdapter(t, run, nil)
	a.idleInterval = 0
	spec, _ := newSpec(t)

	res := a.Start(context.Background(), spec)

	if bootstrapSent(run) {
		t.Fatal("the bootstrap was typed into a shell running a command string")
	}
	if causeClass(res) != backend.FailureTargetBusy {
		t.Errorf("class = %v, want FailureTargetBusy", causeClass(res))
	}
}

func TestInteractiveArgv(t *testing.T) {
	for _, tc := range []struct {
		argv []string
		want bool
	}{
		{[]string{"-zsh"}, true},
		{[]string{"zsh"}, true},
		{[]string{"bash", "-l"}, true},
		{[]string{"zsh", "-i", "--login"}, true},
		{nil, false},
		{[]string{"bash", "-c", "claude"}, false},
		{[]string{"bash", "-lc", "claude"}, false},
		{[]string{"zsh", "-ic", "claude"}, false},
		{[]string{"fish", "--command", "claude"}, false},
		{[]string{"fish", "--command=claude"}, false},
		{[]string{"sh", "script.sh"}, false},
		{[]string{"sh", "-"}, false},
		{[]string{"sh", "--", "script.sh"}, false},
	} {
		if got := interactiveArgv(tc.argv); got != tc.want {
			t.Errorf("interactiveArgv(%q) = %v, want %v", tc.argv, got, tc.want)
		}
	}
}

// TestAShellThatStartsLateStillReceivesTheBootstrap: a pane whose shell has
// not started reports no shell pid; that is re-read, not refused.
func TestAShellThatStartsLateStillReceivesTheBootstrap(t *testing.T) {
	reads := 0
	run := newRunner().on(exec.KindHerdrPaneInspect, func() (exec.SensitiveResult, error) {
		reads++
		if reads == 1 {
			return stdout(processInfoJSON(0, 0)), nil
		}
		return stdout(processInfoJSON(4242, 4242, "zsh")), nil
	})
	a := newTestAdapter(t, run, nil)
	a.idleInterval = 0
	spec, _ := newSpec(t)

	if res := a.Start(context.Background(), spec); res.Failed() {
		t.Fatalf("a late shell failed the launch: %v", causeClass(res))
	}
	if !bootstrapSent(run) {
		t.Fatal("the bootstrap was not sent once the shell appeared")
	}
}
