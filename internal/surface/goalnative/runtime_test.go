//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package goalnative

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/net/websocket"
)

func TestMain(main *testing.M) {
	if os.Getenv("GOALNATIVE_HOST_HELPER") == "1" {
		var spec string
		for index, argument := range os.Args {
			if argument == "--spec" && index+1 < len(os.Args) {
				spec = os.Args[index+1]
			}
		}
		if err := RunHost(context.Background(), spec, os.Stdin, os.Stderr); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	if os.Getenv("GOALNATIVE_NATIVE_HELPER") == "1" {
		if err := runNativeHelper(os.Args); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	if os.Getenv("GOALNATIVE_ABRUPT_HELPER") == "1" {
		if err := runAbruptSupervisorHelper(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(main.Run())
}

func TestGoalNativeSessionLifecycle(t *testing.T) {
	root, err := os.MkdirTemp("/tmp", "gn-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	control := filepath.Join(root, "control")
	worktree := filepath.Join(root, "worktree")
	if err := os.Mkdir(control, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(worktree, 0o700); err != nil {
		t.Fatal(err)
	}
	native := writeHelper(t, "native", "GOALNATIVE_NATIVE_HELPER=1")
	supervisor := writeHelper(t, "supervisor", "GOALNATIVE_HOST_HELPER=1")
	socket := filepath.Join(control, "app.sock")
	var events int
	var eventPayloads []string
	session, err := Start(t.Context(), Config{Binary: native, SupervisorBinary: supervisor, ControlDir: control, Cwd: worktree, Socket: socket, Model: "gpt-test", WritableRoots: []string{worktree}, OnEvent: func(event json.RawMessage) { events++; eventPayloads = append(eventPayloads, string(event)) }})
	if err != nil {
		t.Fatal(err)
	}
	if session.ThreadID != "thread-test" || session.Model != "gpt-test" || session.Provider != "openai" {
		t.Fatalf("session identity = %#v", session)
	}
	for _, event := range append(eventPayloads, rawStrings(session.Events())...) {
		if strings.Contains(event, "config-secret-marker") {
			t.Fatalf("config/read response was persisted as an event: %s", event)
		}
	}
	specMatches, err := filepath.Glob(filepath.Join(control, "host-spec-*.json"))
	if err != nil || len(specMatches) != 1 {
		t.Fatalf("host specs = %v, %v", specMatches, err)
	}
	specBytes, err := os.ReadFile(specMatches[0])
	if err != nil || strings.Contains(string(specBytes), "environment") {
		t.Fatalf("host spec persisted environment data: %s (%v)", specBytes, err)
	}
	if _, err := session.Call(t.Context(), "triggerApproval", nil); err != nil {
		t.Fatalf("approval request: %v", err)
	}
	turn, err := session.Turn(t.Context(), "finish this bounded turn")
	if err != nil {
		t.Fatal(err)
	}
	if turn.ID != "turn-test" || turn.Status != "completed" || turn.Text != "native final" {
		t.Fatalf("turn = %#v", turn)
	}
	command, err := session.Exec(t.Context(), []string{"printf", "ok"}, worktree, []string{worktree}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if command.ExitCode != 3 || command.Stdout != "out" || command.Stderr != "err" {
		t.Fatalf("command = %#v", command)
	}
	timeoutProbe, err := session.Exec(t.Context(), []string{"timeout-probe"}, worktree, []string{worktree}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if timeoutProbe.Stdout != "1000" {
		t.Fatalf("native timeoutMs = %q", timeoutProbe.Stdout)
	}
	if _, err := session.Exec(t.Context(), []string{"incomplete"}, worktree, []string{worktree}, time.Second); err == nil {
		t.Fatal("incomplete native command result was accepted")
	}
	if err := session.WaitIdle(t.Context()); err != nil {
		t.Fatal(err)
	}
	if events == 0 || len(session.Events()) == 0 {
		t.Fatal("native events were not captured")
	}
	if err := session.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	receiptData, err := os.ReadFile(filepath.Join(control, "guardian-receipt.json")) //nolint:gosec // test receipt is created below the owner-private temp directory
	if err != nil {
		t.Fatal(err)
	}
	var receipt guardianReceipt
	if err := json.Unmarshal(receiptData, &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.Status != "stopped" || !receipt.GroupGone || !receipt.SocketGone {
		t.Fatalf("receipt = %#v", receipt)
	}
}

func TestGoalNativeRealCodexBoundaryIntegration(t *testing.T) {
	if os.Getenv("FORGECTL_GOALNATIVE_INTEGRATION") != "1" {
		t.Skip("set FORGECTL_GOALNATIVE_INTEGRATION=1 to run the real Codex integration")
	}
	native, err := exec.LookPath("codex")
	if err != nil {
		t.Skipf("codex is unavailable: %v", err)
	}
	root, err := os.MkdirTemp("/tmp", "gn-real-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	configHome := filepath.Join(root, "codex-home")
	control := filepath.Join(root, "control")
	worktree := filepath.Join(root, "worktree")
	for _, directory := range []string{configHome, control, worktree} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	config := "[mcp_servers.\"secret.server\"]\ncommand = \"/bin/echo\"\n[plugins.sample]\nenabled = true\n[apps.calendar]\nenabled = true\n"
	if err := os.WriteFile(filepath.Join(configHome, "config.toml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", configHome)
	supervisor := writeHelper(t, "supervisor", "GOALNATIVE_HOST_HELPER=1")
	socket := filepath.Join(control, "app.sock")
	model := os.Getenv("FORGECTL_GOALNATIVE_MODEL")
	if model == "" {
		model = "gpt-5"
	}
	session, err := Start(t.Context(), Config{Binary: native, SupervisorBinary: supervisor, ControlDir: control, Cwd: worktree, Socket: socket, Model: model, WritableRoots: []string{worktree}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = session.Close(context.Background()) }()
	allowed := filepath.Join(worktree, "allowed.txt")
	denied := filepath.Join(root, "denied.txt")
	result, err := session.Exec(t.Context(), []string{"/bin/sh", "-c", `set -C; printf allowed > "$1"; printf denied > "$2"`, "goal-boundary", allowed, denied}, worktree, []string{worktree}, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode == 0 {
		t.Fatalf("denied write unexpectedly succeeded: %#v", result)
	}
	if data, err := os.ReadFile(allowed); err != nil || string(data) != "allowed" { //nolint:gosec // test output is written below the owner-private temp directory
		t.Fatalf("allowed write = %q, %v", data, err)
	}
	if _, err := os.Lstat(denied); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("denied write created %s: %v", denied, err)
	}
	pidPath := filepath.Join(worktree, "long-command.pid")
	commandDone := make(chan error, 1)
	go func() {
		_, commandErr := session.Exec(context.Background(), []string{"/bin/sh", "-c", `printf '%s' "$$" > "$1"; sleep 60`, "goal-long", pidPath}, worktree, []string{worktree}, time.Minute)
		commandDone <- commandErr
	}()
	pid, err := waitForPIDFile(pidPath, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case commandErr := <-commandDone:
		if commandErr == nil {
			t.Fatal("owned command unexpectedly completed successfully")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("owned command did not observe session close")
	}
	if !waitForProcessGone(pid, 5*time.Second) {
		t.Fatalf("owned command process %d survived session close", pid)
	}
}

func TestGoalNativeRealCodexAbruptSupervisorDeath(t *testing.T) {
	testRealCodexAbruptSupervisorDeath(t, false)
}

func testRealCodexAbruptSupervisorDeath(t *testing.T, executionOnly bool) {
	t.Helper()
	if os.Getenv("FORGECTL_GOALNATIVE_INTEGRATION") != "1" {
		t.Skip("set FORGECTL_GOALNATIVE_INTEGRATION=1 to run the real Codex integration")
	}
	native, err := exec.LookPath("codex")
	if err != nil {
		t.Skipf("codex is unavailable: %v", err)
	}
	root, err := os.MkdirTemp("/tmp", "gn-abrupt-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	configHome := filepath.Join(root, "codex-home")
	control := filepath.Join(root, "control")
	worktree := filepath.Join(root, "worktree")
	for _, directory := range []string{configHome, control, worktree} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	config := "[mcp_servers.\"secret.server\"]\ncommand = \"/bin/echo\"\n[plugins.sample]\nenabled = true\n[apps.calendar]\nenabled = true\n"
	if err := os.WriteFile(filepath.Join(configHome, "config.toml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(control, "abrupt-state.json")
	pidPath := filepath.Join(worktree, "long-command.pids")
	model := os.Getenv("FORGECTL_GOALNATIVE_MODEL")
	if model == "" {
		model = "gpt-5"
	}
	binary, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	mode := "model"
	if executionOnly {
		mode = "execution"
	}
	helper := exec.CommandContext(t.Context(), binary, "-test.run=TestGoalNativeAbruptSupervisorHelper", "--") //nolint:gosec // test launches only its own resolved executable
	helper.Env = append(filteredHelperEnvironment(os.Environ()),
		"GOALNATIVE_ABRUPT_HELPER=1",
		"GOALNATIVE_ABRUPT_CONFIG_HOME="+configHome,
		"GOALNATIVE_ABRUPT_CONTROL="+control,
		"GOALNATIVE_ABRUPT_NATIVE="+native,
		"GOALNATIVE_ABRUPT_MODEL="+model,
		"GOALNATIVE_ABRUPT_PID_PATH="+pidPath,
		"GOALNATIVE_ABRUPT_STATE="+statePath,
		"GOALNATIVE_ABRUPT_WORKTREE="+worktree,
		"GOALNATIVE_ABRUPT_MODE="+mode,
		"GOALNATIVE_ABRUPT_SUPERVISOR="+writeHelper(t, "supervisor", "GOALNATIVE_HOST_HELPER=1"),
	)
	helper.Stdout = os.Stderr
	helper.Stderr = os.Stderr
	if err := helper.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if helper.Process != nil {
			_ = helper.Process.Kill()
		}
		_ = helper.Wait()
	}()
	state, err := waitForAbruptSupervisorState(statePath, 15*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !state.Ready || state.SupervisorPID != helper.Process.Pid || state.NativePID <= 1 || state.ProcessGroup != state.NativePID || state.ShellPID <= 1 || state.ChildPID <= 1 || state.ShellPID == state.ChildPID {
		t.Fatalf("abrupt helper state = %#v", state)
	}
	if !processAlive(state.NativePID) || !processAlive(state.ShellPID) || !processAlive(state.ChildPID) {
		t.Fatalf("abrupt helper did not publish live owned processes: %#v", state)
	}
	if err := helper.Process.Kill(); err != nil {
		t.Fatalf("kill abrupt helper: %v", err)
	}
	if err := helper.Wait(); err == nil {
		t.Fatal("abrupt helper exited successfully after SIGKILL")
	}
	receipt, err := waitForGuardianReceipt(filepath.Join(control, "guardian-receipt.json"), 15*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Status != "stopped" || receipt.NativePID != state.NativePID || receipt.ProcessGroup != state.ProcessGroup || receipt.Socket != filepath.Join(control, "app.sock") || !receipt.SocketGone || !receipt.GroupGone || receipt.Error != "" {
		t.Fatalf("abrupt cleanup receipt = %#v", receipt)
	}
	for _, pid := range []int{state.NativePID, state.ShellPID, state.ChildPID} {
		if !waitForProcessGone(pid, 5*time.Second) {
			t.Fatalf("owned process %d survived abrupt supervisor death", pid)
		}
	}
}

type abruptSupervisorState struct {
	Ready         bool   `json:"ready"`
	Error         string `json:"error,omitempty"`
	SupervisorPID int    `json:"supervisor_pid"`
	NativePID     int    `json:"native_pid"`
	ProcessGroup  int    `json:"process_group"`
	ShellPID      int    `json:"shell_pid"`
	ChildPID      int    `json:"child_pid"`
}

type abruptCommandOutcome struct {
	Result CommandResult
	Err    error
}

func runAbruptSupervisorHelper() error {
	control, err := abruptHelperEnv("GOALNATIVE_ABRUPT_CONTROL")
	if err != nil {
		return err
	}
	worktree, err := abruptHelperEnv("GOALNATIVE_ABRUPT_WORKTREE")
	if err != nil {
		return err
	}
	configHome, err := abruptHelperEnv("GOALNATIVE_ABRUPT_CONFIG_HOME")
	if err != nil {
		return err
	}
	native, err := abruptHelperEnv("GOALNATIVE_ABRUPT_NATIVE")
	if err != nil {
		return err
	}
	model := os.Getenv("GOALNATIVE_ABRUPT_MODEL")
	if model == "" || !safeToken(model) {
		return errors.New("GOALNATIVE_ABRUPT_MODEL is missing or invalid")
	}
	pidPath, err := abruptHelperEnv("GOALNATIVE_ABRUPT_PID_PATH")
	if err != nil {
		return err
	}
	statePath, err := abruptHelperEnv("GOALNATIVE_ABRUPT_STATE")
	if err != nil {
		return err
	}
	environment := append(filteredHelperEnvironment(os.Environ()), "CODEX_HOME="+configHome, "GOALNATIVE_HOST_HELPER=1")
	supervisor, err := abruptHelperEnv("GOALNATIVE_ABRUPT_SUPERVISOR")
	if err != nil {
		return err
	}
	executionOnly := os.Getenv("GOALNATIVE_ABRUPT_MODE") == "execution"
	if executionOnly {
		model = ""
	}
	session, err := Start(context.Background(), Config{Binary: native, SupervisorBinary: supervisor, ControlDir: control, Cwd: worktree, Socket: filepath.Join(control, "app.sock"), Model: model, Environment: environment, WritableRoots: []string{worktree}, ExecutionOnly: executionOnly})
	if err != nil {
		_ = writePrivateJSON(statePath, abruptSupervisorState{Error: err.Error(), SupervisorPID: os.Getpid()})
		return err
	}
	defer func() { _ = session.Close(context.Background()) }()
	if executionOnly && (session.ThreadID != "" || session.Model != "" || session.Provider != "") {
		return writeAbruptHelperError(statePath, errors.New("execution host created a model session"))
	}
	started, err := readGuardianReceipt(startupReceiptPath(control))
	if err != nil {
		return writeAbruptHelperError(statePath, err)
	}
	commandDone := make(chan abruptCommandOutcome, 1)
	go func() {
		result, commandErr := session.Exec(context.Background(), []string{"/bin/sh", "-c", `printf '%s\n' "$$" > "$1"; sleep 60 & child=$!; printf '%s\n' "$child" >> "$1"; wait "$child"`, "goal-abrupt-long", pidPath}, worktree, []string{worktree}, time.Minute)
		commandDone <- abruptCommandOutcome{Result: result, Err: commandErr}
	}()
	shellPID, childPID, outcome, err := waitForPIDPairOrCommand(pidPath, commandDone, 10*time.Second)
	if err != nil {
		if outcome != nil {
			return writeAbruptHelperError(statePath, fmt.Errorf("%w: result=%#v error=%v", err, outcome.Result, outcome.Err))
		}
		return writeAbruptHelperError(statePath, err)
	}
	state := abruptSupervisorState{Ready: true, SupervisorPID: os.Getpid(), NativePID: started.NativePID, ProcessGroup: started.ProcessGroup, ShellPID: shellPID, ChildPID: childPID}
	if err := writePrivateJSON(statePath, state); err != nil {
		return err
	}
	if outcome := <-commandDone; outcome.Err != nil {
		return writeAbruptHelperError(statePath, fmt.Errorf("owned command failed after readiness: result=%#v error=%v", outcome.Result, outcome.Err))
	}
	return writeAbruptHelperError(statePath, errors.New("owned long command completed before supervisor death"))
}

func abruptHelperEnv(name string) (string, error) {
	value := os.Getenv(name)
	if value == "" || !filepath.IsAbs(value) || filepath.Clean(value) != value {
		return "", fmt.Errorf("%s is missing or not an absolute clean path", name)
	}
	return value, nil
}

func filteredHelperEnvironment(environment []string) []string {
	filtered := make([]string, 0, len(environment))
	for _, entry := range environment {
		if strings.HasPrefix(entry, "CODEX_HOME=") || strings.HasPrefix(entry, "GOALNATIVE_ABRUPT_") || strings.HasPrefix(entry, "GOALNATIVE_HOST_HELPER=") || strings.HasPrefix(entry, "GOALNATIVE_NATIVE_HELPER=") {
			continue
		}
		filtered = append(filtered, entry)
	}
	return filtered
}

func writeAbruptHelperError(path string, err error) error {
	stateErr := writePrivateJSON(path, abruptSupervisorState{Error: err.Error(), SupervisorPID: os.Getpid()})
	return errors.Join(err, stateErr)
}

func waitForAbruptSupervisorState(path string, timeout time.Duration) (abruptSupervisorState, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path) //nolint:gosec // path is a test-generated fixture below the owner-private temp directory
		if err == nil {
			var state abruptSupervisorState
			if json.Unmarshal(data, &state) == nil {
				if state.Error != "" {
					return abruptSupervisorState{}, errors.New(state.Error)
				}
				if state.Ready {
					return state, nil
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	return abruptSupervisorState{}, fmt.Errorf("abrupt supervisor did not publish ready state in %s", path)
}

func waitForGuardianReceipt(path string, timeout time.Duration) (guardianReceipt, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		receipt, err := readGuardianReceipt(path)
		if err == nil && receipt.Status == "stopped" {
			return receipt, nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return guardianReceipt{}, fmt.Errorf("guardian receipt did not confirm stopped cleanup in %s", path)
}

func waitForPIDPairOrCommand(path string, commandDone <-chan abruptCommandOutcome, timeout time.Duration) (int, int, *abruptCommandOutcome, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case outcome := <-commandDone:
			return 0, 0, &outcome, errors.New("owned command completed before PID publication")
		default:
		}
		data, err := os.ReadFile(path) //nolint:gosec // path is a test-generated PID file below the owner-private temp directory
		if err == nil {
			var shellPID, childPID int
			if _, scanErr := fmt.Sscan(string(data), &shellPID, &childPID); scanErr == nil && shellPID > 1 && childPID > 1 {
				return shellPID, childPID, nil, nil
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	return 0, 0, nil, fmt.Errorf("owned command did not publish shell and child PIDs in %s", path)
}

func TestPIDPairReadinessAcceptsNewlinesAndRejectsEarlyExit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "command.pids")
	if err := os.WriteFile(path, []byte("12345\n23456\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	commandDone := make(chan abruptCommandOutcome, 1)
	shellPID, childPID, outcome, err := waitForPIDPairOrCommand(path, commandDone, time.Second)
	if err != nil || outcome != nil || shellPID != 12345 || childPID != 23456 {
		t.Fatalf("PID pair readiness = %d %d %#v %v", shellPID, childPID, outcome, err)
	}
	commandDone <- abruptCommandOutcome{Result: CommandResult{ExitCode: 1, Stderr: "fixture failure"}}
	_, _, outcome, err = waitForPIDPairOrCommand(path, commandDone, time.Second)
	if err == nil || outcome == nil || outcome.Result.Stderr != "fixture failure" {
		t.Fatalf("early command failure was hidden: %#v %v", outcome, err)
	}
}

func processAlive(pid int) bool {
	return pid > 1 && syscall.Kill(pid, 0) == nil
}

func waitForPIDFile(path string, timeout time.Duration) (int, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path) //nolint:gosec // path is a test-generated PID file below the owner-private temp directory
		if err == nil {
			var pid int
			if _, scanErr := fmt.Sscanf(string(data), "%d", &pid); scanErr == nil && pid > 1 {
				return pid, nil
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	return 0, fmt.Errorf("owned command did not publish a PID in %s", path)
}

func waitForProcessGone(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		err := syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
}

func TestGoalNativeValidationRefusesControlRootOverlap(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "work"), 0o700); err != nil {
		t.Fatal(err)
	}
	config := Config{Binary: filepath.Join(root, "codex"), SupervisorBinary: filepath.Join(root, "forgectl"), ControlDir: root, Cwd: filepath.Join(root, "work"), Socket: filepath.Join(root, "socket"), Model: "model", WritableRoots: []string{root}}
	if err := validateConfig(config); err == nil {
		t.Fatal("control root overlap was accepted")
	}
}

func TestStartupFailureReportsUnconfirmedCleanup(t *testing.T) {
	control := t.TempDir()
	childDone := make(chan struct{})
	close(childDone)
	session := &Session{childDone: childDone, done: make(chan struct{}), config: Config{ControlDir: control, Socket: filepath.Join(control, "app.sock")}}
	cause := errors.New("startup failed")

	err := startupFailure(session, cause)
	if !errors.Is(err, cause) {
		t.Fatalf("startup error = %v, want cause", err)
	}
	if !errors.Is(err, ErrCleanupUnconfirmed) {
		t.Fatalf("startup error = %v, want ErrCleanupUnconfirmed", err)
	}
}

func TestSessionWaitIdleTracksExactNativeTurns(t *testing.T) {
	idle := make(chan struct{})
	close(idle)
	session := &Session{ThreadID: "thread-test", turnIdle: idle, nativeBusy: make(map[string]struct{})}
	session.observeTurn(rpcEnvelope{Method: "turn/started", Params: mustJSON(map[string]any{"threadId": "other-thread", "turn": map[string]any{"id": "turn-other", "status": "inProgress"}})})
	if err := session.WaitIdle(context.Background()); err != nil {
		t.Fatalf("unrelated native turn blocked idle: %v", err)
	}
	session.observeTurn(rpcEnvelope{Method: "turn/started", Params: mustJSON(map[string]any{"threadId": "thread-test", "turn": map[string]any{"id": "turn-external", "status": "inProgress"}})})
	if _, err := session.Turn(context.Background(), "must not overlap"); err == nil {
		t.Fatal("local turn started while an external native turn was active")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := session.WaitIdle(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("active native turn idle result = %v", err)
	}
	session.observeTurn(rpcEnvelope{Method: "turn/completed", Params: mustJSON(map[string]any{"threadId": "thread-test", "turn": map[string]any{"id": "turn-external", "status": "completed"}})})
	if err := session.WaitIdle(context.Background()); err != nil {
		t.Fatalf("completed native turn did not become idle: %v", err)
	}
	session.observeTurn(rpcEnvelope{Method: "turn/started", Params: mustJSON(map[string]any{"threadId": "thread-test", "turn": map[string]any{"id": "turn-failed", "status": "inProgress"}})})
	session.observeTurn(rpcEnvelope{Method: "turn/completed", Params: mustJSON(map[string]any{"threadId": "thread-test", "turn": map[string]any{"id": "turn-failed", "status": "failed"}})})
	if err := session.WaitIdle(context.Background()); err == nil || !strings.Contains(err.Error(), "failed") {
		t.Fatalf("failed native turn idle result = %v", err)
	}
}

func TestSessionFinalTextPrefersFinalAnswerBeyondEventWindow(t *testing.T) {
	idle := make(chan struct{})
	close(idle)
	session := &Session{ThreadID: "thread-test", turnIdle: idle, nativeBusy: make(map[string]struct{}), turnText: make(map[string]string), turnFinalText: make(map[string]string)}
	session.recordEvent([]byte(`{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"thread-test","turnId":"turn-test","item":{"type":"agentMessage","phase":"commentary","text":"intermediate"}}}`))
	session.recordEvent([]byte(`{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"thread-test","turnId":"turn-test","item":{"type":"agentMessage","phase":"final_answer","text":"final"}}}`))
	for index := 0; index < maxEvents+8; index++ {
		session.recordEvent([]byte(`{"jsonrpc":"2.0","method":"item/started","params":{}}`))
	}
	if text := session.finalText("turn-test"); text != "final" {
		t.Fatalf("final text = %q", text)
	}
}

func TestGuardianReceiptRejectsError(t *testing.T) {
	control := t.TempDir()
	socket := filepath.Join(control, "app.sock")
	startedAt := time.Now().UTC()
	startup := guardianReceipt{Status: "started", NativePID: 10001, ProcessGroup: 10001, Socket: socket, StartedAt: startedAt}
	finished := guardianReceipt{Status: "stopped", NativePID: 10001, ProcessGroup: 10001, Socket: socket, StartedAt: startedAt, SocketGone: true, GroupGone: true, Error: "native process group remains"}
	if err := writeGuardianReceipt(startupReceiptPath(control), startup); err != nil {
		t.Fatal(err)
	}
	if err := writeGuardianReceipt(filepath.Join(control, "guardian-receipt.json"), finished); err != nil {
		t.Fatal(err)
	}
	if err := validateGuardianReceipts(control, socket); err == nil {
		t.Fatal("receipt error was accepted")
	}
}

func TestOpenAIRoutingRejectsOverride(t *testing.T) {
	if err := assertOpenAIRouting(json.RawMessage(`{"config":{"model_providers":{"openai":{"base_url":"https://example.invalid"}}}}`)); err == nil {
		t.Fatal("non-OpenAI provider override was accepted")
	}
	if err := assertOpenAIRouting(json.RawMessage(`{"config":{"model_providers":{"openai":{"base_url":"https://api.openai.com/v1"}}}}`)); err != nil {
		t.Fatalf("direct OpenAI endpoint was rejected: %v", err)
	}
	t.Setenv("OPENAI_BASE_URL", "https://example.invalid")
	if err := validateOpenAIEnvironment(nil); err == nil {
		t.Fatal("non-OpenAI environment override was accepted")
	}
}

func TestNativeIsolationAcceptsDisabledConfiguration(t *testing.T) {
	raw := json.RawMessage(`{"config":{"features":{"apps":false,"hooks":false,"memories":false,"multi_agent":false,"plugins":false},"agents":{"enabled":false},"web_search":"disabled","notify":[],"apps":{"_default":{"enabled":false},"calendar":{"enabled":false}},"mcp_servers":{"local":{"enabled":false}},"plugins":{"sample.plugin":{"enabled":false}}}}`)
	if err := assertNativeIsolation(raw); err != nil {
		t.Fatalf("assertNativeIsolation = %v", err)
	}
}

func TestNativeIsolationRejectsEnabledConfiguration(t *testing.T) {
	base := map[string]any{
		"config": map[string]any{
			"features": map[string]any{"apps": false, "hooks": false, "memories": false, "multi_agent": false, "plugins": false},
			"agents":   map[string]any{"enabled": false}, "web_search": "disabled", "notify": []any{},
			"apps": map[string]any{"_default": map[string]any{"enabled": false}},
		},
	}
	for _, test := range []struct {
		name string
		edit func(map[string]any)
	}{
		{name: "feature", edit: func(config map[string]any) { config["features"].(map[string]any)["plugins"] = true }},
		{name: "agent", edit: func(config map[string]any) { config["agents"].(map[string]any)["enabled"] = true }},
		{name: "web search", edit: func(config map[string]any) { config["web_search"] = "enabled" }},
		{name: "notifications", edit: func(config map[string]any) { config["notify"] = []any{"desktop"} }},
		{name: "app", edit: func(config map[string]any) {
			config["apps"].(map[string]any)["calendar"] = map[string]any{"enabled": true}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := base["config"].(map[string]any)
			copyConfig := make(map[string]any, len(config))
			for key, value := range config {
				copyConfig[key] = value
			}
			test.edit(copyConfig)
			raw, err := json.Marshal(map[string]any{"config": copyConfig})
			if err != nil {
				t.Fatal(err)
			}
			if err := assertNativeIsolation(raw); err == nil {
				t.Fatal("enabled native capability was accepted")
			}
		})
	}
}

func TestNativeServerArgumentsDisableConfiguredEntries(t *testing.T) {
	root := t.TempDir()
	configHome := filepath.Join(root, "codex")
	if err := os.Mkdir(configHome, 0o700); err != nil {
		t.Fatal(err)
	}
	config := "[mcp_servers.\"local.server\"]\ncommand = \"secret\"\n[plugins.sample]\nenabled = true\n[apps.calendar]\nenabled = true\n"
	if err := os.WriteFile(filepath.Join(configHome, "config.toml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	spec := hostSpec{Cwd: filepath.Join(root, "work"), Socket: filepath.Join(root, "socket")}
	arguments, err := nativeServerArguments(spec, []string{"CODEX_HOME=" + configHome})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(arguments, " ")
	for _, want := range []string{"--disable apps", "--disable hooks", "--disable memories", "--disable multi_agent", "--disable plugins", `mcp_servers={"local.server"={enabled=false}}`, `plugins.sample.enabled=false`, `apps.calendar.enabled=false`} {
		if !strings.Contains(joined, want) {
			t.Errorf("arguments %q missing %q", joined, want)
		}
	}
	if strings.Contains(joined, "secret") {
		t.Fatal("native arguments leaked a config value")
	}
}

func writeHelper(t *testing.T, name, environment string) string {
	t.Helper()
	binary, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), name)
	script := "#!/bin/sh\nexec " + shellQuote(binary) + " -test.run=TestGoalNativeHelper -- \"$@\"\n"
	if environment != "" {
		script = "#!/bin/sh\nenv -u GOALNATIVE_HOST_HELPER " + environment + " " + shellQuote(binary) + " -test.run=TestGoalNativeHelper -- \"$@\"\n"
	}
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil { //nolint:gosec // test helper must be executable
		t.Fatal(err)
	}
	return path
}

func shellQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'" }

func rawStrings(values []json.RawMessage) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		result = append(result, string(value))
	}
	return result
}

func runNativeHelper(arguments []string) error {
	var socket string
	for index, argument := range arguments {
		if argument == "--listen" && index+1 < len(arguments) {
			socket = strings.TrimPrefix(arguments[index+1], "unix://")
		}
	}
	if socket == "" {
		return errors.New("native helper socket missing")
	}
	listener, err := net.Listen("unix", socket) //nolint:gosec,noctx // test-only local Unix socket
	if err != nil {
		return err
	}
	defer func() { _ = listener.Close() }()
	grandchild := exec.Command("/bin/sleep", "60") //nolint:gosec,noctx // fixed test helper command
	if err := grandchild.Start(); err != nil {
		return err
	}
	defer func() { _ = grandchild.Process.Kill() }()
	server := &http.Server{Handler: websocket.Server{Handler: websocket.Handler(nativeConnection)}, ReadHeaderTimeout: time.Second}
	return server.Serve(listener)
}

func nativeConnection(conn *websocket.Conn) {
	defer func() { _ = conn.Close() }()
	var cwd string
	for {
		var request map[string]json.RawMessage
		if err := websocket.JSON.Receive(conn, &request); err != nil {
			return
		}
		var method string
		_ = json.Unmarshal(request["method"], &method)
		var requestParams map[string]json.RawMessage
		_ = json.Unmarshal(request["params"], &requestParams)
		_ = json.Unmarshal(requestParams["cwd"], &cwd)
		var command []string
		_ = json.Unmarshal(requestParams["command"], &command)
		var timeoutMS int64
		_ = json.Unmarshal(requestParams["timeoutMs"], &timeoutMS)
		id := request["id"]
		if method == "initialized" {
			continue
		}
		if method == "triggerApproval" {
			_ = websocket.JSON.Send(conn, map[string]any{"jsonrpc": protocolVersion, "id": 900, "method": "server/request", "params": map[string]any{}})
			var answer map[string]json.RawMessage
			if websocket.JSON.Receive(conn, &answer) != nil {
				return
			}
			var result any = map[string]any{"declined": true}
			_ = websocket.JSON.Send(conn, map[string]any{"jsonrpc": protocolVersion, "id": id, "result": result})
			continue
		}
		if len(id) == 0 {
			continue
		}
		var result any
		switch method {
		case "initialize":
			result = map[string]any{}
		case "config/read":
			result = map[string]any{"config": map[string]any{ //nolint:gosec // G101: the response includes a fake marker for the persistence test
				"projects":             map[string]any{cwd: map[string]any{"trust_level": "untrusted"}},
				"config_secret_marker": "config-secret-marker",
				"features":             map[string]any{"apps": false, "hooks": false, "memories": false, "multi_agent": false, "plugins": false},
				"agents":               map[string]any{"enabled": false},
				"web_search":           "disabled",
				"notify":               []any{},
				"apps":                 map[string]any{"_default": map[string]any{"enabled": false}},
			}, "layers": []any{map[string]any{"name": map[string]any{"type": "sessionFlags"}, "config": map[string]any{"projects": map[string]any{cwd: map[string]any{"trust_level": "untrusted"}}}}}}
		case "thread/start":
			result = map[string]any{"thread": map[string]any{"id": "thread-test", "model": "gpt-test", "modelProvider": "openai", "cwd": cwd, "ephemeral": false}, "cwd": cwd, "model": "gpt-test", "modelProvider": "openai", "approvalPolicy": "never", "approvalsReviewer": "user", "sandbox": map[string]any{"type": "readOnly"}}
		case "command/exec":
			result = map[string]any{"exitCode": 3, "stdout": "out", "stderr": "err"}
			if len(command) > 0 && command[0] == "timeout-probe" {
				result = map[string]any{"exitCode": 0, "stdout": fmt.Sprintf("%d", timeoutMS), "stderr": ""}
			}
			if len(command) > 0 && command[0] == "incomplete" {
				result = map[string]any{}
			}
		case "turn/start":
			result = map[string]any{"turn": map[string]any{"id": "turn-test"}}
			_ = websocket.JSON.Send(conn, map[string]any{"jsonrpc": protocolVersion, "id": id, "result": result})
			_ = websocket.JSON.Send(conn, map[string]any{"jsonrpc": protocolVersion, "method": "item/completed", "params": map[string]any{"threadId": "thread-test", "turnId": "turn-test", "item": map[string]any{"type": "agentMessage", "text": "native final"}}})
			_ = websocket.JSON.Send(conn, map[string]any{"jsonrpc": protocolVersion, "method": "turn/completed", "params": map[string]any{"threadId": "thread-test", "turn": map[string]any{"id": "turn-test", "status": "completed"}}})
			continue
		default:
			result = map[string]any{}
		}
		_ = websocket.JSON.Send(conn, map[string]any{"jsonrpc": protocolVersion, "id": json.RawMessage(id), "result": result})
	}
}
