//go:build unix

package goalcontainer

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/surface/goalnative"
)

func realConfig(t *testing.T) Config {
	t.Helper()
	if os.Getenv("FORGECTL_CONTAINER_INTEGRATION") != "1" {
		t.Skip("set FORGECTL_CONTAINER_INTEGRATION=1 with an approved local image, socket and shared test root")
	}
	base := os.Getenv("FORGECTL_CONTAINER_TEST_ROOT")
	if err := canonicalDirectory(base); err != nil {
		t.Fatalf("shared test root: %v", err)
	}
	directory, err := os.MkdirTemp(base, "container-proof-")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("retained container evidence: %s", directory)
	config := fixtureConfig(t, directory)
	config.Isolation.Socket, config.Isolation.Image = os.Getenv("FORGECTL_CONTAINER_SOCKET"), os.Getenv("FORGECTL_CONTAINER_IMAGE")
	if err := config.Isolation.Validate(); err != nil {
		t.Fatal(err)
	}
	return config
}

func closeRealHost(t *testing.T, host *Host) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := host.Close(ctx); err != nil {
		t.Errorf("container cleanup: %v", err)
	}
}

func TestContainerRealBoundaryAndFileHelper(t *testing.T) {
	config := realConfig(t)
	host, err := New(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer closeRealHost(t, host)
	result, err := host.File(t.Context(), config.Worktree, "file.txt", true, "contained file helper\n", config.Worktree, config.WritableRoots)
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("file write: %+v %v", result, err)
	}
	result, err = host.File(t.Context(), config.Worktree, "file.txt", false, "", config.Worktree, config.WritableRoots)
	if err != nil || result.ExitCode != 0 || result.Stdout != "contained file helper\n" {
		t.Fatalf("file read: %+v %v", result, err)
	}
	script := `import errno,json,os,socket
assert os.getuid() != 0 and os.getgid() != 0 and os.getgroups() == []
assert os.readlink('/proc/self/fd/0') == '/dev/null'
protected,metadata,runtime = __import__('sys').argv[1:]
assert open(protected).read().startswith('gitdir:')
assert open(metadata).read().startswith('ref:')
for target in (protected,metadata,'/outside-boundary'):
    try:
        descriptor=os.open(target,os.O_WRONLY|os.O_APPEND|os.O_CREAT,0o600)
    except OSError as failure:
        assert failure.errno==errno.EROFS,(target,failure)
    else:
        os.close(descriptor)
        raise AssertionError('write boundary absent: '+target)
assert not os.path.exists(runtime)
try:
    descriptor=os.open('/proc/1/fd/0',os.O_WRONLY)
except OSError as failure:
    assert failure.errno==errno.EACCES,failure
else:
    os.close(descriptor)
    raise AssertionError('workload can spoof lease')
try:
    os.kill(1,19)
except OSError as failure:
    assert failure.errno==errno.EPERM,failure
else:
    raise AssertionError('workload can freeze PID 1')
connection=socket.socket()
connection.settimeout(1)
try:
    connection.connect(('1.1.1.1',80))
except OSError as failure:
    assert failure.errno==errno.ENETUNREACH,failure
else:
    raise AssertionError('container has network')
finally:
    connection.close()
assert not os.path.exists('/var/run/docker.sock')
print('CONTAINER-BOUNDARY-PROVEN')`
	result, err = host.Exec(t.Context(), []string{"python3", "-c", script, filepath.Join(config.Worktree, ".git"), filepath.Join(config.GitDirectories[0], "HEAD"), config.ControlDir}, config.Worktree, config.WritableRoots, 15*time.Second)
	if err != nil || result.ExitCode != 0 || result.Stdout != "CONTAINER-BOUNDARY-PROVEN\n" {
		t.Fatalf("boundary proof: %+v %v", result, err)
	}
	assertRemovedReceipts(t, config)
}

const daemonProbe = `import os,signal,time
output=__import__('sys').argv[1]
child=os.fork()
if child==0:
    os.setsid()
    grandchild=os.fork()
    if grandchild!=0:
        os._exit(0)
    signal.signal(signal.SIGTERM,signal.SIG_IGN)
    time.sleep(0.05)
    with open(output+'/daemon.json','w') as stream:
        __import__('json').dump({'pid':os.getpid(),'parent':os.getppid(),'group':os.getpgrp(),'session':os.getsid(0)},stream)
    end=time.monotonic()+20
    while time.monotonic()<end:
        with open(output+'/heartbeat','w') as stream:
            stream.write(str(time.monotonic_ns()))
        time.sleep(0.05)
    os._exit(0)
os.waitpid(child,0)
while True:
    time.sleep(1)`

func waitDaemon(t *testing.T, config Config) {
	t.Helper()
	root, err := os.OpenRoot(config.Directory)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		data, err := root.ReadFile("daemon.json")
		if err == nil {
			var identity struct{ PID, Parent, Group, Session int }
			if json.Unmarshal(data, &identity) == nil && identity.PID > 1 && identity.Parent == 1 && identity.Group == identity.Session && identity.Session != identity.PID && identity.Session > 1 {
				first, err := root.ReadFile("heartbeat")
				if err == nil {
					time.Sleep(100 * time.Millisecond)
					second, err := root.ReadFile("heartbeat")
					if err == nil && string(first) != string(second) {
						return
					}
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("double-forked daemon never proved live outside the command's process group")
}

func assertHeartbeatStopped(t *testing.T, config Config) {
	t.Helper()
	root, err := os.OpenRoot(config.Directory)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	first, err := root.ReadFile("heartbeat")
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	second, err := root.ReadFile("heartbeat")
	if err != nil || string(first) != string(second) {
		t.Fatalf("escaped daemon survived: %v", err)
	}
}

func assertRemovedReceipts(t *testing.T, config Config) {
	t.Helper()
	root, err := os.OpenRoot(config.ControlDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	files, err := os.ReadDir(config.ControlDir)
	if err != nil {
		t.Fatal(err)
	}
	started, finished := map[string]bool{}, map[string]bool{}
	for _, entry := range files {
		data, err := root.ReadFile(entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		var receipt Receipt
		if err := json.Unmarshal(data, &receipt); err != nil {
			t.Fatal(err)
		}
		if receipt.Phase == "creating" {
			started[receipt.Nonce] = true
		}
		if receipt.Phase == "removed" {
			if !receipt.Stopped || !receipt.Removed || receipt.State.PID != 0 || receipt.State.Running || receipt.ImageID != config.Isolation.Image {
				t.Fatalf("invalid cleanup receipt: %+v", receipt)
			}
			api := newClient(config.Isolation.Socket)
			_, err := api.inspect(t.Context(), receipt.ContainerID)
			api.transport.CloseIdleConnections()
			var absent apiError
			if !errors.As(err, &absent) || absent.status != http.StatusNotFound {
				t.Fatalf("retained cleanup did not match actual runtime absence: %v", err)
			}
			finished[receipt.Nonce] = true
		}
	}
	if len(started) == 0 || len(started) != len(finished) {
		t.Fatalf("unpaired container receipts: started=%v finished=%v", started, finished)
	}
	for nonce := range started {
		if !finished[nonce] {
			t.Fatalf("container %s lacks verified removal", nonce)
		}
	}
}

func TestContainerRealCancellationKillsEscapedDescendants(t *testing.T) {
	config := realConfig(t)
	host, err := New(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer closeRealHost(t, host)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := host.Exec(ctx, []string{"python3", "-c", daemonProbe, config.Directory}, config.Worktree, config.WritableRoots, 25*time.Second)
		done <- err
	}()
	waitDaemon(t, config)
	started := time.Now()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || errors.Is(err, goalnative.ErrCleanupUnconfirmed) {
			t.Fatalf("cancellation: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("container cancellation exceeded its bound")
	}
	if time.Since(started) > 7*time.Second {
		t.Fatal("container cancellation exceeded the lease/cleanup bound")
	}
	assertHeartbeatStopped(t, config)
	assertRemovedReceipts(t, config)
}

func TestContainerRealLostLeaseAndInitDeathKillEscapedDescendants(t *testing.T) {
	for _, mode := range []string{"eof", "stalled", "init-killed"} {
		t.Run(mode, func(t *testing.T) {
			config := realConfig(t)
			host, err := New(t.Context(), config)
			if err != nil {
				t.Fatal(err)
			}
			defer closeRealHost(t, host)
			var entropy [16]byte
			if _, err := rand.Read(entropy[:]); err != nil {
				t.Fatal(err)
			}
			nonce := hex.EncodeToString(entropy[:])
			argv := []string{"python3", "-c", daemonProbe, config.Directory}
			request, err := host.request(argv, config.Worktree, config.WritableRoots, nonce, time.Now().Add(25*time.Second))
			if err != nil {
				t.Fatal(err)
			}
			current := &operation{host: host, request: request, receipt: Receipt{Version: 1, Name: "fg-goal-" + nonce, Nonce: nonce, Role: config.Role, ImageID: config.Isolation.Image, Policy: request.HostConfig}}
			if err := current.record("creating", nil); err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := current.cleanup(); err != nil {
					t.Errorf("owned fault-probe cleanup: %v", err)
				}
			}()
			var created struct{ ID string }
			if err := host.api.request(t.Context(), http.MethodPost, "/containers/create?name="+current.receipt.Name, request, &created); err != nil {
				t.Fatal(err)
			}
			current.receipt.ContainerID = created.ID
			observed, err := host.api.inspect(t.Context(), created.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err := errors.Join(verifyIdentity(observed, request, current.receipt.Name, created.ID), verifyPolicy(observed, request)); err != nil {
				t.Fatal(err)
			}
			if err := current.record("created", nil); err != nil {
				t.Fatal(err)
			}
			connection, _, err := host.api.attach(t.Context(), created.ID)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = connection.Close() }()
			if err := current.record("starting", nil); err != nil {
				t.Fatal(err)
			}
			if err := host.api.request(t.Context(), http.MethodPost, "/containers/"+created.ID+"/start", nil, nil); err != nil {
				t.Fatal(err)
			}
			if err := current.record("running", nil); err != nil {
				t.Fatal(err)
			}
			if _, err := connection.Write([]byte("alive\n")); err != nil {
				t.Fatal(err)
			}
			waitDaemon(t, config)
			if _, err := connection.Write([]byte("alive\n")); err != nil {
				t.Fatal(err)
			}
			started := time.Now()
			expectedExit := 124
			switch mode {
			case "eof":
				if err := connection.Close(); err != nil {
					t.Fatal(err)
				}
			case "stalled":
				time.Sleep(time.Second)
				live, err := host.api.inspect(t.Context(), created.ID)
				if err != nil || !live.State.Running || live.State.PID == 0 {
					t.Fatalf("stalled stream was not held open with a live namespace: %v", err)
				}
				waitDaemon(t, config)
			case "init-killed":
				expectedExit = 137
				if err := host.api.request(t.Context(), http.MethodPost, "/containers/"+created.ID+"/kill?signal=KILL", nil, nil); err != nil {
					t.Fatal(err)
				}
			}
			for time.Since(started) < 7*time.Second {
				observed, err = host.api.inspect(t.Context(), created.ID)
				if err != nil {
					t.Fatal(err)
				}
				if !observed.State.Running && observed.State.PID == 0 {
					break
				}
				time.Sleep(50 * time.Millisecond)
			}
			if observed.State.Running || observed.State.PID != 0 || observed.State.ExitCode != expectedExit || time.Since(started) >= 7*time.Second || (mode == "stalled" && time.Since(started) < 2*time.Second) {
				t.Fatalf("fault did not terminate its namespace within the lease bound: duration=%s state=%+v", time.Since(started), observed.State)
			}
			assertHeartbeatStopped(t, config)
			t.Logf("%s terminated the observed escaped daemon in %s", mode, time.Since(started))
		})
	}
}

func TestContainerRealOwnerHelper(t *testing.T) {
	encoded := os.Getenv("FORGECTL_CONTAINER_OWNER_CONFIG")
	if encoded == "" {
		t.Skip("only the owned subprocess invokes this helper")
	}
	var config Config
	if err := json.Unmarshal([]byte(encoded), &config); err != nil {
		t.Fatal(err)
	}
	host, err := New(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer closeRealHost(t, host)
	_, err = host.Exec(t.Context(), []string{"python3", "-c", daemonProbe, config.Directory}, config.Worktree, config.WritableRoots, 25*time.Second)
	if err != nil {
		t.Fatal(err)
	}
}

func TestContainerRealLogicalOwnerDeathKillsEscapedDescendants(t *testing.T) {
	config := realConfig(t)
	encoded, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	owner := exec.CommandContext(t.Context(), executable, "-test.run=^TestContainerRealOwnerHelper$", "-test.v")
	owner.Env = append(os.Environ(), "FORGECTL_CONTAINER_OWNER_CONFIG="+string(encoded))
	owner.Stdout, owner.Stderr = os.Stdout, os.Stderr
	if err := owner.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	defer func() {
		if !waited {
			_ = owner.Process.Kill()
			_ = owner.Wait()
		}
	}()
	waitDaemon(t, config)
	root, err := os.OpenRoot(config.ControlDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	files, err := os.ReadDir(config.ControlDir)
	if err != nil {
		t.Fatal(err)
	}
	var running Receipt
	for _, entry := range files {
		if !strings.HasSuffix(entry.Name(), "-running.json") {
			continue
		}
		data, err := root.ReadFile(entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		var receipt Receipt
		if err := json.Unmarshal(data, &receipt); err != nil {
			t.Fatal(err)
		}
		if receipt.UpdatedAt.After(running.UpdatedAt) {
			running = receipt
		}
	}
	if !identityPattern.MatchString(running.ContainerID) {
		t.Fatal("no captured live container identity")
	}
	api := newClient(config.Isolation.Socket)
	defer api.transport.CloseIdleConnections()
	observed, err := api.inspect(t.Context(), running.ContainerID)
	if err != nil || !observed.State.Running || observed.Config.Labels["io.forgectl.goal-owner"] != digest([]byte(config.ControlDir)) || observed.Config.Labels["io.forgectl.goal-nonce"] != running.Nonce || observed.Image != config.Isolation.Image {
		t.Fatalf("owned live identity not proven: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		remaining, err := api.inspect(ctx, running.ContainerID)
		if err != nil {
			t.Errorf("inspect owned owner-death container: %v", err)
			return
		}
		if err := verifyIdentity(remaining, observed.Config, running.Name, running.ContainerID); err != nil {
			t.Errorf("owned identity changed: %v", err)
			return
		}
		if remaining.State.Running {
			_ = api.request(ctx, http.MethodPost, "/containers/"+running.ContainerID+"/kill?signal=KILL", nil, nil)
		}
		if err := api.request(ctx, http.MethodDelete, "/containers/"+running.ContainerID, nil, nil); err != nil {
			t.Errorf("remove owned owner-death container: %v", err)
		}
	}()
	started := time.Now()
	if err := owner.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := owner.Wait(); err == nil {
		t.Fatal("logical owner was not killed")
	}
	waited = true
	for time.Since(started) < 7*time.Second {
		observed, err = api.inspect(t.Context(), running.ContainerID)
		if err != nil {
			t.Fatal(err)
		}
		if !observed.State.Running && observed.State.PID == 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if observed.State.Running || observed.State.PID != 0 || time.Since(started) >= 7*time.Second {
		t.Fatal("owner death left the tool namespace active")
	}
	assertHeartbeatStopped(t, config)
	data, err := json.Marshal(observed.State)
	if err != nil {
		t.Fatal(err)
	}
	if err := root.WriteFile("owner-death-observed.json", data, 0o600); err != nil {
		t.Fatal(err)
	}
}
