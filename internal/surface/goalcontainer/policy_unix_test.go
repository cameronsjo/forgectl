//go:build unix

package goalcontainer

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/cameronsjo/forgectl/internal/privdir"
	"github.com/cameronsjo/forgectl/internal/surface/goalnative"
)

func fixtureConfig(t *testing.T, base string) Config {
	t.Helper()
	canonical, err := filepath.EvalSymlinks(base)
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(canonical)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	for _, name := range []string{"role", "role/cache", "worktree", "metadata", "metadata/worktree", "control", "run"} {
		if err := root.Mkdir(name, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for name, content := range map[string]string{"worktree/.git": "gitdir: ../metadata/worktree\n", "metadata/worktree/HEAD": "ref: refs/heads/goal\n", "metadata/HEAD": "ref: refs/heads/main\n", "worktree/file.txt": "original\n"} {
		if err := root.WriteFile(name, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	directory, worktree := filepath.Join(canonical, "role"), filepath.Join(canonical, "worktree")
	return Config{Isolation: Spec{Mode: Mode, Socket: "/run/docker.sock", Image: "sha256:" + strings.Repeat("a", 64)}, Role: "implementation", Directory: directory, Worktree: worktree, GitDirectories: []string{filepath.Join(canonical, "metadata/worktree"), filepath.Join(canonical, "metadata")}, ControlDir: filepath.Join(canonical, "control"), PatchPath: filepath.Join(canonical, "run/result.patch"), WritableRoots: []string{directory, worktree}}
}

func fixtureRequest(t *testing.T) (Config, createRequest) {
	t.Helper()
	config := fixtureConfig(t, t.TempDir())
	if err := config.validate(); err != nil {
		t.Fatal(err)
	}
	host := &Host{config: config}
	request, err := host.request([]string{"/bin/sh", "-c", "printf allowed"}, config.Worktree, config.WritableRoots, strings.Repeat("b", 32), time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	return config, request
}

func inspectionFor(t *testing.T, request createRequest, name string) inspection {
	t.Helper()
	mounts := make([]map[string]any, 0, len(request.HostConfig.Mounts))
	for _, entry := range request.HostConfig.Mounts {
		mounts = append(mounts, map[string]any{"Type": entry.Type, "Source": entry.Source, "Destination": entry.Target, "RW": !entry.ReadOnly, "Propagation": "rprivate"})
	}
	data, err := json.Marshal(map[string]any{"Id": strings.Repeat("c", 64), "Name": "/" + name, "Image": request.Image, "Config": request, "HostConfig": request.HostConfig, "State": map[string]any{"Status": "created"}, "Mounts": mounts})
	if err != nil {
		t.Fatal(err)
	}
	var observed inspection
	if err := json.Unmarshal(data, &observed); err != nil {
		t.Fatal(err)
	}
	return observed
}

func TestContainerPolicyBindsOnlyRoleCeilingAndProtectsGit(t *testing.T) {
	config, request := fixtureRequest(t)
	if len(request.HostConfig.Mounts) != 4 {
		t.Fatalf("unexpected mounts: %+v", request.HostConfig.Mounts)
	}
	for _, entry := range request.HostConfig.Mounts {
		if entry.Source == config.ControlDir || entry.Target != entry.Source || !entry.BindOptions.NonRecursive || entry.BindOptions.Propagation != "rprivate" {
			t.Fatalf("unsafe mount: %+v", entry)
		}
		if entry.Source == filepath.Join(config.Worktree, ".git") && !entry.ReadOnly {
			t.Fatal("worktree Git pointer is writable")
		}
		if within(entry.Source, filepath.Dir(config.GitDirectories[0])) && !entry.ReadOnly {
			t.Fatal("Git metadata is writable")
		}
	}
	host := &Host{config: config}
	for name, roots := range map[string][]string{"control": {config.Directory, config.ControlDir}, "parent": {filepath.Dir(config.Directory)}, "missing-runtime": {config.Worktree}} {
		t.Run(name, func(t *testing.T) {
			if _, err := host.request([]string{"true"}, config.Worktree, roots, "nonce", time.Now().Add(time.Minute)); err == nil {
				t.Fatal("request escaped ceiling")
			}
		})
	}
	config.Role = "reviewer"
	host.config = config
	readonly, err := host.request([]string{"true"}, config.Directory, []string{config.Directory}, "nonce", time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if !readonly.HostConfig.Mounts[0].ReadOnly {
		t.Fatal("reviewer model can write worktree")
	}
	checked, err := host.request([]string{"true"}, config.Worktree, config.WritableRoots, "nonce", time.Now().Add(time.Minute))
	if err != nil || checked.HostConfig.Mounts[0].ReadOnly {
		t.Fatalf("explicit approved check cannot write its workspace: %v", err)
	}
}

func TestContainerPolicyRejectsUnsafeMountGraphAndLinks(t *testing.T) {
	config := fixtureConfig(t, t.TempDir())
	for name, mutate := range map[string]func(*Config){
		"role":        func(config *Config) { config.Role = "operator" },
		"control":     func(config *Config) { config.ControlDir = config.Worktree },
		"git":         func(config *Config) { config.GitDirectories = []string{config.Worktree} },
		"patch":       func(config *Config) { config.PatchPath = filepath.Join(config.Worktree, "result.patch") },
		"ceiling":     func(config *Config) { config.WritableRoots = append(config.WritableRoots, config.ControlDir) },
		"coordinator": func(config *Config) { config.Role = "coordinator" },
	} {
		t.Run(name, func(t *testing.T) {
			changed := config
			mutate(&changed)
			if changed.validate() == nil {
				t.Fatal("unsafe mount graph accepted")
			}
		})
	}
	root, err := os.OpenRoot(config.Worktree)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	if err := root.Rename(".git", "git-original"); err != nil {
		t.Fatal(err)
	}
	if err := root.Symlink("git-original", ".git"); err != nil {
		t.Fatal(err)
	}
	if _, err := (&Host{config: config}).request([]string{"true"}, config.Worktree, config.WritableRoots, "nonce", time.Now().Add(time.Minute)); err == nil {
		t.Fatal("symlinked Git pointer accepted")
	}
}

func TestContainerInspectionRejectsEveryWeakenedPolicy(t *testing.T) {
	_, request := fixtureRequest(t)
	name := "fg-goal-owned"
	for name, mutate := range map[string]func(*inspection){
		"valid":          func(_ *inspection) {},
		"network":        func(value *inspection) { value.HostConfig.NetworkMode = "host" },
		"pid":            func(value *inspection) { value.HostConfig.PidMode = "host" },
		"root":           func(value *inspection) { value.HostConfig.ReadonlyRootfs = false },
		"capabilities":   func(value *inspection) { value.HostConfig.CapAdd = append(value.HostConfig.CapAdd, "SYS_ADMIN") },
		"privileged":     func(value *inspection) { value.HostConfig.Privileged = true },
		"privilege-gain": func(value *inspection) { value.HostConfig.SecurityOpt = nil },
		"limits":         func(value *inspection) { value.HostConfig.PidsLimit = -1 },
		"rootfs":         func(value *inspection) { value.HostConfig.Binds = []string{"/:/host"} },
		"socket":         func(value *inspection) { value.Mounts[0].Source = "/var/run/docker.sock" },
		"metadata":       func(value *inspection) { value.Mounts[len(value.Mounts)-1].RW = true },
		"helper":         func(value *inspection) { value.Config.Entrypoint = []string{"/bin/sh"} },
		"argv":           func(value *inspection) { value.Config.Cmd = []string{"unapproved"} },
		"environment":    func(value *inspection) { value.Config.Env = append(value.Config.Env, "UNAPPROVED=value") },
		"stdin":          func(value *inspection) { value.Config.StdinOnce = false },
	} {
		t.Run(name, func(t *testing.T) {
			observed := inspectionFor(t, request, "fg-goal-owned")
			mutate(&observed)
			if err := verifyPolicy(observed, request); (err == nil) != (name == "valid") {
				t.Fatalf("unexpected policy verdict: %v", err)
			}
		})
	}
	observed := inspectionFor(t, request, name)
	if err := verifyIdentity(observed, request, name, observed.ID); err != nil {
		t.Fatal(err)
	}
	observed.Config.Labels["io.forgectl.goal-nonce"] = "another-operation"
	if verifyIdentity(observed, request, name, observed.ID) == nil {
		t.Fatal("another operation was accepted for cleanup")
	}
}

func testHost(t *testing.T, config Config, handler roundTrip) *Host {
	t.Helper()
	control, err := privdir.OpenChecked(privdir.Spec{Base: filepath.Dir(config.ControlDir), Leaf: filepath.Base(config.ControlDir), Mode: 0o700})
	if err != nil {
		t.Fatal(err)
	}
	host := &Host{config: config, api: &client{http: &http.Client{Transport: handler}, transport: &http.Transport{}}, control: control, gate: make(chan struct{}, 1)}
	host.gate <- struct{}{}
	t.Cleanup(func() { _ = host.Close(context.Background()) })
	return host
}

func TestContainerUncertainCreateNeverReplaysAndRecoversOnlyItsName(t *testing.T) {
	config := fixtureConfig(t, t.TempDir())
	var request createRequest
	var name string
	creates, deletes, starts := 0, 0, 0
	removed := false
	host := testHost(t, config, func(incoming *http.Request) (*http.Response, error) {
		switch {
		case incoming.Method == http.MethodPost && incoming.URL.Path == apiVersion+"/containers/create":
			creates++
			if err := json.NewDecoder(incoming.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			name = incoming.URL.Query().Get("name")
			return nil, errors.New("connection lost after accepted create")
		case incoming.Method == http.MethodGet:
			if removed {
				return responseJSON(404, nil), nil
			}
			if incoming.URL.Path != apiVersion+"/containers/"+name+"/json" {
				t.Fatalf("uncertain create looked up an unrelated name: %s", incoming.URL.Path)
			}
			return responseJSON(200, inspectionFor(t, request, name)), nil
		case incoming.Method == http.MethodDelete:
			deletes++
			if !strings.Contains(incoming.URL.Path, strings.Repeat("c", 64)) {
				t.Fatal("cleanup did not use full verified ID")
			}
			removed = true
			return responseJSON(204, nil), nil
		default:
			starts++
			return responseJSON(500, nil), nil
		}
	})
	_, err := host.Exec(t.Context(), []string{"true"}, config.Worktree, config.WritableRoots, time.Second)
	if err == nil || errors.Is(err, goalnative.ErrCleanupUnconfirmed) || creates != 1 || deletes != 1 || starts != 0 {
		t.Fatalf("create=%d delete=%d start=%d error=%v", creates, deletes, starts, err)
	}
	if _, err := host.Exec(t.Context(), []string{"true"}, config.Worktree, config.WritableRoots, time.Second); err == nil || creates != 1 {
		t.Fatal("uncertain execution was replayed")
	}
	if err := host.Close(t.Context()); err != nil {
		t.Fatalf("owned cleanup was actually confirmed: %v", err)
	}
}

func TestContainerCleanupNeverKillsMismatchedOrUninspectableIdentity(t *testing.T) {
	for _, mode := range []string{"label", "unavailable"} {
		t.Run(mode, func(t *testing.T) {
			config, request := fixtureRequest(t)
			mutations := 0
			host := testHost(t, config, func(incoming *http.Request) (*http.Response, error) {
				if incoming.Method != http.MethodGet {
					mutations++
					return responseJSON(500, nil), nil
				}
				if mode == "unavailable" {
					return responseJSON(503, nil), nil
				}
				observed := inspectionFor(t, request, "fg-goal-owned")
				observed.Config.Labels["io.forgectl.goal-nonce"] = "unrelated"
				observed.State.Running, observed.State.PID = true, 42
				return responseJSON(200, observed), nil
			})
			current := &operation{host: host, request: request, receipt: Receipt{Name: "fg-goal-owned", Nonce: "owned", ContainerID: strings.Repeat("c", 64)}}
			if err := current.cleanup(); !errors.Is(err, goalnative.ErrCleanupUnconfirmed) || mutations != 0 || current.receipt.Stopped {
				t.Fatalf("mutations=%d receipt=%+v error=%v", mutations, current.receipt, err)
			}
		})
	}
}

func TestContainerCleanupStopsOwnedExecutionWhenReceiptsCannotBeWritten(t *testing.T) {
	config, request := fixtureRequest(t)
	kills, deletes := 0, 0
	running := true
	host := testHost(t, config, func(incoming *http.Request) (*http.Response, error) {
		switch incoming.Method {
		case http.MethodGet:
			observed := inspectionFor(t, request, "fg-goal-owned")
			if running {
				observed.State.Running, observed.State.PID = true, 42
			}
			return responseJSON(200, observed), nil
		case http.MethodPost:
			if incoming.URL.Path != apiVersion+"/containers/"+strings.Repeat("c", 64)+"/kill" || incoming.URL.Query().Get("signal") != "KILL" {
				t.Fatalf("unexpected cleanup mutation: %s", incoming.URL)
			}
			kills++
			running = false
			return responseJSON(204, nil), nil
		case http.MethodDelete:
			deletes++
		}
		return responseJSON(500, nil), nil
	})
	if err := unix.Close(host.control); err != nil {
		t.Fatal(err)
	}
	host.control = -1
	current := &operation{host: host, request: request, receipt: Receipt{Name: "fg-goal-owned", Nonce: "owned", ContainerID: strings.Repeat("c", 64)}}
	if err := current.cleanup(); !errors.Is(err, goalnative.ErrCleanupUnconfirmed) || kills != 1 || deletes != 0 || running || !current.receipt.Stopped || current.receipt.Removed {
		t.Fatalf("kill=%d delete=%d running=%t receipt=%+v error=%v", kills, deletes, running, current.receipt, err)
	}
}

func TestContainerCanceledUncertainCleanupPoisonsFurtherExecution(t *testing.T) {
	for _, canceled := range []error{context.Canceled, context.DeadlineExceeded} {
		t.Run(canceled.Error(), func(t *testing.T) {
			config := fixtureConfig(t, t.TempDir())
			creates := 0
			host := testHost(t, config, func(incoming *http.Request) (*http.Response, error) {
				if incoming.Method == http.MethodPost && incoming.URL.Path == apiVersion+"/containers/create" {
					creates++
					return nil, canceled
				}
				return responseJSON(http.StatusServiceUnavailable, nil), nil
			})
			_, err := host.Exec(t.Context(), []string{"true"}, config.Worktree, config.WritableRoots, time.Second)
			if !errors.Is(err, canceled) || !errors.Is(err, goalnative.ErrCleanupUnconfirmed) || creates != 1 {
				t.Fatalf("uncertain cancellation creates=%d error=%v", creates, err)
			}
			_, err = host.Exec(t.Context(), []string{"true"}, config.Worktree, config.WritableRoots, time.Second)
			if err == nil || creates != 1 || !strings.Contains(err.Error(), "closed or unavailable") {
				t.Fatalf("uncertain canceled host admitted another operation: creates=%d error=%v", creates, err)
			}
			if err := host.Close(t.Context()); !errors.Is(err, goalnative.ErrCleanupUnconfirmed) {
				t.Fatalf("unconfirmed cleanup was lost at close: %v", err)
			}
		})
	}
}

func TestContainerCommandExitRequiresObservedTerminalState(t *testing.T) {
	for _, state := range []containerState{{Status: "exited"}, {Status: "exited", ExitCode: 17}} {
		if err := verifyCommandExit(state); err != nil {
			t.Fatalf("settled command exit refused: %+v %v", state, err)
		}
	}
	for _, state := range []containerState{{}, {Status: "created"}, {Status: "dead"}, {Status: "exited", Running: true}, {Status: "exited", PID: 42}, {Status: "exited", OOMKilled: true}, {Status: "exited", Error: "unknown"}, {Status: "exited", Paused: true}, {Status: "exited", Restarting: true}, {Status: "exited", Dead: true}} {
		if err := verifyCommandExit(state); err == nil {
			t.Errorf("unconfirmed command execution was accepted: %+v", state)
		}
	}
}
