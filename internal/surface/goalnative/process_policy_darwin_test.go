//go:build darwin

package goalnative

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestConfinedCommandFailsClosedForInvalidInput(t *testing.T) {
	for _, command := range [][]string{nil, {}, {""}, {"/bin/sh", "bad\x00argument"}} {
		if wrapped, err := confinedCommand(command); err == nil || wrapped != nil {
			t.Fatalf("confinedCommand(%#v) = %#v, %v; want refusal", command, wrapped, err)
		}
	}
}

func TestConfinedCommandUsesExactDenyOnlyProfile(t *testing.T) {
	original := []string{"/bin/sh", "-c", "printf ok", "--"}
	wrapped, err := confinedCommand(original)
	if err != nil {
		t.Fatal(err)
	}
	if len(wrapped) != len(original)+3 || wrapped[0] != sandboxExecPath || wrapped[1] != "-p" || wrapped[2] != processContainmentProfile() {
		t.Fatalf("wrapped command = %#v", wrapped)
	}
	if !strings.Contains(wrapped[2], "(allow default)") || !strings.Contains(wrapped[2], "(syscall-number 147)") || !strings.Contains(wrapped[2], "(syscall-number 82)") || !strings.Contains(wrapped[2], "(syscall-number 244) (with errno 78)") {
		t.Fatalf("profile = %q, want exact process-operation denies", wrapped[2])
	}
	if !strings.EqualFold(wrapped[2], processContainmentProfile()) {
		t.Fatal("profile changed between construction and inspection")
	}
	if !strings.HasSuffix(wrapped[2], ")") || unix.SYS_SETSID != 147 || unix.SYS_SETPGID != 82 || unix.SYS_POSIX_SPAWN != 244 || unix.ENOSYS != 78 { //nolint:staticcheck // verify both supported Darwin architectures' installed constants.
		t.Fatalf("Darwin syscall constants changed: setsid=%d setpgid=%d posix_spawn=%d enosys=%d", unix.SYS_SETSID, unix.SYS_SETPGID, unix.SYS_POSIX_SPAWN, unix.ENOSYS) //nolint:staticcheck // verify both supported Darwin architectures' installed constants.
	}
	if strings.Join(original, "\x00") != strings.Join([]string{"/bin/sh", "-c", "printf ok", "--"}, "\x00") {
		t.Fatal("confinedCommand mutated the caller's command")
	}
}

func runProcessPolicyCommand(t testing.TB, command []string, confined bool) (string, string, error) {
	t.Helper()
	argv := command
	if confined {
		var err error
		argv, err = confinedCommand(command)
		if err != nil {
			return "", "", err
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	process := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // test commands are fixed below
	var stdout, stderr bytes.Buffer
	process.Stdout, process.Stderr = &stdout, &stderr
	err := process.Run()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("process %q exceeded bounded timeout", command[0])
	}
	return stdout.String(), stderr.String(), err
}

func strictPolicyUnavailable(stderr string) bool {
	return strings.Contains(stderr, "sandbox_apply: Operation not permitted") || strings.Contains(stderr, "sandbox_init: Operation not permitted")
}

func requireStrictPolicy(t *testing.T) {
	t.Helper()
	_, stderr, err := runProcessPolicyCommand(t, []string{"/bin/sh", "-c", "printf strict-probe"}, true)
	if err != nil && strictPolicyUnavailable(stderr) {
		t.Skipf("sandbox-exec is unavailable in this test host: %s", strings.TrimSpace(stderr))
	}
	if err != nil {
		t.Fatalf("strict process policy probe failed: %v: %s", err, stderr)
	}
}

func TestConfinedProcessControlsRejectSessionAndGroupOwnership(t *testing.T) {
	requireStrictPolicy(t)
	probe := buildProcessControlProbe(t)
	controls := []struct {
		name string
		mark string
	}{
		{name: "setsid", mark: "session-escaped"},
		{name: "setpgid", mark: "group-escaped"},
	}
	for _, control := range controls {
		t.Run(control.name, func(t *testing.T) {
			command := []string{probe, control.name}
			unconfined, stderr, err := runProcessPolicyCommand(t, command, false)
			if err != nil || !strings.Contains(unconfined, control.mark) {
				t.Fatalf("unconfined %s control = %q, %q, %v; want successful escape control", control.name, unconfined, stderr, err)
			}
			confined, stderr, err := runProcessPolicyCommand(t, command, true)
			if err != nil || !strings.Contains(confined, strings.TrimSuffix(control.mark, "escaped")+"blocked") || strings.Contains(confined, control.mark) {
				t.Fatalf("confined %s control = %q, %q, %v; want blocked operation", control.name, confined, stderr, err)
			}
		})
	}
}

func TestConfinedDoubleForkCannotCreateNewSessionAfterGroupLeaderExit(t *testing.T) {
	requireStrictPolicy(t)
	probe := buildProcessControlProbe(t)
	command := []string{probe, "doublefork"}
	unconfined, stderr, err := runProcessPolicyCommand(t, command, false)
	if err != nil || !strings.Contains(unconfined, "session-escaped") {
		t.Fatalf("unconfined double fork = %q, %q, %v; want escaped control", unconfined, stderr, err)
	}
	confined, stderr, err := runProcessPolicyCommand(t, command, true)
	if err != nil || !strings.Contains(confined, "session-blocked") || strings.Contains(confined, "session-escaped") {
		t.Fatalf("confined double fork = %q, %q, %v; want clean exit with blocked session after leader exit", confined, stderr, err)
	}
}

const processControlProbeSource = `#include <errno.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/wait.h>
#include <unistd.h>
int main(int argc, char **argv) {
    if (argc < 2) return 2;
    if (strcmp(argv[1], "setsid") == 0) {
        if (setsid() == -1) { printf("session-blocked:%d\n", errno); return 0; }
        printf("session-escaped\n"); return 0;
    }
    if (strcmp(argv[1], "setpgid") == 0) {
        if (setpgid(0, 0) == -1) { printf("group-blocked:%d\n", errno); return 0; }
        printf("group-escaped\n"); return 0;
    }
    if (strcmp(argv[1], "doublefork") == 0) {
        pid_t child = fork();
        if (child == -1) return 2;
        if (child > 0) { waitpid(child, NULL, 0); return 0; }
        pid_t grandchild = fork();
        if (grandchild == -1) _exit(2);
        if (grandchild > 0) _exit(0);
        if (setsid() == -1) printf("session-blocked:%d\n", errno);
        else printf("session-escaped\n");
        fflush(stdout);
        usleep(50000);
        return 0;
    }
    return 2;
}`

func buildProcessControlProbe(t *testing.T) string {
	t.Helper()
	compiler, err := exec.LookPath("cc")
	if err != nil {
		t.Skip("C compiler is unavailable")
	}
	root := t.TempDir()
	source := filepath.Join(root, "process_control_probe.c")
	binary := filepath.Join(root, "process_control_probe")
	if err := os.WriteFile(source, []byte(processControlProbeSource), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	compile := exec.CommandContext(ctx, compiler, source, "-o", binary) //nolint:gosec // compiler and source are test-local.
	output, err := compile.CombinedOutput()
	if err != nil {
		t.Fatalf("compile process-control probe: %v: %s", err, output)
	}
	return binary
}

func TestConfinedNormalShellNodePythonAndGoCommands(t *testing.T) {
	commands := []struct {
		name   string
		binary string
		args   []string
		marker string
	}{
		{name: "shell", binary: "/bin/sh", args: []string{"-c", "printf shell-ok"}, marker: "shell-ok"},
		{name: "node", binary: "/opt/homebrew/bin/node", args: []string{"-e", "process.stdout.write('node-ok')"}, marker: "node-ok"},
		{name: "go", binary: "/opt/homebrew/bin/go", args: []string{"version"}, marker: "go version go"},
	}
	requireStrictPolicy(t)
	for _, testCase := range commands {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := exec.LookPath(filepath.Base(testCase.binary)); err != nil {
				t.Skipf("%s is unavailable", testCase.binary)
			}
			stdout, stderr, err := runProcessPolicyCommand(t, append([]string{testCase.binary}, testCase.args...), true)
			if err != nil || !strings.Contains(stdout, testCase.marker) {
				t.Fatalf("confined %s = %q, %q, %v; want marker %q", testCase.name, stdout, stderr, err, testCase.marker)
			}
		})
	}
}

const posixSpawnProbeSource = `#include <spawn.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/wait.h>
extern char **environ;
int main(int argc, char **argv) {
    posix_spawnattr_t attr;
    int rc = posix_spawnattr_init(&attr);
    if (rc) { printf("init:%d\n", rc); return 2; }
    short flags = 0;
    if (argc > 1 && strcmp(argv[1], "setpgroup") == 0) {
        flags |= POSIX_SPAWN_SETPGROUP;
        rc = posix_spawnattr_setpgroup(&attr, 0);
        if (rc) { printf("setpgroup:%d\n", rc); return 2; }
    }
    if (argc > 1 && strcmp(argv[1], "setsid") == 0) {
#if defined(POSIX_SPAWN_SETSID)
        flags |= POSIX_SPAWN_SETSID;
#elif defined(POSIX_SPAWN_SETSID_NP)
        flags |= POSIX_SPAWN_SETSID_NP;
#else
#error "Darwin SDK has no POSIX_SPAWN_SETSID flag"
#endif
    }
    rc = posix_spawnattr_setflags(&attr, flags);
    if (rc) { printf("flags:%d\n", rc); return 2; }
    char *child_argv[] = { (char *)"/bin/sh", (char *)"-c", (char *)"exit 0", NULL };
    pid_t pid = 0;
    rc = posix_spawn(&pid, child_argv[0], NULL, &attr, child_argv, environ);
    posix_spawnattr_destroy(&attr);
    if (rc) { printf("spawn-error:%d\n", rc); return 3; }
    int status = 0;
    waitpid(pid, &status, 0);
    printf("spawn-ok:%d\n", status);
    return 0;
}`

func buildPosixSpawnProbe(t *testing.T) string {
	t.Helper()
	compiler, err := exec.LookPath("cc")
	if err != nil {
		t.Skip("C compiler is unavailable")
	}
	root := t.TempDir()
	source := filepath.Join(root, "posix_spawn_probe.c")
	binary := filepath.Join(root, "posix_spawn_probe")
	if err := os.WriteFile(source, []byte(posixSpawnProbeSource), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	compile := exec.CommandContext(ctx, compiler, source, "-o", binary) //nolint:gosec // compiler and source are test-local.
	output, err := compile.CombinedOutput()
	if err != nil {
		t.Fatalf("compile posix_spawn probe: %v: %s", err, output)
	}
	return binary
}

func TestConfinedPosixSpawnSetGroupAndSessionFlagsFailClosed(t *testing.T) {
	requireStrictPolicy(t)
	probe := buildPosixSpawnProbe(t)
	for _, mode := range []string{"setpgroup", "setsid"} {
		t.Run(mode, func(t *testing.T) {
			unconfined, stderr, err := runProcessPolicyCommand(t, []string{probe, mode}, false)
			if err != nil || !strings.Contains(unconfined, "spawn-ok:0") {
				t.Fatalf("unconfined %s = %q, %q, %v; want spawn-ok", mode, unconfined, stderr, err)
			}
			confined, stderr, err := runProcessPolicyCommand(t, []string{probe, mode}, true)
			if err == nil || !strings.Contains(confined, "spawn-error:78") || strings.Contains(confined, "spawn-ok") {
				t.Fatalf("confined %s = %q, %q, %v; want ENOSYS refusal", mode, confined, stderr, err)
			}
		})
	}
}

func TestConfinedNodeChildProcessSpawnWorks(t *testing.T) {
	requireStrictPolicy(t)
	node := "/opt/homebrew/bin/node"
	if _, err := os.Stat(node); err != nil {
		t.Skipf("%s is unavailable", node)
	}
	commands := []struct {
		name string
		code string
		mark string
	}{
		{name: "execFile", code: `require("child_process").execFile("/bin/echo", ["node-execFile-ok"], (error, stdout) => { if (error) process.exit(1); process.stdout.write(stdout) })`, mark: "node-execFile-ok"},
		{name: "spawn", code: `const child=require("child_process").spawn("/bin/echo", ["node-spawn-ok"]); child.stdout.on("data", d=>process.stdout.write(d)); child.on("error", ()=>process.exit(1)); child.on("close", c=>process.exit(c||0))`, mark: "node-spawn-ok"},
	}
	for _, testCase := range commands {
		t.Run(testCase.name, func(t *testing.T) {
			stdout, stderr, err := runProcessPolicyCommand(t, []string{node, "-e", testCase.code}, true)
			if err != nil || !strings.Contains(stdout, testCase.mark) {
				t.Fatalf("confined Node %s = %q, %q, %v; want marker %q", testCase.name, stdout, stderr, err, testCase.mark)
			}
		})
	}
}

func TestConfinedPythonFrameworkLaunchersExposeCompatibilityFailure(t *testing.T) {
	requireStrictPolicy(t)
	launchers := []string{"/usr/bin/python3", "/opt/homebrew/bin/python3"}
	for _, launcher := range launchers {
		t.Run(filepath.Base(launcher), func(t *testing.T) {
			if _, err := os.Stat(launcher); err != nil {
				t.Skipf("%s is unavailable", launcher)
			}
			unconfined, stderr, err := runProcessPolicyCommand(t, []string{launcher, "-c", "import subprocess; print(subprocess.check_output(['/bin/echo','python-child-ok']).decode(), end='')"}, false)
			if err != nil || !strings.Contains(unconfined, "python-child-ok") {
				t.Fatalf("unconfined %s = %q, %q, %v; want child marker", launcher, unconfined, stderr, err)
			}
			_, strictStderr, strictErr := runProcessPolicyCommand(t, []string{launcher, "-c", "print('python-strict-ok')"}, true)
			if strictErr == nil || !strings.Contains(strictStderr, "posix_spawn:") {
				t.Fatalf("strict %s = %q, %v; want explicit framework-launcher incompatibility", launcher, strictStderr, strictErr)
			}
			t.Logf("strict %s is incompatible with the framework launcher: %s", launcher, strings.TrimSpace(strictStderr))
		})
	}
}
