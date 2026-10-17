//go:build unix

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/surface/backend"
	"github.com/cameronsjo/forgectl/internal/surface/mail"
	"github.com/cameronsjo/forgectl/internal/surface/project"
)

type projectThreadViewFixture struct {
	start  func(context.Context, backend.StartSpec) backend.StartResult
	close  func(context.Context, backend.Ref) backend.CloseResult
	starts int
	closes int
}

func (*projectThreadViewFixture) Kind() backend.Kind { return backend.KindHerdr }

func (fixture *projectThreadViewFixture) Start(ctx context.Context, spec backend.StartSpec) backend.StartResult {
	fixture.starts++
	return fixture.start(ctx, spec)
}

func (fixture *projectThreadViewFixture) Close(ctx context.Context, ref backend.Ref) backend.CloseResult {
	fixture.closes++
	return fixture.close(ctx, ref)
}

func (*projectThreadViewFixture) Probe(context.Context, backend.Ref) backend.ProbeResult {
	return backend.NewProbePresent()
}

func prepareProjectThreadView(test *testing.T) (project.Store, projectReport, mail.Worker, *projectPresentationFixture, *projectThreadViewFixture) {
	test.Helper()
	store := project.Store{Root: filepath.Join(test.TempDir(), "registry")}
	created := createCLIProject(test, store.Root, test.TempDir(), "Thread views")
	member := mail.Worker{Name: "lead", Harness: mail.HarnessCodex, Coordinator: true, Worktree: created.Project.Worktree, RunID: "view-run"}
	registerProjectCLIMember(test, created.Ledger, member, false)
	bindProjectCLIThread(test, store.Root, created.Project.ID, created.Project.CoordinatorThreadID, member.Name, member.RunID, "none")
	tag, err := backend.NewRecoveryTag()
	if err != nil {
		test.Fatal(err)
	}
	parent := projectPresentationTestRef(test, tag, "w1:t1", "w1:p1", "terminal-1")
	endpoint, _ := parent.Endpoint()
	_, operation, err := store.BeginWorkspace(test.Context(), created.Project.ID, "projects-test", tag, parent.Server(), endpoint)
	if err != nil {
		test.Fatal(err)
	}
	if _, _, err := store.FinishSurface(test.Context(), created.Project.ID, operation.ID, project.SurfaceVerified(parent)); err != nil {
		test.Fatal(err)
	}
	threadFixture := &projectThreadViewFixture{}
	presentation := &projectPresentationFixture{threadAdapter: threadFixture}
	installProjectPresentationFixture(test, presentation)
	return store, created, member, presentation, threadFixture
}

func threadViewCommand(test *testing.T, store project.Store, created projectReport, action string, flags ...string) (projectPresentationReport, string, int) {
	test.Helper()
	arguments := []string{"project", "--root", store.Root, "thread", "--project", created.Project.ID, action, created.Project.CoordinatorThreadID, "--session", "projects-test", "--herdr-binary", "/test/herdr", "--json"}
	arguments = append(arguments, flags...)
	output, stderr, code := runSurface(test, "", arguments...)
	var report projectPresentationReport
	if output != "" {
		if err := json.Unmarshal([]byte(output), &report); err != nil {
			test.Fatalf("invalid view output: %v: %s", err, output)
		}
	}
	return report, stderr, code
}

func TestSurfaceProjectThreadViewRoundTripAndSameRunReopen(test *testing.T) {
	store, created, member, presentation, fixture := prepareProjectThreadView(test)
	presentation.metadataError = errors.New("metadata unavailable")
	fixture.start = func(ctx context.Context, spec backend.StartSpec) backend.StartResult {
		registry, err := store.Snapshot(ctx)
		if err != nil {
			test.Fatal(err)
		}
		operations := registry.Projects[0].SurfaceOperations
		pending := operations[len(operations)-1]
		if pending.Phase != project.SurfacePhasePending || pending.Tag != spec.Tag().String() || pending.RunID != member.RunID || pending.Presentation != project.SurfacePresentationTimeline {
			test.Fatalf("view creation preceded exact durable intent: %#v", pending)
		}
		return backend.NewRefKnown(projectPresentationTestRef(test, spec.Tag(), "w1:t2", "w1:p2", "terminal-2"))
	}
	opened, stderr, code := threadViewCommand(test, store, created, "open", "--view", "timeline")
	if code != 0 || opened.Operation == nil || !opened.Focused || opened.ProcessLiveness != project.ProcessLivenessUnknown || len(presentation.threadNames) != 1 || presentation.threadNames[0] != created.Project.Threads[0].Name || len(opened.Warnings) != 1 || opened.Warnings[0] != projectMetadataWarning {
		test.Fatalf("open exit=%d stderr=%s report=%#v", code, stderr, opened)
	}
	if reopened, stderr, code := threadViewCommand(test, store, created, "open", "--view", "timeline"); code != 0 || fixture.starts != 1 || presentation.threadFocus != 2 || len(presentation.threadNames) != 2 || presentation.threadNames[1] != created.Project.Threads[0].Name || len(reopened.Warnings) != 1 || reopened.Warnings[0] != projectMetadataWarning || reopened.Operation.ID != opened.Operation.ID {
		test.Fatalf("reopen duplicated view: exit=%d stderr=%s report=%#v", code, stderr, reopened)
	}
	fixture.close = func(ctx context.Context, ref backend.Ref) backend.CloseResult {
		registry, err := store.Snapshot(ctx)
		if err != nil {
			test.Fatal(err)
		}
		pending := registry.Projects[0].SurfaceOperations[1]
		if pending.Retirement == nil || pending.Retirement.Phase != project.RetirementPending || pending.Ref.Tag() != ref.Tag() || ref.Tag() != opened.Operation.Ref.Tag() {
			test.Fatalf("close preceded retirement intent or targeted another view: %#v", pending)
		}
		return backend.NewCloseClosed()
	}
	closed, stderr, code := threadViewCommand(test, store, created, "close-view", "--operation", opened.Operation.ID)
	if code != 0 || fixture.closes != 1 || closed.Operation.Retirement.Phase != project.RetirementClosed || closed.Operation.Phase != project.SurfacePhaseVerified || closed.ProcessLiveness != project.ProcessLivenessUnknown {
		test.Fatalf("close discarded creation evidence or implied native stop: exit=%d stderr=%s report=%#v", code, stderr, closed)
	}
	if _, stderr, code := threadViewCommand(test, store, created, "focus"); code != exitFailed || !strings.Contains(stderr, "no verified tab") {
		test.Fatalf("retired view focused: exit=%d stderr=%s", code, stderr)
	}
	reopened, stderr, code := threadViewCommand(test, store, created, "open", "--view", "timeline")
	if code != 0 || fixture.starts != 2 || reopened.RunID != member.RunID || reopened.Operation.ID == opened.Operation.ID {
		test.Fatalf("same-run view recreation refused or reused operation: exit=%d stderr=%s report=%#v", code, stderr, reopened)
	}
}

func TestSurfaceProjectThreadViewUnknownCreationBlocksRetry(test *testing.T) {
	store, created, _, _, fixture := prepareProjectThreadView(test)
	fixture.start = func(_ context.Context, spec backend.StartSpec) backend.StartResult {
		return backend.NewOutcomeUnknown(spec.Tag(), backend.NewStartCause(backend.FailureMalformedResponse, errors.New("lost reply")))
	}
	first, stderr, code := threadViewCommand(test, store, created, "open", "--view", "timeline")
	if code != exitFailed || first.Operation == nil || first.Operation.Phase != project.SurfacePhaseUnknown || !strings.Contains(stderr, "unknown") {
		test.Fatalf("ambiguity lost: exit=%d stderr=%s report=%#v", code, stderr, first)
	}
	if _, _, code := threadViewCommand(test, store, created, "open", "--view", "timeline"); code != exitFailed || fixture.starts != 1 {
		test.Fatalf("unknown create retried: exit=%d creates=%d", code, fixture.starts)
	}
}

func TestSurfaceProjectThreadViewUnknownCloseBlocksMutationAndHistoricalCloseIsAllowed(test *testing.T) {
	store, created, member, presentation, fixture := prepareProjectThreadView(test)
	fixture.start = func(_ context.Context, spec backend.StartSpec) backend.StartResult {
		return backend.NewRefKnown(projectPresentationTestRef(test, spec.Tag(), "w1:t2", "w1:p2", "terminal-2"))
	}
	opened, stderr, code := threadViewCommand(test, store, created, "open", "--view", "timeline")
	if code != 0 {
		test.Fatalf("open exit=%d stderr=%s", code, stderr)
	}
	member.RunID = "replacement-run"
	registerProjectCLIMember(test, created.Ledger, member, true)
	if _, _, code := threadViewCommand(test, store, created, "open", "--view", "timeline"); code != exitFailed || fixture.starts != 1 {
		test.Fatal("stale binding opened a view")
	}
	fixture.close = func(context.Context, backend.Ref) backend.CloseResult {
		return backend.NewCloseUnreadable(backend.NewStartCause(backend.FailureUnavailable, errors.New("server unavailable")))
	}
	closed, stderr, code := threadViewCommand(test, store, created, "close-view", "--operation", opened.Operation.ID)
	if code != exitFailed || fixture.closes != 1 || closed.Operation == nil || closed.Operation.Retirement.Phase != project.RetirementUnknown || !strings.Contains(stderr, "unknown") {
		test.Fatalf("historical close not attempted or ambiguity lost: exit=%d stderr=%s report=%#v", code, stderr, closed)
	}
	if _, _, code := threadViewCommand(test, store, created, "close-view", "--operation", opened.Operation.ID); code != exitFailed || fixture.closes != 1 {
		test.Fatal("ambiguous close automatically retried")
	}
	bindProjectCLIThread(test, store.Root, created.Project.ID, created.Project.CoordinatorThreadID, member.Name, member.RunID, "view-run")
	if _, _, code := threadViewCommand(test, store, created, "open", "--view", "timeline"); code != exitFailed || fixture.starts != 1 || presentation.threadFocus != 1 {
		test.Fatal("replacement bypassed unresolved old view")
	}
}

func TestSurfaceProjectThreadViewExplicitFlagsAndTarget(test *testing.T) {
	isolateJSONContractEnv(test)
	store, created, _, _, fixture := prepareProjectThreadView(test)
	for _, flags := range [][]string{
		{"open", created.Project.CoordinatorThreadID},
		{"open", created.Project.CoordinatorThreadID, "--view", "automatic"},
		{"close-view", created.Project.CoordinatorThreadID},
	} {
		arguments := []string{"surface", "project", "--root", store.Root, "thread", "--project", created.Project.ID}
		arguments = append(arguments, flags...)
		arguments = append(arguments, "--session", "projects-test", "--herdr-binary", "/test/herdr", "--json")
		output, stderr, err := runJSONThroughFang(test, productionJSONRoot(&exec.FakeRunner{}), arguments...)
		assertProjectJSONError(test, output, stderr, err, jsonCodeUsage, exitUsage)
	}
	if _, _, code := threadViewCommand(test, store, created, "close-view", "--operation", "wrong-operation"); code != exitFailed || fixture.closes != 0 || fixture.starts != 0 {
		test.Fatal("wrong operation or missing flags reached mutation")
	}
}

func TestSurfaceProjectThreadTimelineBootstrapSelectsExactThread(test *testing.T) {
	bootstrap, err := projectThreadTimelineBootstrap("/tmp/forgectl", "/tmp/project root", "project-id", "thread-id")
	if err != nil {
		test.Fatal(err)
	}
	expected := `/tmp/forgectl surface project '--root' '/tmp/project root' timeline project-id '--live' '--thread' thread-id`
	if !bootstrap.SensitiveArg().Equal(exec.Opaque(expected)) {
		test.Fatal("thread bootstrap did not select the exact logical thread")
	}
}

func TestSurfaceProjectThreadViewTextSeparatesRetirementFromNativeProcess(test *testing.T) {
	var output strings.Builder
	report := projectPresentationReport{
		ProjectID: "project-id", Action: "close-view", ProcessLiveness: project.ProcessLivenessUnknown,
		Operation: &project.SurfaceOperation{Phase: project.SurfacePhaseVerified, Retirement: &project.SurfaceRetirement{Phase: project.RetirementClosed}},
	}
	if err := writeProjectPresentation(&output, report, false); err != nil {
		test.Fatal(err)
	}
	if !strings.Contains(output.String(), "view retirement=closed") || !strings.Contains(output.String(), "process liveness remains unknown") {
		test.Fatalf("close-view text overclaims native process state: %s", output.String())
	}
}
