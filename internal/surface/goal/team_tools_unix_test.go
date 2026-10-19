//go:build unix

package goal

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/surface/goalnative"
	"github.com/cameronsjo/forgectl/internal/surface/mail"
)

type recordingGoalExecutor struct {
	calls  []goalExecCall
	result goalnative.CommandResult
	err    error
}

type goalExecCall struct {
	argv  []string
	cwd   string
	roots []string
	limit time.Duration
}

type noOpGoalExecutor struct{}

func (noOpGoalExecutor) Exec(context.Context, []string, string, []string, time.Duration) (goalnative.CommandResult, error) {
	return goalnative.CommandResult{ExitCode: 0}, nil
}

func (executor *recordingGoalExecutor) Exec(_ context.Context, argv []string, cwd string, roots []string, limit time.Duration) (goalnative.CommandResult, error) {
	executor.calls = append(executor.calls, goalExecCall{argv: append([]string(nil), argv...), cwd: cwd, roots: append([]string(nil), roots...), limit: limit})
	return executor.result, executor.err
}

func callGoalTool(t testing.TB, handler func(context.Context, string, json.RawMessage) (any, error), operation string, arguments any) (any, error) {
	t.Helper()
	encoded, err := json.Marshal(arguments)
	if err != nil {
		t.Fatal(err)
	}
	return handler(context.Background(), operation, encoded)
}

func TestToolHandlerBindsExecutionToRoleCwdAndRoots(t *testing.T) {
	fixture := newGoalFixture(t, "src")
	roleDirectory := filepath.Join(fixture.state.RunDir, "agents", "implementation")
	if err := os.MkdirAll(roleDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	executor := &recordingGoalExecutor{result: goalnative.CommandResult{ExitCode: 0, Stdout: "ok"}}
	role := &nativeRole{Name: "implementation", Directory: roleDirectory, Executor: executor}
	role.Member.Worktree = fixture.state.Worktree
	engine := newGoalTestExecution(fixture)
	engine.self = "/private/bin/forgectl"
	handler := engine.toolHandler(role, []string{roleDirectory, fixture.state.Worktree})
	_, err := callGoalTool(t, handler, "goal_exec", map[string]any{
		"argv":            []string{"/bin/printf", "ok"},
		"timeout_seconds": 7,
		"cwd":             "/outside",
		"roots":           []string{"/outside"},
	})
	if err == nil || len(executor.calls) != 0 {
		t.Fatalf("goal_exec accepted caller-controlled cwd/roots: err=%v calls=%#v", err, executor.calls)
	}
	_, err = callGoalTool(t, handler, "goal_exec", map[string]any{
		"argv":            []string{"/bin/printf", "--cwd", "/outside", "--roots", "/outside"},
		"timeout_seconds": 7,
	})
	if err != nil {
		t.Fatalf("goal_exec: %v", err)
	}
	if len(executor.calls) != 1 {
		t.Fatalf("executor calls = %d, want one", len(executor.calls))
	}
	call := executor.calls[0]
	if call.cwd != fixture.state.Worktree {
		t.Fatalf("cwd = %q, want bound worktree %q", call.cwd, fixture.state.Worktree)
	}
	if !reflect.DeepEqual(call.roots, []string{roleDirectory, fixture.state.Worktree}) {
		t.Fatalf("roots = %#v, want role-bound roots", call.roots)
	}
	if call.limit != 7*time.Second {
		t.Fatalf("timeout = %s, want 7s", call.limit)
	}
}

func TestReadOnlyRoleToolsKeepTheirOwnExecutionCwd(t *testing.T) {
	fixture := newGoalFixture(t, "src")
	if err := os.MkdirAll(fixture.state.Worktree, 0o700); err != nil {
		t.Fatal(err)
	}
	writeGoalFile(t, filepath.Join(fixture.state.Worktree, "readable.txt"), "public source")
	for _, name := range []string{"coordinator", "reviewer"} {
		t.Run(name, func(t *testing.T) {
			directory := t.TempDir()
			executor := &recordingGoalExecutor{}
			role := &nativeRole{Name: name, Directory: directory, Member: mail.Worker{Worktree: directory}, Executor: executor}
			engine := newGoalTestExecution(fixture)
			engine.self = "/private/bin/forgectl"
			handler := engine.toolHandler(role, []string{directory})
			if _, err := callGoalTool(t, handler, "goal_exec", map[string]any{"argv": []string{"/usr/bin/true"}}); err != nil {
				t.Fatal(err)
			}
			if _, err := callGoalTool(t, handler, "goal_read", map[string]any{"path": "readable.txt"}); err != nil {
				t.Fatal(err)
			}
			if len(executor.calls) != 2 {
				t.Fatalf("expected two real executor calls, got %#v", executor.calls)
			}
			for _, call := range executor.calls {
				if call.cwd != directory || !reflect.DeepEqual(call.roots, []string{directory}) {
					t.Fatalf("read-only role tool inherited source write authority: %#v", call)
				}
			}
		})
	}
}

func TestNativeRoleClosedGuardIsSafeDuringToolShutdown(t *testing.T) {
	fixture := newGoalFixture(t, "src")
	engine := newGoalTestExecution(fixture)
	role := &nativeRole{Name: "implementation", Directory: t.TempDir(), Executor: noOpGoalExecutor{}}
	handler := engine.toolHandler(role, []string{role.Directory, fixture.state.Worktree})
	var wait sync.WaitGroup
	wait.Add(2)
	go func() {
		defer wait.Done()
		for range 100 {
			_, _ = callGoalTool(t, handler, "goal_exec", map[string]any{"argv": []string{"/bin/true"}})
		}
	}()
	go func() {
		defer wait.Done()
		role.markClosed()
	}()
	wait.Wait()
}

func TestToolHandlerRefusesCoordinatorAndReviewerWrites(t *testing.T) {
	fixture := newGoalFixture(t, "src")
	for _, name := range []string{"coordinator", "reviewer"} {
		t.Run(name, func(t *testing.T) {
			executor := &recordingGoalExecutor{}
			role := &nativeRole{Name: name, Directory: t.TempDir(), Executor: executor}
			engine := newGoalTestExecution(fixture)
			engine.self = "/private/bin/forgectl"
			handler := engine.toolHandler(role, []string{role.Directory})
			_, err := callGoalTool(t, handler, "goal_write", map[string]any{"path": "src/review.txt", "content": "must refuse"})
			if err == nil || !strings.Contains(err.Error(), "only the implementation role may write") {
				t.Fatalf("goal_write error = %v, want role refusal", err)
			}
			if len(executor.calls) != 0 {
				t.Fatal("refused role write reached the executor")
			}
		})
	}
}

func TestToolHandlerAllowsOnlyLiteralApprovedWritesAndRejectsDecoys(t *testing.T) {
	fixture := newGoalFixture(t, "src")
	src := filepath.Join(fixture.state.Worktree, "src")
	if err := os.MkdirAll(src, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.txt")
	writeGoalFile(t, outside, "outside")
	if err := os.Symlink(outside, filepath.Join(src, "link.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(outside, filepath.Join(src, "hard.txt")); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{".git/config", ".env.local", "src-old/new.txt", "../outside.txt", "src/link.txt", "src/hard.txt"} {
		t.Run(strings.ReplaceAll(path, "/", "_"), func(t *testing.T) {
			role := &nativeRole{Name: "implementation", Directory: t.TempDir(), Executor: &recordingGoalExecutor{}}
			engine := newGoalTestExecution(fixture)
			engine.self = "/private/bin/forgectl"
			handler := engine.toolHandler(role, []string{role.Directory, fixture.state.Worktree})
			_, err := callGoalTool(t, handler, "goal_write", map[string]any{"path": path, "content": "overwrite"})
			if err == nil {
				t.Fatalf("goal_write(%q) succeeded, want refusal", path)
			}
		})
	}
	if got, err := os.ReadFile(outside); err != nil || string(got) != "outside" { //nolint:gosec // Test-owned negative-control fixture.
		t.Fatalf("raw decoy target changed: %q, %v", got, err)
	}

	executor := &recordingGoalExecutor{result: goalnative.CommandResult{ExitCode: 0}}
	role := &nativeRole{Name: "implementation", Directory: t.TempDir(), Executor: executor}
	engine := newGoalTestExecution(fixture)
	engine.self = "/private/bin/forgectl"
	handler := engine.toolHandler(role, []string{role.Directory, fixture.state.Worktree})
	if _, err := callGoalTool(t, handler, "goal_write", map[string]any{"path": "src/new.txt", "content": "approved"}); err != nil {
		t.Fatalf("literal approved write: %v", err)
	}
	if len(executor.calls) != 1 || !reflect.DeepEqual(executor.calls[0].argv, []string{"/private/bin/forgectl", "surface", "project", "goal", "_file", "--root", fixture.state.Worktree, "--path", "src/new.txt", "--write", "--content", "approved"}) {
		t.Fatalf("approved write executor call = %#v", executor.calls)
	}
}

func TestToolHandlerReadsOnlyExactFrozenPatch(t *testing.T) {
	fixture := newGoalFixture(t, "src")
	patch := []byte("frozen patch\n")
	if err := fixture.store.WriteArtifact(fixture.state.ID, "result.patch", patch); err != nil {
		t.Fatal(err)
	}
	executor := &recordingGoalExecutor{result: goalnative.CommandResult{ExitCode: 0, Stdout: string(patch)}}
	role := &nativeRole{Name: "reviewer", Directory: t.TempDir(), Executor: executor}
	engine := newGoalTestExecution(fixture)
	engine.self = "/private/bin/forgectl"
	handler := engine.toolHandler(role, []string{role.Directory})
	path := filepath.Join(fixture.state.RunDir, "result.patch")
	result, err := callGoalTool(t, handler, "goal_read", map[string]any{"path": path})
	if err != nil {
		t.Fatalf("exact patch read: %v", err)
	}
	if got := result.(map[string]any)["content"]; got != string(patch) {
		t.Fatalf("patch content = %#v, want frozen bytes", got)
	}
	if len(executor.calls) != 1 || !reflect.DeepEqual(executor.calls[0].argv, []string{"/private/bin/forgectl", "surface", "project", "goal", "_file", "--root", fixture.state.RunDir, "--path", "result.patch"}) {
		t.Fatalf("patch read argv = %#v", executor.calls)
	}
	_, err = callGoalTool(t, handler, "goal_write", map[string]any{"path": path, "content": "tamper"})
	if err == nil {
		t.Fatal("goal_write result.patch succeeded")
	}
}

func TestSafeToolFileRejectsLinksAndHardLinksButAllowsRawRegularFile(t *testing.T) {
	root := t.TempDir()
	regular := filepath.Join(root, "regular")
	outside := filepath.Join(t.TempDir(), "outside")
	writeGoalFile(t, regular, "regular")
	writeGoalFile(t, outside, "outside")
	if err := os.Symlink(outside, filepath.Join(root, "symlink")); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(outside, filepath.Join(root, "hardlink")); err != nil {
		t.Fatal(err)
	}
	if err := safeToolFile(regular, false); err != nil {
		t.Fatalf("raw regular file was refused: %v", err)
	}
	for _, path := range []string{filepath.Join(root, "symlink"), filepath.Join(root, "hardlink")} {
		if err := safeToolFile(path, false); err == nil {
			t.Fatalf("safeToolFile(%q) accepted unsafe decoy", path)
		}
	}
	if err := safeToolFile(filepath.Join(root, "missing"), true); err != nil {
		t.Fatalf("new literal path was refused: %v", err)
	}
	if err := safeToolFile(filepath.Join(root, "missing"), false); err == nil {
		t.Fatal("missing file unexpectedly accepted without allowNew")
	}
}
