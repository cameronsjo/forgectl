//go:build unix

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/surface/backend"
	"github.com/cameronsjo/forgectl/internal/surface/mail"
	"github.com/cameronsjo/forgectl/internal/surface/project"
)

type projectPresentationFixture struct {
	start          func(context.Context, backend.StartSpec, func(backend.ServerID, backend.EndpointID) error) backend.StartResult
	starts         int
	workspaceFocus int
	threadFocus    int
	focusError     error
	lastParent     backend.Ref
	lastChild      backend.Ref
}

func (fixture *projectPresentationFixture) StartProjectWorkspace(ctx context.Context, spec backend.StartSpec, before func(backend.ServerID, backend.EndpointID) error) backend.StartResult {
	fixture.starts++
	return fixture.start(ctx, spec, before)
}

func (fixture *projectPresentationFixture) FocusProjectWorkspace(_ context.Context, ref backend.Ref) error {
	fixture.workspaceFocus++
	fixture.lastParent = ref
	return fixture.focusError
}

func (fixture *projectPresentationFixture) FocusThread(_ context.Context, parent, child backend.Ref) error {
	fixture.threadFocus++
	fixture.lastParent, fixture.lastChild = parent, child
	return fixture.focusError
}

func installProjectPresentationFixture(test *testing.T, fixture *projectPresentationFixture) {
	test.Helper()
	previous := projectPresentationFor
	projectPresentationFor = func(_ module.Deps, binary, session string, _ io.Writer) (projectPresentation, error) {
		if binary != "/test/herdr" || session != "projects-test" {
			return nil, errors.New("unexpected test presentation target")
		}
		return fixture, nil
	}
	test.Cleanup(func() { projectPresentationFor = previous })
}

func projectPresentationTestRef(test *testing.T, tag backend.RecoveryTag, tab, pane, terminal string) backend.Ref {
	test.Helper()
	server, err := backend.Fingerprint(backend.IncarnationInput{Endpoint: "/test/herdr.sock", Version: "test-version", Inode: 10})
	if err != nil {
		test.Fatal(err)
	}
	identity, err := backend.NewHerdrIdentity("w1", tab, pane)
	if err != nil {
		test.Fatal(err)
	}
	identity, err = identity.WithTerminal(terminal)
	if err != nil {
		test.Fatal(err)
	}
	ref, err := backend.NewHerdrRef(backend.HerdrNamedSessionServer(), server, tag, identity)
	if err != nil {
		test.Fatal(err)
	}
	ref, err = ref.WithEndpoint(backend.EndpointOf("/test/herdr.sock"))
	if err != nil {
		test.Fatal(err)
	}
	return ref
}

func openCLIProject(test *testing.T, root, projectID string) (projectPresentationReport, string, int) {
	test.Helper()
	output, stderr, code := runSurface(test, "", "project", "--root", root, "open", projectID, "--session", "projects-test", "--herdr-binary", "/test/herdr", "--json")
	var report projectPresentationReport
	if output != "" {
		if err := json.Unmarshal([]byte(output), &report); err != nil {
			test.Fatalf("invalid presentation report: %v: %s", err, output)
		}
	}
	return report, stderr, code
}

func TestSurfaceProjectOpenPersistsBeforeCreateAndReopensExactWorkspace(test *testing.T) {
	root := filepath.Join(test.TempDir(), "registry")
	created := createCLIProject(test, root, test.TempDir(), "A display name, not an argument")
	store := project.Store{Root: root}
	fixture := &projectPresentationFixture{}
	fixture.start = func(ctx context.Context, spec backend.StartSpec, before func(backend.ServerID, backend.EndpointID) error) backend.StartResult {
		ref := projectPresentationTestRef(test, spec.Tag(), "w1:t1", "w1:p1", "terminal-1")
		endpoint, _ := ref.Endpoint()
		if err := before(ref.Server(), endpoint); err != nil {
			test.Fatal(err)
		}
		_, pending, _, err := store.Inspect(ctx, created.Project.ID)
		if err != nil || pending.Workspace != nil || len(pending.SurfaceOperations) != 1 || pending.SurfaceOperations[0].Phase != project.SurfacePhasePending || pending.SurfaceOperations[0].Tag != spec.Tag().String() {
			test.Fatalf("workspace started before durable pending identity: %#v %v", pending, err)
		}
		return backend.NewRefKnown(ref)
	}
	installProjectPresentationFixture(test, fixture)
	report, stderr, code := openCLIProject(test, root, created.Project.ID)
	if code != 0 || !report.Focused || fixture.workspaceFocus != 1 || report.Workspace == nil || report.Operation == nil || report.Operation.Phase != project.SurfacePhaseVerified || report.ProcessLiveness != project.ProcessLivenessUnknown {
		test.Fatalf("open exit=%d stderr=%s report=%#v", code, stderr, report)
	}
	revision := report.Revision
	refJSON, err := report.Workspace.Ref.MarshalJSON()
	if err != nil {
		test.Fatal(err)
	}
	report, stderr, code = openCLIProject(test, root, created.Project.ID)
	focusedJSON, err := fixture.lastParent.MarshalJSON()
	if err != nil || code != 0 || !report.Focused || fixture.starts != 1 || fixture.workspaceFocus != 2 || report.Action != "focused" || report.Revision != revision || string(refJSON) != string(focusedJSON) {
		test.Fatalf("reopen duplicated or changed workspace: exit=%d stderr=%s report=%#v fixture=%#v err=%v", code, stderr, report, fixture, err)
	}
	fixture.focusError = errors.New("server incarnation changed")
	if _, _, code := openCLIProject(test, root, created.Project.ID); code != exitFailed || fixture.starts != 1 {
		test.Fatalf("stale workspace was recreated: exit=%d starts=%d", code, fixture.starts)
	}
}

func TestSurfaceProjectOpenAmbiguityRemainsUnknownAndBlocksDuplicate(test *testing.T) {
	root := filepath.Join(test.TempDir(), "registry")
	created := createCLIProject(test, root, test.TempDir(), "Ambiguous create")
	mutations := 0
	fixture := &projectPresentationFixture{}
	fixture.start = func(_ context.Context, spec backend.StartSpec, before func(backend.ServerID, backend.EndpointID) error) backend.StartResult {
		ref := projectPresentationTestRef(test, spec.Tag(), "w1:t1", "w1:p1", "terminal-1")
		endpoint, _ := ref.Endpoint()
		if err := before(ref.Server(), endpoint); err != nil {
			return backend.NewNotMutated(backend.NewStartCause(backend.FailureUnavailable, err))
		}
		mutations++
		return backend.NewOutcomeUnknown(spec.Tag(), backend.NewStartCause(backend.FailureMalformedResponse, errors.New("lost acknowledgement")))
	}
	installProjectPresentationFixture(test, fixture)
	report, stderr, code := openCLIProject(test, root, created.Project.ID)
	if code != exitFailed || report.Operation == nil || report.Operation.Phase != project.SurfacePhaseUnknown || report.Workspace != nil || !strings.Contains(stderr, "unknown") {
		test.Fatalf("unknown result lost: exit=%d stderr=%s report=%#v", code, stderr, report)
	}
	if _, stderr, code := openCLIProject(test, root, created.Project.ID); code != exitFailed || mutations != 1 || !strings.Contains(stderr, "active workspace operation") {
		test.Fatalf("ambiguous workspace duplicated: exit=%d stderr=%s mutations=%d", code, stderr, mutations)
	}
}

func TestSurfaceProjectFocusChecksBindingAndNeverCreates(test *testing.T) {
	root := filepath.Join(test.TempDir(), "registry")
	created := createCLIProject(test, root, test.TempDir(), "Exact focus")
	store := project.Store{Root: root}
	member := mail.Worker{Name: "lead", Harness: mail.HarnessCodex, Coordinator: true, Worktree: created.Project.Worktree, RunID: "run-lead"}
	registerProjectCLIMember(test, created.Ledger, member, false)
	bindProjectCLIThread(test, root, created.Project.ID, created.Project.CoordinatorThreadID, member.Name, member.RunID, "none")
	parentTag, err := backend.NewRecoveryTag()
	if err != nil {
		test.Fatal(err)
	}
	parent := projectPresentationTestRef(test, parentTag, "w1:t1", "w1:p1", "terminal-1")
	endpoint, _ := parent.Endpoint()
	_, operation, err := store.BeginWorkspace(test.Context(), created.Project.ID, "projects-test", parentTag, parent.Server(), endpoint)
	if err != nil {
		test.Fatal(err)
	}
	if _, _, err := store.FinishSurface(test.Context(), created.Project.ID, operation.ID, project.SurfaceVerified(parent)); err != nil {
		test.Fatal(err)
	}
	childTag, err := backend.NewRecoveryTag()
	if err != nil {
		test.Fatal(err)
	}
	child := projectPresentationTestRef(test, childTag, "w1:t2", "w1:p2", "terminal-2")
	_, operation, err = store.BeginThread(test.Context(), created.Project.ID, created.Project.CoordinatorThreadID, member.RunID, "projects-test", childTag, parent)
	if err != nil {
		test.Fatal(err)
	}
	if _, _, err := store.FinishSurface(test.Context(), created.Project.ID, operation.ID, project.SurfaceVerified(child)); err != nil {
		test.Fatal(err)
	}
	fixture := &projectPresentationFixture{}
	installProjectPresentationFixture(test, fixture)
	arguments := []string{"project", "--root", root, "thread", "--project", created.Project.ID, "focus", created.Project.CoordinatorThreadID, "--session", "projects-test", "--herdr-binary", "/test/herdr", "--json"}
	output, stderr, code := runSurface(test, "", arguments...)
	if code != 0 || fixture.threadFocus != 1 || fixture.starts != 0 {
		test.Fatalf("focus exit=%d stdout=%s stderr=%s fixture=%#v", code, output, stderr, fixture)
	}
	member.RunID = "run-replacement"
	registerProjectCLIMember(test, created.Ledger, member, true)
	if output, stderr, code := runSurface(test, "", arguments...); code != exitFailed || fixture.threadFocus != 1 || output != "" || !strings.Contains(stderr, "stale") {
		test.Fatalf("stale focus reached adapter: exit=%d stdout=%s stderr=%s fixture=%#v", code, output, stderr, fixture)
	}
}

func TestSurfaceProjectPresentationRequiresExplicitTarget(test *testing.T) {
	isolateJSONContractEnv(test)
	root := filepath.Join(test.TempDir(), "registry")
	created := createCLIProject(test, root, test.TempDir(), "Explicit only")
	for _, flags := range [][]string{
		{},
		{"--session", "projects-test"},
		{"--herdr-binary", "/test/herdr"},
		{"--session", "bad/session", "--herdr-binary", "/test/herdr"},
		{"--session", "projects-test", "--herdr-binary", "relative"},
	} {
		arguments := append([]string{"surface", "project", "--root", root, "open", created.Project.ID, "--json"}, flags...)
		output, stderr, err := runJSONThroughFang(test, productionJSONRoot(&exec.FakeRunner{}), arguments...)
		assertProjectJSONError(test, output, stderr, err, jsonCodeUsage, exitUsage)
	}
	if _, err := newProjectPresentation(module.Deps{}, "/test/herdr", "projects-test", io.Discard); err == nil {
		test.Fatal("presentation silently substituted an ordinary command runner")
	}
	if _, err := os.Stat(filepath.Join(root, created.Project.ID)); !os.IsNotExist(err) {
		test.Fatalf("invalid open created a team: %v", err)
	}
}

func TestSurfaceProjectTimelineBootstrapIsFixedAndQuoted(test *testing.T) {
	bootstrap, err := projectTimelineBootstrap("/tmp/forgectl's build", "/tmp/projects with spaces", "project-id")
	if err != nil {
		test.Fatal(err)
	}
	expected := `'/tmp/forgectl'\''s build' surface project '--root' '/tmp/projects with spaces' timeline project-id '--live'`
	if !bootstrap.SensitiveArg().Equal(exec.Opaque(expected)) {
		test.Fatal("timeline bootstrap is not the exact quoted read-only command")
	}
	if _, err := projectTimelineBootstrap("/tmp/forgectl", "/tmp/invalid\nroot", "project-id"); err == nil {
		test.Fatal("newline entered a bootstrap command")
	}
}

func TestSurfaceProjectOpenPreservesFailureCategory(test *testing.T) {
	for _, scenario := range []struct {
		class    backend.StartFailureClass
		category project.SurfaceErrorCategory
	}{
		{backend.FailureCanceled, project.SurfaceErrorCanceled},
		{backend.FailureUnavailable, project.SurfaceErrorServer},
		{backend.FailurePermissionDenied, project.SurfaceErrorEndpoint},
		{backend.FailureIdentityMismatch, project.SurfaceErrorIdentity},
		{backend.FailureTargetBusy, project.SurfaceErrorConflict},
		{backend.FailureInternal, project.SurfaceErrorInternal},
	} {
		test.Run(scenario.class.String(), func(test *testing.T) {
			root := filepath.Join(test.TempDir(), "registry")
			created := createCLIProject(test, root, test.TempDir(), "Bounded cause")
			fixture := &projectPresentationFixture{}
			fixture.start = func(_ context.Context, spec backend.StartSpec, before func(backend.ServerID, backend.EndpointID) error) backend.StartResult {
				ref := projectPresentationTestRef(test, spec.Tag(), "w1:t1", "w1:p1", "terminal-1")
				endpoint, _ := ref.Endpoint()
				if err := before(ref.Server(), endpoint); err != nil {
					test.Fatal(err)
				}
				return backend.NewNotMutated(backend.NewStartCause(scenario.class, errors.New("private adapter diagnostic")))
			}
			installProjectPresentationFixture(test, fixture)
			report, stderr, code := openCLIProject(test, root, created.Project.ID)
			if code != exitFailed || report.Operation == nil || report.Operation.Phase != project.SurfacePhaseFailed || report.Operation.ErrorCategory != scenario.category || strings.Contains(stderr, "private adapter diagnostic") {
				test.Fatalf("wrong failure classification: exit=%d stderr=%s report=%#v", code, stderr, report)
			}
		})
	}
}
