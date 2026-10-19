//go:build unix

package goalnative

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestExecutionEnvironmentRemovesCredentialsAndAmbientConfiguration(t *testing.T) {
	environment := CredentialFreeEnvironment([]string{"HOME=/ambient", "CODEX_HOME=/ambient", "ANTHROPIC_API_KEY=sentinel", "OPENAI_API_KEY=sentinel", "OPENAI_BASE_URL=https://example.invalid", "NODE_OPTIONS=--require=ambient", "BASH_ENV=/ambient", "PATH=/bin", "TMPDIR=/allowed/tmp"}, "/private/empty")
	joined := strings.Join(environment, "\n")
	for _, denied := range []string{"sentinel", "ambient", "example.invalid", "NODE_OPTIONS", "BASH_ENV"} {
		if strings.Contains(joined, denied) {
			t.Fatalf("execution environment retained %s", denied)
		}
	}
	if !strings.Contains(joined, "CODEX_HOME=/private/empty") || !strings.Contains(joined, "TMPDIR=/allowed/tmp") {
		t.Fatal("execution environment omitted its explicit private paths")
	}
}

func TestExecutionHostNeverCreatesModelSession(t *testing.T) {
	root, err := os.MkdirTemp("/tmp", "gn-exec-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	work := filepath.Join(root, "work")
	control := filepath.Join(root, "control")
	if err := os.Mkdir(work, 0o700); err != nil {
		t.Fatal(err)
	}
	host, err := StartCommandHost(t.Context(), Config{Binary: writeHelper(t, "native", "GOALNATIVE_NATIVE_HELPER=1"), SupervisorBinary: writeHelper(t, "supervisor", "GOALNATIVE_HOST_HELPER=1"), ControlDir: control, Cwd: work, Socket: filepath.Join(root, "exec.sock"), WritableRoots: []string{work}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = host.Close(context.Background()) })
	if host.session.ThreadID != "" || host.session.Model != "" || host.session.Provider != "" {
		t.Fatal("execution-only host created a native model session")
	}
	for _, method := range []string{"thread/start", "turn/start", "account/login/start", "config/write", "arbitrary"} {
		if _, err := host.session.Call(t.Context(), method, nil); err == nil || !strings.Contains(err.Error(), "execution-only") {
			t.Fatalf("execution-only RPC %s was not refused before transport: %v", method, err)
		}
	}
	result, err := host.Exec(t.Context(), []string{"printf", "ok"}, work, []string{work}, time.Second)
	if err != nil || result.Stdout != "out" {
		t.Fatalf("approved command did not use the live transport: %#v, %v", result, err)
	}
	if _, err := host.Exec(t.Context(), []string{"printf", "bad"}, work, []string{root}, time.Second); err == nil {
		t.Fatal("command widened the host's writable roots")
	}
	if err := host.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestJSONProcessUsesOwnedGuardianAndBoundedFrames(t *testing.T) {
	root, err := os.MkdirTemp("/tmp", "gn-json-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	work := filepath.Join(root, "work")
	if err := os.Mkdir(work, 0o700); err != nil {
		t.Fatal(err)
	}
	events := make(chan json.RawMessage, 1)
	process, err := StartJSONProcess(t.Context(), ProcessConfig{Binary: "/bin/cat", SupervisorBinary: writeHelper(t, "supervisor", "GOALNATIVE_HOST_HELPER=1"), ControlDir: filepath.Join(root, "control"), Cwd: work, Socket: filepath.Join(root, "native.sock"), OnMessage: func(raw json.RawMessage) { events <- raw }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = process.Close(context.Background()) })
	if err := process.Write(t.Context(), map[string]string{"probe": "owned transport"}); err != nil {
		t.Fatal(err)
	}
	select {
	case raw := <-events:
		if string(raw) != `{"probe":"owned transport"}` {
			t.Fatalf("unexpected native frame: %s", raw)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("native JSON transport did not return the exact frame")
	}
	if err := process.Write(t.Context(), strings.Repeat("x", maxPayloadBytes)); err == nil {
		t.Fatal("oversized native frame was accepted")
	}
	if err := process.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-process.Done():
	case <-time.After(time.Second):
		t.Fatal("native reader survived close")
	}
}

func TestExecutionHostRealBoundaryAndNoModel(t *testing.T) {
	if os.Getenv("FORGECTL_GOALNATIVE_INTEGRATION") != "1" {
		t.Skip("set FORGECTL_GOALNATIVE_INTEGRATION=1 for the real no-model execution boundary")
	}
	native, err := exec.LookPath("codex")
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp("/tmp", "gn-boundary-")
	if err != nil {
		t.Fatal(err)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	work := filepath.Join(root, "work")
	if err := os.Mkdir(work, 0o700); err != nil {
		t.Fatal(err)
	}
	host, err := StartCommandHost(t.Context(), Config{Binary: native, SupervisorBinary: writeHelper(t, "supervisor", "GOALNATIVE_HOST_HELPER=1"), ControlDir: filepath.Join(root, "control"), Cwd: work, Socket: filepath.Join(root, "exec.sock"), WritableRoots: []string{work}, Environment: []string{"PATH=" + os.Getenv("PATH"), "OPENAI_API_KEY=not-a-real-credential", "ANTHROPIC_API_KEY=not-a-real-credential"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = host.Close(context.Background()) })
	result, err := host.Exec(t.Context(), []string{"/usr/bin/env"}, work, []string{work}, 10*time.Second)
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("real allowed command: %#v, %v", result, err)
	}
	if strings.Contains(result.Stdout, "not-a-real-credential") || host.session.ThreadID != "" {
		t.Fatal("execution host exposed credentials or started a model thread")
	}
	allowed, denied := filepath.Join(work, "allowed.txt"), filepath.Join(root, "denied.txt")
	result, err = host.Exec(t.Context(), []string{"/bin/sh", "-c", `printf allowed > "$1"; printf forbidden > "$2"`, "probe", allowed, denied}, work, []string{work}, 10*time.Second)
	if err != nil || result.ExitCode == 0 || !strings.Contains(result.Stderr, "Operation not permitted") {
		t.Fatalf("real denied write: %#v, %v", result, err)
	}
	data, err := os.ReadFile(allowed)
	if err != nil || string(data) != "allowed" {
		t.Fatalf("allowed write did not work: %q, %v", data, err)
	}
	if _, err := os.Stat(denied); !os.IsNotExist(err) {
		t.Fatalf("denied target was created: %v", err)
	}
	probeSocket := filepath.Join(root, "private.sock")
	listener, err := net.Listen("unix", probeSocket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	control, err := net.Dial("unix", probeSocket)
	if err != nil {
		t.Fatalf("Unix socket positive control failed: %v", err)
	}
	_ = control.Close()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal(err)
	}
	result, err = host.Exec(t.Context(), []string{python, "-c", "import socket,sys; channel=socket.socket(socket.AF_UNIX); channel.connect(sys.argv[1])", probeSocket}, work, []string{work}, 10*time.Second)
	if err != nil || result.ExitCode == 0 || !strings.Contains(result.Stderr, "Operation not permitted") {
		t.Fatalf("sandboxed tools reached the private role socket: %#v, %v", result, err)
	}
	if err := host.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(root, "control", "execution-home"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() == "sessions" || entry.Name() == "auth.json" {
			t.Fatalf("execution-only host created %s", entry.Name())
		}
	}
}

func TestExecutionHostRealEscapedChildCancellation(t *testing.T) {
	if os.Getenv("FORGECTL_GOALNATIVE_INTEGRATION") != "1" {
		t.Skip("set FORGECTL_GOALNATIVE_INTEGRATION=1 for the escaped-child negative control")
	}
	native, err := exec.LookPath("codex")
	if err != nil {
		t.Fatal(err)
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal(err)
	}
	control := exec.CommandContext(t.Context(), python, "-c", "import os; os.setsid(); print('session-created')")
	if output, err := control.CombinedOutput(); err != nil || strings.TrimSpace(string(output)) != "session-created" {
		t.Fatalf("unconfined setsid control failed: %s, %v", output, err)
	}
	root, err := os.MkdirTemp("/tmp", "gn-escape-")
	if err != nil {
		t.Fatal(err)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	work := filepath.Join(root, "work")
	if err := os.Mkdir(work, 0o700); err != nil {
		t.Fatal(err)
	}
	host, err := StartCommandHost(t.Context(), Config{Binary: native, SupervisorBinary: writeHelper(t, "supervisor", "GOALNATIVE_HOST_HELPER=1"), ControlDir: filepath.Join(root, "control"), Cwd: work, Socket: filepath.Join(root, "exec.sock"), WritableRoots: []string{work}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = host.Close(context.Background()) })
	pidPath := filepath.Join(work, "escaped.pid")
	commandCtx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := host.Exec(commandCtx, []string{python, "-c", "import os,sys,time\nchild=os.fork()\nif child==0:\n os.setsid()\n with open(sys.argv[1],'w') as output: output.write(str(os.getpid()))\n time.sleep(8)\n os._exit(0)\ntime.sleep(30)", pidPath}, work, []string{work}, 35*time.Second)
		done <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	childPID := 0
	for childPID == 0 && time.Now().Before(deadline) {
		data, readErr := os.ReadFile(pidPath)
		if readErr == nil {
			childPID, _ = strconv.Atoi(string(data))
		}
		time.Sleep(20 * time.Millisecond)
	}
	if childPID <= 1 {
		t.Fatal("escaped-child negative control did not establish its live child")
	}
	if err := syscall.Kill(childPID, 0); err != nil {
		t.Fatalf("negative-control child was not alive before cancellation: %v", err)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("canceled command did not report cancellation: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("command cancellation did not settle")
	}
	closeErr := host.Close(t.Context())
	gone := waitForProcessGone(childPID, time.Second)
	if !gone {
		t.Errorf("escaped native-tool descendant survived cancellation; clean receipt=%v", closeErr)
		if !waitForProcessGone(childPID, 10*time.Second) {
			t.Error("bounded negative-control child did not exit at its own deadline")
		}
	}
	if closeErr != nil {
		t.Fatalf("host cleanup: %v", closeErr)
	}
}
