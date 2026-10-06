//go:build unix

package cli

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/launch"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/surface"
	"github.com/cameronsjo/forgectl/internal/surface/backend"
	"github.com/cameronsjo/forgectl/internal/surface/worker"
)

const testRepoTop = "/repo/coordinated"

// testWorkerLedger opens a real ledger under a temp state dir, so the row a
// test reads back is the row a later `surface list` would read.
func testWorkerLedger(t *testing.T) *worker.Ledger {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	led, err := worker.Open(testRepoTop, "default")
	if err != nil {
		t.Fatalf("worker.Open: %v", err)
	}
	return led
}

func onlyRow(t *testing.T, led *worker.Ledger) worker.Row {
	t.Helper()
	rows, err := led.Rows()
	if err != nil {
		t.Fatalf("Rows: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %+v, want exactly one", rows)
	}
	return rows[0]
}

func testHerdrRef(t *testing.T) backend.Ref {
	t.Helper()
	server, err := backend.Fingerprint(backend.IncarnationInput{
		Endpoint: "/private/tmp/fc/herdr.sock", Version: "herdr/20", Device: 1, Inode: 4242,
	})
	if err != nil {
		t.Fatal(err)
	}
	tag, err := backend.NewRecoveryTag()
	if err != nil {
		t.Fatal(err)
	}
	id, err := backend.NewHerdrIdentity("w9", "w9:t1", "w9:p1")
	if err != nil {
		t.Fatal(err)
	}
	ref, err := backend.NewHerdrRef(backend.HerdrDefaultSessionServer(), server, tag, id)
	if err != nil {
		t.Fatal(err)
	}
	return ref
}

func goodSteps(t *testing.T) workerSteps {
	return workerSteps{
		addWorktree: func(context.Context) (worker.Worktree, error) {
			return worker.Worktree{Path: testRepoTop + "/.claude/worktrees/w1", Branch: "feat/w1", Base: "abc123"}, nil
		},
		build: func(cwd string) (launch.BuiltInvocation, error) {
			return launch.BuiltInvocation{Invocation: launch.Invocation{Harness: "codex", CWD: cwd}, Worker: true}, nil
		},
		launch: func(context.Context, launch.Invocation) (backend.Ref, error) { return testHerdrRef(t), nil },
		now:    func() time.Time { return time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC) },
	}
}

func TestWorkerLaunchRecordsEveryStep(t *testing.T) {
	led := testWorkerLedger(t)
	got, err := runWorkerSteps(context.Background(), led, "w1", "feat/w1", goodSteps(t))
	if err != nil {
		t.Fatalf("runWorkerSteps: %v", err)
	}
	row := onlyRow(t, led)
	if row.Stage != worker.StageLaunched || row.Harness != "codex" || row.Base != "abc123" || row.Worktree != got.worktree {
		t.Fatalf("row = %+v", row)
	}
	decoded, err := backend.DecodeRef(row.Ref)
	if err != nil {
		t.Fatalf("the stored ref does not decode: %v", err)
	}
	if decoded.Tag() != got.ref.Tag() {
		t.Error("the stored ref names a different workspace than the launch returned")
	}
}

// TestWorkerLaunchFailureAfterWorktreeLeavesARow is the plan's partial-launch
// negative control: a failure injected after `git worktree add` leaves a row
// that names the worktree, so `list --orphans` (T4) can find it.
func TestWorkerLaunchFailureAfterWorktreeLeavesARow(t *testing.T) {
	for name, mutate := range map[string]func(*workerSteps){
		"build fails": func(s *workerSteps) {
			s.build = func(string) (launch.BuiltInvocation, error) {
				return launch.BuiltInvocation{}, launch.ErrWorkerPosture
			}
		},
		"launch fails": func(s *workerSteps) {
			s.launch = func(context.Context, launch.Invocation) (backend.Ref, error) {
				return backend.Ref{}, errors.New("herdr went away")
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			led := testWorkerLedger(t)
			steps := goodSteps(t)
			mutate(&steps)

			if _, err := runWorkerSteps(context.Background(), led, "w1", "feat/w1", steps); err == nil {
				t.Fatal("the injected failure was not reported")
			}
			row := onlyRow(t, led)
			if row.Stage != worker.StageFailed {
				t.Errorf("stage = %q, want failed", row.Stage)
			}
			if row.Worktree == "" {
				t.Error("the failed row lost the worktree it created")
			}
			if row.Failure == "" {
				t.Error("the failed row does not say what failed")
			}
		})
	}
}

// TestWorkerLaunchKeepsTheRecoveryTag: a launch whose rollback could not close
// the workspace hands back a tag; the row keeps it so the workspace can be
// found later.
func TestWorkerLaunchKeepsTheRecoveryTag(t *testing.T) {
	led := testWorkerLedger(t)
	steps := goodSteps(t)
	steps.launch = func(context.Context, launch.Invocation) (backend.Ref, error) {
		return backend.Ref{}, &surface.LaunchError{Recovery: "0123456789abcdef"}
	}
	if _, err := runWorkerSteps(context.Background(), led, "w1", "feat/w1", steps); err == nil {
		t.Fatal("no error")
	}
	if got := onlyRow(t, led).Recovery; got != "0123456789abcdef" {
		t.Errorf("recovery = %q, want the launch error's tag", got)
	}
}

// TestWorkerLaunchFailureBeforeWorktreeStillLeavesARow: the pending row is
// written before git runs, so even a refused worktree is on record.
func TestWorkerLaunchFailureBeforeWorktreeStillLeavesARow(t *testing.T) {
	led := testWorkerLedger(t)
	steps := goodSteps(t)
	steps.addWorktree = func(context.Context) (worker.Worktree, error) {
		return worker.Worktree{}, worker.ErrUnsafeWorktreeRoot
	}
	if _, err := runWorkerSteps(context.Background(), led, "w1", "feat/w1", steps); !errors.Is(err, worker.ErrUnsafeWorktreeRoot) {
		t.Fatalf("err = %v", err)
	}
	if row := onlyRow(t, led); row.Stage != worker.StageFailed || row.Worktree != "" {
		t.Errorf("row = %+v, want failed with no worktree", row)
	}
}

func TestWorkerLaunchRefusesBeforeTouchingAnything(t *testing.T) {
	for name, opts := range map[string]surfaceLaunchOptions{
		"not herdr":  {Backend: "tmux", Worktree: "feat/x", DisplayName: "w1"},
		"no name":    {Backend: "herdr", Worktree: "feat/x"},
		"path name":  {Backend: "herdr", Worktree: "feat/x", DisplayName: "../w1"},
		"upper name": {Backend: "herdr", Worktree: "feat/x", DisplayName: "W1"},
	} {
		t.Run(name, func(t *testing.T) {
			cmd := &cobra.Command{}
			cmd.SetContext(context.Background())
			err := runWorkerLaunch(cmd, module.Deps{}, opts)
			if code := ExitCode(err); code != 2 {
				t.Fatalf("exit code = %d (err %v), want 2", code, err)
			}
		})
	}
}

// TestWorkerLaunchRecordsTheSession pins the atelier join key: a claude
// worker's session id and transcript path land in its row.
func TestWorkerLaunchRecordsTheSession(t *testing.T) {
	led := testWorkerLedger(t)
	steps := goodSteps(t)
	const id = "0f8e2c1a-3b4d-4e5f-8a6b-7c8d9e0f1a2b"
	steps.build = func(cwd string) (launch.BuiltInvocation, error) {
		return launch.BuiltInvocation{
			Invocation: launch.Invocation{Harness: "claude", CWD: cwd, Env: []string{"HOME=/h"}},
			SessionID:  id,
			Worker:     true,
		}, nil
	}
	if _, err := runWorkerSteps(context.Background(), led, "w1", "feat/w1", steps); err != nil {
		t.Fatalf("runWorkerSteps: %v", err)
	}
	row := onlyRow(t, led)
	if row.SessionID != id || row.Transcript != worker.TranscriptPath([]string{"HOME=/h"}, testRepoTop+"/.claude/worktrees/w1", id) || row.Transcript == "" {
		t.Fatalf("row = %+v", row)
	}
}

// TestBuildWorkerInvocationIsolates pins the call site that turns the worker
// floor on: from surfaceInvocationRequest, as runWorkerLaunch builds it, a
// worker gets the isolation argv, a session id, and none of the launcher's
// handles. Dropping req.Worker, or the call through it, turns this red.
func TestBuildWorkerInvocationIsolates(t *testing.T) {
	t.Setenv("CLAUDE_CODE_MESSAGING_TOKEN", "coordinator-token")
	t.Setenv("HERDR_SOCKET_PATH", "/tmp/herdr.sock")
	cwd, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.LaunchConfig{Defaults: config.LaunchDefaults{PermissionMode: "acceptEdits"}}
	req := surfaceInvocationRequest(cfg, cwd, nil, nil, "claude")
	req.Resolve = func(string, config.LaunchDefaults) (launch.ResolvedBinary, error) {
		return launch.ResolvedBinary{Path: "/stub/claude", Source: launch.BinaryPATH}, nil
	}
	const id = "0f8e2c1a-3b4d-4e5f-8a6b-7c8d9e0f1a2b"
	built, err := buildWorkerInvocation(req, "Fix it.", func() (string, error) { return id, nil }, io.Discard)
	if err != nil {
		t.Fatalf("buildWorkerInvocation: %v", err)
	}
	if !slices.Contains(built.Invocation.Args, "--setting-sources") || !slices.Contains(built.Invocation.Args, "--safe-mode") || built.SessionID != id {
		t.Fatalf("argv %q, session %q: not a worker build", built.Invocation.Args, built.SessionID)
	}
	for _, e := range built.Invocation.Env {
		if strings.HasPrefix(e, "CLAUDE_CODE_MESSAGING_TOKEN=") || strings.HasPrefix(e, "HERDR_SOCKET_PATH=") {
			t.Fatalf("worker env kept the launcher's handle %s", e)
		}
	}
}

// TestWorkerLaunchRefusesANonWorkerBuild closes the bypass where the launch
// path builds the invocation without marking it a worker: no posture floor,
// no isolation argv, the launcher's whole environment. runWorkerSteps refuses
// to start it, and records the failure.
func TestWorkerLaunchRefusesANonWorkerBuild(t *testing.T) {
	led := testWorkerLedger(t)
	steps := goodSteps(t)
	launched := false
	steps.build = func(cwd string) (launch.BuiltInvocation, error) {
		return launch.BuiltInvocation{Invocation: launch.Invocation{Harness: "claude", CWD: cwd}}, nil
	}
	steps.launch = func(context.Context, launch.Invocation) (backend.Ref, error) {
		launched = true
		return testHerdrRef(t), nil
	}
	if _, err := runWorkerSteps(context.Background(), led, "w1", "feat/w1", steps); err == nil {
		t.Fatal("a non-worker build was accepted")
	}
	if launched {
		t.Fatal("a non-worker build was launched")
	}
	if row := onlyRow(t, led); row.Stage != worker.StageFailed {
		t.Fatalf("stage = %q, want failed", row.Stage)
	}
}
