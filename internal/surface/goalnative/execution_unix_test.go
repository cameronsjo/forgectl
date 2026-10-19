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
	"strings"
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
	if _, err := StartCommandHost(t.Context(), Config{Cwd: root, WritableRoots: []string{work}}); err == nil || !strings.Contains(err.Error(), "cwd") {
		t.Fatalf("startup accepted a cwd outside its ceiling: %v", err)
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
	for _, argv := range [][]string{nil, {}, {""}, {" "}, {"printf", "bad\x00argument"}} {
		if _, err := host.Exec(t.Context(), argv, work, []string{work}, time.Second); err == nil {
			t.Fatalf("invalid execution argv accepted: %#v", argv)
		}
	}
	result, err := host.Exec(t.Context(), []string{"printf", "ok"}, work, []string{work}, time.Second)
	if err != nil || result.Stdout != "out" {
		t.Fatalf("approved command did not use the live transport: %#v, %v", result, err)
	}
	if _, err := host.Exec(t.Context(), []string{"printf", "bad"}, work, []string{root}, time.Second); err == nil {
		t.Fatal("command widened the host's writable roots")
	}
	narrow := filepath.Join(work, "narrow")
	if err := os.Mkdir(narrow, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := host.Exec(t.Context(), []string{"printf", "bad"}, narrow, []string{narrow}, time.Second); err == nil {
		t.Fatal("command omitted the native host's implicitly writable startup cwd")
	}
	if _, err := host.Exec(t.Context(), []string{"printf", "bad"}, root, []string{work}, time.Second); err == nil {
		t.Fatal("command cwd widened the approved writable roots")
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
	data, err := os.ReadFile(allowed) //nolint:gosec // Test-owned positive-control output.
	if err != nil || string(data) != "allowed" {
		t.Fatalf("allowed write did not work: %q, %v", data, err)
	}
	if _, err := os.Stat(denied); !os.IsNotExist(err) {
		t.Fatalf("denied target was created: %v", err)
	}
	probeSocket := filepath.Join(root, "private.sock")
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "unix", probeSocket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	control, err := (&net.Dialer{}).DialContext(t.Context(), "unix", probeSocket)
	if err != nil {
		t.Fatalf("Unix socket positive control failed: %v", err)
	}
	_ = control.Close()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal(err)
	}
	result, err = host.Exec(t.Context(), []string{node, "-e", `const net=require("net"); const channel=net.createConnection(process.argv[1]); channel.on("connect",()=>process.exit(0)); channel.on("error",error=>{ console.error(error.message); process.exit(1) })`, probeSocket}, work, []string{work}, 10*time.Second)
	if err != nil || result.ExitCode == 0 || !strings.Contains(result.Stderr, "connect EPERM "+probeSocket) {
		t.Fatalf("sandboxed tools reached the private role socket: %#v, %v", result, err)
	}
	network, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = network.Close() })
	connection, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", network.Addr().String())
	if err != nil {
		t.Fatalf("TCP positive control failed: %v", err)
	}
	_ = connection.Close()
	_, port, err := net.SplitHostPort(network.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	result, err = host.Exec(t.Context(), []string{node, "-e", `const net=require("net"); const channel=net.createConnection({host:"127.0.0.1",port:Number(process.argv[1])}); channel.on("connect",()=>process.exit(0)); channel.on("error",error=>{ console.error(error.message); process.exit(1) })`, port}, work, []string{work}, 10*time.Second)
	if err != nil || result.ExitCode == 0 || !strings.Contains(result.Stderr, "connect EPERM 127.0.0.1:"+port) {
		t.Fatalf("sandboxed tools reached the TCP listener: %#v, %v", result, err)
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

func TestExecutionHostRealRoleScopedCommands(t *testing.T) {
	if os.Getenv("FORGECTL_GOALNATIVE_INTEGRATION") != "1" {
		t.Skip("set FORGECTL_GOALNATIVE_INTEGRATION=1 for real role-scoped command hosts")
	}
	native, err := exec.LookPath("codex")
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp("/tmp", "gn-roles-")
	if err != nil {
		t.Fatal(err)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	directories := make(map[string]string)
	for _, name := range []string{"coordinator", "implementation", "reviewer"} {
		directories[name] = filepath.Join(root, name)
		if err := os.Mkdir(directories[name], 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"coordinator", "implementation", "reviewer"} {
		t.Run(name, func(t *testing.T) {
			directory := directories[name]
			ceiling := []string{directory}
			if name == "reviewer" {
				ceiling = append(ceiling, directories["implementation"])
			}
			host, err := StartCommandHost(t.Context(), Config{Binary: native, SupervisorBinary: writeHelper(t, "supervisor", "GOALNATIVE_HOST_HELPER=1"), ControlDir: filepath.Join(root, name+"-control"), Cwd: directory, Socket: filepath.Join(root, name+".sock"), WritableRoots: ceiling})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = host.Close(context.Background()) })
			allowed := filepath.Join(directory, "allowed")
			result, err := host.Exec(t.Context(), []string{"/bin/sh", "-c", `printf allowed > "$1"`, "probe", allowed}, directory, []string{directory}, 10*time.Second)
			if err != nil || result.ExitCode != 0 {
				t.Fatalf("role positive control: %#v, %v", result, err)
			}
			roleRoot, err := os.OpenRoot(directory)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = roleRoot.Close() })
			if data, err := roleRoot.ReadFile("allowed"); err != nil || string(data) != "allowed" {
				t.Fatalf("role allowed write missing: %q, %v", data, err)
			}
			for other, otherDirectory := range directories {
				if other == name {
					continue
				}
				target := filepath.Join(otherDirectory, "denied-from-"+name)
				result, err := host.Exec(t.Context(), []string{"/bin/sh", "-c", `printf denied > "$1"`, "probe", target}, directory, []string{directory}, 10*time.Second)
				if err != nil || result.ExitCode == 0 || !strings.Contains(result.Stderr, "Operation not permitted") {
					t.Fatalf("%s wrote into %s: %#v, %v", name, other, result, err)
				}
				if _, err := os.Stat(target); !os.IsNotExist(err) {
					t.Fatalf("cross-role target exists: %v", err)
				}
			}
			if name == "reviewer" {
				target := filepath.Join(directories["implementation"], "approved-check")
				result, err := host.Exec(t.Context(), []string{"/bin/sh", "-c", `printf check > "$1"`, "probe", target}, directories["implementation"], ceiling, 10*time.Second)
				if err != nil || result.ExitCode != 0 {
					t.Fatalf("explicit supervisor check scope failed: %#v, %v", result, err)
				}
				checkRoot, err := os.OpenRoot(directories["implementation"])
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = checkRoot.Close() })
				if data, err := checkRoot.ReadFile("approved-check"); err != nil || string(data) != "check" {
					t.Fatalf("approved check write missing: %q, %v", data, err)
				}
			}
			if err := host.Close(t.Context()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestExecutionHostRealProcessGroupCleanupDoesNotCoverEscapedChildren(t *testing.T) {
	if os.Getenv("FORGECTL_GOALNATIVE_INTEGRATION") != "1" {
		t.Skip("set FORGECTL_GOALNATIVE_INTEGRATION=1 for the escaped-child negative control")
	}
	native, err := exec.LookPath("codex")
	if err != nil {
		t.Fatal(err)
	}
	probe := buildEscapedChildProbe(t)
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
	unconfinedPIDPath := filepath.Join(root, "unconfined.pid")
	unconfinedStatePath := filepath.Join(root, "unconfined.state")
	control := exec.CommandContext(t.Context(), probe, unconfinedPIDPath, unconfinedStatePath) //nolint:gosec // test-local compiled probe.
	if err := control.Start(); err != nil {
		t.Fatal(err)
	}
	unconfinedPID, err := waitForPIDFile(unconfinedPIDPath, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !processAlive(unconfinedPID) {
		t.Fatal("unconfined escaped child was not alive before its deadline")
	}
	if err := control.Wait(); err != nil {
		t.Fatalf("unconfined control: %v", err)
	}
	if data, err := os.ReadFile(unconfinedStatePath); err != nil || strings.TrimSpace(string(data)) != "escaped" {
		t.Fatalf("unconfined control state = %q, %v; want escaped", data, err)
	}
	if processAlive(unconfinedPID) {
		t.Fatal("unconfined self-expiring child outlived its bounded deadline")
	}

	pidPath := filepath.Join(work, "sandboxed.pid")
	statePath := filepath.Join(work, "sandboxed.state")
	commandCtx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := host.Exec(commandCtx, []string{probe, pidPath, statePath}, work, []string{work}, 20*time.Second)
		done <- err
	}()
	childPID, err := waitForPIDFile(pidPath, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if !waitForProcessGone(childPID, 10*time.Second) {
			t.Error("self-expiring escaped-child probe exceeded its deadline")
		}
	})
	if !processAlive(childPID) {
		t.Fatal("sandboxed child was not alive before cancellation")
	}
	if data, err := os.ReadFile(statePath); err != nil || strings.TrimSpace(string(data)) != "escaped" { //nolint:gosec // Test-owned process-control output.
		t.Fatalf("sandboxed process state = %q, %v; want escaped negative control", data, err)
	}
	cancel()
	select {
	case err := <-done:
		if err == nil || !errors.Is(err, context.Canceled) {
			t.Errorf("canceled command did not report cancellation: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("command cancellation did not settle")
	}
	closeErr := host.Close(t.Context())
	if closeErr != nil {
		t.Fatalf("host cleanup: %v", closeErr)
	}
	if !processAlive(childPID) {
		t.Fatal("escaped-child negative control did not survive group cleanup; re-evaluate the documented cleanup boundary")
	}
	t.Log("owned process-group cleanup succeeded while the self-expiring escaped child remained alive; this is not all-descendant containment")
}

const escapedChildProbeSource = `#include <errno.h>
#include <stdio.h>
#include <stdlib.h>
#include <unistd.h>
int main(int argc, char **argv) {
    if (argc != 3) return 2;
    pid_t child = fork();
    if (child == -1) return 2;
    if (child > 0) {
        int status = 0;
        waitpid(child, &status, 0);
        return WIFEXITED(status) ? WEXITSTATUS(status) : 1;
    }
    FILE *pid = fopen(argv[1], "w");
    if (pid == NULL) _exit(2);
    fprintf(pid, "%d\n", (int)getpid());
    fclose(pid);
    FILE *state = fopen(argv[2], "w");
    if (state == NULL) _exit(2);
    if (setsid() == -1) fputs("blocked\n", state);
    else fputs("escaped\n", state);
    fclose(state);
    sleep(8);
    return 0;
}`

func buildEscapedChildProbe(t *testing.T) string {
	t.Helper()
	compiler, err := exec.LookPath("cc")
	if err != nil {
		t.Skip("C compiler is unavailable")
	}
	root := t.TempDir()
	source := filepath.Join(root, "escaped_child_probe.c")
	binary := filepath.Join(root, "escaped_child_probe")
	if err := os.WriteFile(source, []byte("#include <sys/wait.h>\n"+escapedChildProbeSource), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	compile := exec.CommandContext(ctx, compiler, source, "-o", binary) //nolint:gosec // compiler and source are test-local.
	output, err := compile.CombinedOutput()
	if err != nil {
		t.Fatalf("compile escaped-child probe: %v: %s", err, output)
	}
	return binary
}

func TestExecutionHostRealSupervisorDeathCleansActiveCommandAndChild(t *testing.T) {
	testRealCodexAbruptSupervisorDeath(t, true)
}
