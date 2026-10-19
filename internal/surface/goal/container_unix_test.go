//go:build unix

package goal

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/surface/goalnative"
	"github.com/cameronsjo/forgectl/internal/surface/mail"
)

func TestContainerSpecRequiresExplicitTeamAndChangesApproval(t *testing.T) {
	spec := goalSpec("/source")
	team := spec.EffectiveTeam()
	spec.Team = &team
	groupBytes, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	spec.Isolation = &IsolationSpec{Mode: "container", Socket: "/run/docker.sock", Image: "sha256:" + strings.Repeat("a", 64)}
	if err := spec.Validate(); err != nil {
		t.Fatal(err)
	}
	containerBytes, err := json.Marshal(spec)
	if err != nil || Digest(groupBytes) == Digest(containerBytes) {
		t.Fatal("stronger isolation did not change the approved digest")
	}
	for _, field := range []string{"socket", "image"} {
		changed := spec
		isolation := *spec.Isolation
		changed.Isolation = &isolation
		if field == "socket" {
			isolation.Socket = "/run/other.sock"
		} else {
			isolation.Image = "sha256:" + strings.Repeat("b", 64)
		}
		data, err := json.Marshal(changed)
		if err != nil || Digest(data) == Digest(containerBytes) {
			t.Fatalf("changed %s retained approval", field)
		}
	}
	spec.Team = nil
	requireErrorContains(t, spec.Validate(), "requires an explicit native team")
}

func TestContainerUnavailablePreparationNeverFallsBack(t *testing.T) {
	spec := goalSpec(newGoalRepository(t))
	team := spec.EffectiveTeam()
	spec.Team = &team
	spec.Isolation = &IsolationSpec{Mode: "container", Socket: "/run/forgectl-unavailable.sock", Image: "sha256:" + strings.Repeat("a", 64)}
	prepared, err := Prepare(t.Context(), spec)
	if err == nil || prepared.Spec.Repository != "" || !strings.Contains(err.Error(), "container isolation prerequisite") {
		t.Fatalf("unavailable isolation yielded preparation: %+v, %v", prepared, err)
	}
}

func TestContainerTeamPromptNamesLinuxAndSeparateCleanupScopes(t *testing.T) {
	fixture := newGoalFixture(t)
	engine := newNativeTeamTestExecution(fixture)
	engine.state.Spec.Isolation = &IsolationSpec{Mode: "container", Socket: "/run/docker.sock", Image: "sha256:" + strings.Repeat("a", 64)}
	contract := engine.contract()
	for _, wanted := range []string{"fresh leased Linux container", engine.state.Spec.Isolation.Image, "Use Linux binaries and semantics", "Native model processes retain", "goal_read/goal_write/goal_exec/goal_mail/goal_inbox"} {
		if !strings.Contains(contract, wanted) {
			t.Errorf("container prompt omitted %q", wanted)
		}
	}
}

type recordingFileExecutor struct {
	recordingGoalExecutor
	files []fileOperationCall
}

type fileOperationCall struct {
	root, path, content, cwd string
	write                    bool
	roots                    []string
}

func (executor *recordingFileExecutor) File(_ context.Context, root, path string, write bool, content, cwd string, roots []string) (goalnative.CommandResult, error) {
	executor.files = append(executor.files, fileOperationCall{root: root, path: path, write: write, content: content, cwd: cwd, roots: append([]string(nil), roots...)})
	return executor.result, executor.err
}

func TestToolHandlerUsesTypedFileOperationWithoutCommandFallback(t *testing.T) {
	fixture := newGoalFixture(t, ".")
	if err := os.MkdirAll(fixture.state.Worktree, 0o700); err != nil {
		t.Fatal(err)
	}
	writeGoalFile(t, filepath.Join(fixture.state.Worktree, "file.txt"), "existing")
	executor := &recordingFileExecutor{}
	executor.result.Stdout = "existing"
	directory := t.TempDir()
	role := &nativeRole{Name: "implementation", Directory: directory, Member: mail.Worker{Worktree: fixture.state.Worktree}, Executor: executor}
	engine := newGoalTestExecution(fixture)
	engine.self = "/host/macos/forgectl"
	roots := []string{directory, fixture.state.Worktree}
	handler := engine.toolHandler(role, roots)
	for _, operation := range []string{"goal_read", "goal_write"} {
		arguments := map[string]any{"path": "file.txt"}
		if operation == "goal_write" {
			arguments["content"] = "changed"
		}
		if _, err := callGoalTool(t, handler, operation, arguments); err != nil {
			t.Fatal(err)
		}
	}
	if len(executor.files) != 2 || len(executor.calls) != 0 {
		t.Fatalf("typed file calls=%+v host command calls=%+v", executor.files, executor.calls)
	}
	for index, call := range executor.files {
		if call.root != fixture.state.Worktree || call.path != "file.txt" || call.cwd != fixture.state.Worktree || !reflect.DeepEqual(call.roots, roots) || call.write != (index == 1) {
			t.Fatalf("file operation escaped bound arguments: %+v", call)
		}
	}
	if executor.files[1].content != "changed" {
		t.Fatal("typed write lost exact content")
	}
	executor.err = errors.New("container unavailable")
	if _, err := callGoalTool(t, handler, "goal_read", map[string]any{"path": "file.txt"}); err == nil || len(executor.calls) != 0 {
		t.Fatal("failed container file operation fell back to a host command")
	}
	executor.err = nil
	argv := []string{"/host/macos/forgectl", "not-a-file-helper"}
	if _, err := callGoalTool(t, handler, "goal_exec", map[string]any{"argv": argv}); err != nil || len(executor.calls) != 1 || !reflect.DeepEqual(executor.calls[0].argv, argv) {
		t.Fatalf("arbitrary command argv were rewritten: %+v %v", executor.calls, err)
	}
}

func TestContainerRealGoalBoundaryPreflight(t *testing.T) {
	if os.Getenv("FORGECTL_CONTAINER_INTEGRATION") != "1" {
		t.Skip("set FORGECTL_CONTAINER_INTEGRATION=1 with an approved image, socket and shared test root")
	}
	base, err := os.MkdirTemp(os.Getenv("FORGECTL_CONTAINER_TEST_ROOT"), "goal-boundary-proof-")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("retained container goal preflight: %s", base)
	repository := newGoalRepositoryAt(t, base)
	spec := goalSpec(repository)
	team := spec.EffectiveTeam()
	spec.Team = &team
	spec.Isolation = &IsolationSpec{Mode: "container", Socket: os.Getenv("FORGECTL_CONTAINER_SOCKET"), Image: os.Getenv("FORGECTL_CONTAINER_IMAGE")}
	prepared, err := Prepare(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	for _, warning := range []string{"leased Linux containers", "Native model processes retain", "no host-command fallback", spec.Isolation.Image, spec.Isolation.Socket} {
		if !containsString(prepared.Warnings, warning) {
			t.Errorf("container approval omitted %q", warning)
		}
	}
	store := Store{Root: filepath.Join(base, "registry")}
	state, err := store.Create(t.Context(), prepared)
	if err != nil {
		t.Fatal(err)
	}
	before, err := prepareWorkspace(t.Context(), store, state)
	if err != nil {
		t.Fatal(err)
	}
	engine := newGoalTestExecution(goalFixture{state: state, store: store})
	for _, name := range []string{"coordinator", "implementation", "reviewer"} {
		t.Run(name, func(t *testing.T) {
			directory := filepath.Join(state.RunDir, "agents", name)
			if err := os.MkdirAll(directory, 0o700); err != nil {
				t.Fatal(err)
			}
			cwd := directory
			roots := []string{directory}
			if name == "implementation" {
				cwd = state.Worktree
				roots = append(roots, state.Worktree)
			}
			ceiling := append([]string(nil), roots...)
			if name == "reviewer" {
				ceiling = append(ceiling, state.Worktree)
			}
			role := &nativeRole{Name: name, Directory: directory, Member: mail.Worker{Worktree: cwd}}
			host, err := engine.startExecutionHost(t.Context(), role, ceiling)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				if err := host.Close(ctx); err != nil {
					t.Errorf("preflight cleanup: %v", err)
				}
			}()
			role.Host, role.Executor = host, host
			if err := engine.probeBoundary(t.Context(), role, roots); err != nil {
				t.Fatal(err)
			}
		})
	}
	after, err := snapshotSource(t.Context(), repository)
	if err != nil || after != before {
		t.Fatalf("container preflight changed original source: before=%+v after=%+v error=%v", before, after, err)
	}
}
