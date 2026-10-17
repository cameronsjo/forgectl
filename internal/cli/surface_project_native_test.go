//go:build unix

package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/surface"
	"github.com/cameronsjo/forgectl/internal/surface/backend"
	"github.com/cameronsjo/forgectl/internal/surface/mail"
	"github.com/cameronsjo/forgectl/internal/surface/project"
)

func prepareProjectNativeView(test *testing.T) (project.Store, projectReport, []string, *projectThreadViewFixture, *int) {
	test.Helper()
	store, created, member, _, fixture := prepareProjectThreadView(test)
	member.ThreadID = "12345678-1234-1234-1234-123456789abc"
	socketRoot, err := os.MkdirTemp("", "fc-view-")
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() {
		if err := os.RemoveAll(socketRoot); err != nil {
			test.Error(err)
		}
	})
	member.Socket = filepath.Join(socketRoot, "codex.sock")
	previousRun := member.RunID
	member.RunID = "native-view-run"
	registerProjectCLIMember(test, created.Ledger, member, true)
	bindProjectCLIThread(test, store.Root, created.Project.ID, created.Project.CoordinatorThreadID, member.Name, member.RunID, previousRun)
	binary := filepath.Join(test.TempDir(), "codex")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		test.Fatal(err)
	}
	previousPrepare, previousLaunch, previousInspect := projectPrepareCodexView, projectLaunchNative, projectInspectCodexView
	test.Cleanup(func() {
		projectPrepareCodexView, projectLaunchNative, projectInspectCodexView = previousPrepare, previousLaunch, previousInspect
	})
	projectInspectCodexView = func(_ context.Context, worker mail.Worker) (mail.CodexObservation, error) {
		return mail.CodexObservation{NativeSessionID: worker.ThreadID, Provider: "openai", Model: "test-model", CWD: worker.Worktree}, nil
	}
	preparations := new(int)
	projectPrepareCodexView = func(_ context.Context, worker mail.Worker) (mail.CodexRemoteView, error) {
		*preparations++
		if worker.RunID != member.RunID || worker.ThreadID != member.ThreadID || worker.Socket != member.Socket || worker.Worktree != member.Worktree {
			test.Fatalf("preparation targeted another binding: %#v", worker)
		}
		return mail.CodexRemoteView{NativeSessionID: member.ThreadID, Provider: "openai", Model: "test-model", CWD: member.Worktree, RuntimeWorkspaceRoots: []string{member.Worktree}, ApprovalPolicy: "never", ApprovalsReviewer: "user", Sandbox: mail.CodexRemoteSandbox{Type: "readOnly"}}, nil
	}
	projectLaunchNative = func(ctx context.Context, adapter backend.Capabilities, request surface.LaunchRequest) (backend.Ref, error) {
		invocation, ok := request.Invocation()
		if !ok || invocation.Harness != "codex" || invocation.CWD != member.Worktree {
			test.Fatal("native invocation lost its harness or working directory")
		}
		for _, entry := range invocation.Env {
			if strings.HasPrefix(entry, "HOME=") {
				home := strings.TrimPrefix(entry, "HOME=")
				test.Cleanup(func() { _ = os.RemoveAll(home) })
			}
		}
		tag, err := backend.NewRecoveryTag()
		if err != nil {
			test.Fatal(err)
		}
		bootstrap, err := backend.NewBootstrapCommand(exec.Opaque("test-bootstrap"))
		if err != nil {
			test.Fatal(err)
		}
		spec, err := backend.NewStartSpec(invocation.CWD, request.Name, tag, bootstrap)
		if err != nil {
			test.Fatal(err)
		}
		started := adapter.Start(ctx, spec)
		ref, _ := started.Ref()
		return ref, nil
	}
	fixture.start = func(ctx context.Context, spec backend.StartSpec) backend.StartResult {
		registry, err := store.Snapshot(ctx)
		if err != nil {
			test.Fatal(err)
		}
		operations := registry.Projects[0].SurfaceOperations
		pending := operations[len(operations)-1]
		if pending.Phase != project.SurfacePhasePending || pending.Tag != spec.Tag().String() || pending.Presentation != project.SurfacePresentationCodexRemote {
			test.Fatalf("native mutation preceded durable intent: %#v", pending)
		}
		return backend.NewRefKnown(projectPresentationTestRef(test, spec.Tag(), "w1:t2", "w1:p2", "terminal-2"))
	}
	flags := []string{"--view", "native", "--codex-binary", binary, "--expect-provider", "openai", "--expect-model", "test-model"}
	return store, created, flags, fixture, preparations
}

func TestSurfaceProjectNativeViewJournalsAndReopensWithoutResuming(test *testing.T) {
	store, created, flags, fixture, preparations := prepareProjectNativeView(test)
	opened, stderr, code := threadViewCommand(test, store, created, "open", flags...)
	if code != 0 || opened.Operation == nil || opened.Operation.Phase != project.SurfacePhaseVerified || opened.Operation.Presentation != project.SurfacePresentationCodexRemote || !opened.Focused || *preparations != 1 {
		test.Fatalf("native open exit=%d stderr=%s report=%#v", code, stderr, opened)
	}
	if opened.ProcessLiveness != project.ProcessLivenessUnknown || len(opened.Warnings) != 1 || !strings.Contains(opened.Warnings[0], "attachment and continued native policy are not established") {
		test.Fatalf("native execution was presented as confirmed attachment: %#v", opened)
	}
	reopened, stderr, code := threadViewCommand(test, store, created, "open", flags...)
	if code != 0 || reopened.Operation.ID != opened.Operation.ID || fixture.starts != 1 || *preparations != 1 {
		test.Fatalf("native reopen resumed or duplicated: exit=%d stderr=%s report=%#v", code, stderr, reopened)
	}
}

func TestSurfaceProjectNativeViewExpectationMismatchNeverCreates(test *testing.T) {
	store, created, flags, fixture, preparations := prepareProjectNativeView(test)
	flags[len(flags)-1] = "wrong-model"
	_, stderr, code := threadViewCommand(test, store, created, "open", flags...)
	if code != exitFailed || fixture.starts != 0 || *preparations != 0 || !strings.Contains(stderr, "expectation") {
		test.Fatalf("wrong model created a view: exit=%d stderr=%s starts=%d", code, stderr, fixture.starts)
	}
	registry, err := store.Snapshot(test.Context())
	if err != nil || len(registry.Projects[0].SurfaceOperations) != 1 {
		test.Fatalf("mismatch wrote a presentation operation: %v %#v", err, registry)
	}
}

func TestSurfaceProjectNativeViewExplicitCapability(test *testing.T) {
	store, created, _, fixture, preparations := prepareProjectNativeView(test)
	for _, flags := range [][]string{{"--view", "native"}, {"--view", "timeline", "--expect-model", "test-model"}} {
		if _, _, code := threadViewCommand(test, store, created, "open", flags...); code != exitUsage {
			test.Fatalf("invalid native options returned %d", code)
		}
	}
	if fixture.starts != 0 || *preparations != 0 {
		test.Fatal("invalid native options mutated state")
	}
	if err := (projectNativeViewOptions{}).validateBinding(project.Binding{Harness: mail.HarnessPi, NativeSessionID: "native", Socket: "/private/test.sock"}); err == nil {
		test.Fatal("Pi RPC enrollment was treated as a concurrent native TUI")
	}
}

func TestSurfaceProjectNativeFinishPreservesRollbackOutcome(test *testing.T) {
	tag, err := backend.NewRecoveryTag()
	if err != nil {
		test.Fatal(err)
	}
	ref := projectPresentationTestRef(test, tag, "w1:t2", "w1:p2", "terminal-2")
	for _, cleanup := range []backend.CleanupOutcome{backend.CleanupClosed, backend.CleanupAlreadyGone, backend.CleanupUnreadable, backend.CleanupIdentityMismatch} {
		test.Run(cleanup.String(), func(test *testing.T) {
			store, created, _, _, _ := prepareProjectThreadView(test)
			_, record, _, inspectError := store.Inspect(test.Context(), created.Project.ID)
			if inspectError != nil {
				test.Fatal(inspectError)
			}
			_, operation, beginError := store.BeginThread(test.Context(), record.ID, record.CoordinatorThreadID, "view-run", "projects-test", project.SurfacePresentationCodexRemote, tag, record.Workspace.Ref)
			if beginError != nil {
				test.Fatal(beginError)
			}
			adapter := &projectJournaledAdapter{tag: tag, result: backend.NewRefKnown(ref)}
			finish := projectNativeFinish(adapter, backend.Ref{}, &surface.LaunchError{Outcome: backend.RefKnown, Cleanup: cleanup})
			_, completed, finishError := store.FinishSurface(test.Context(), record.ID, operation.ID, finish)
			if finishError != nil {
				test.Fatal(finishError)
			}
			want := project.SurfacePhaseUnknown
			if cleanup == backend.CleanupClosed || cleanup == backend.CleanupAlreadyGone {
				want = project.SurfacePhaseFailed
			}
			if completed.Phase != want {
				test.Fatalf("cleanup %s recorded %s, want %s", cleanup, completed.Phase, want)
			}
		})
	}
	adapter := &projectJournaledAdapter{begin: func(context.Context, backend.StartSpec) (project.SurfaceOperation, error) {
		return project.SurfaceOperation{}, errors.New("conflict")
	}}
	if result := adapter.Start(test.Context(), backend.StartSpec{}); result.Outcome() != backend.NotMutated || result.Validate() != nil {
		test.Fatal("failed journal creation was not a definitive pre-mutation refusal")
	}
}
