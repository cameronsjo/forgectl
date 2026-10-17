//go:build unix

package project

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/surface/backend"
	"github.com/cameronsjo/forgectl/internal/surface/mail"
)

func surfaceServer(test *testing.T, endpoint string, inode uint64) backend.ServerID {
	test.Helper()
	server, err := backend.Fingerprint(backend.IncarnationInput{Endpoint: endpoint, Version: "herdr-test", Inode: inode})
	if err != nil {
		test.Fatal(err)
	}
	return server
}

func surfaceRef(test *testing.T, server backend.ServerID, tag backend.RecoveryTag, workspace, tab, pane, terminal string, endpoint string) backend.Ref {
	test.Helper()
	identity, err := backend.NewHerdrIdentity(workspace, tab, pane)
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
	if endpoint != "" {
		ref, err = ref.WithEndpoint(backend.EndpointOf(endpoint))
		if err != nil {
			test.Fatal(err)
		}
	}
	return ref
}

func TestSurfaceOperationWorkspaceThreadLifecycleAndGuards(test *testing.T) {
	store, worktree := testStore(test)
	_, project, err := store.CreateProject(context.Background(), "Alpha", worktree, "")
	if err != nil {
		test.Fatal(err)
	}
	_, thread, err := store.CreateThread(context.Background(), project.ID, "worker", "brief")
	if err != nil {
		test.Fatal(err)
	}
	enrollTestRoster(test, store, project.ID, testWorker("worker", false, "run-1", worktree, "native-1"))
	if _, _, err := store.Bind(context.Background(), project.ID, thread.ID, "worker", "run-1", ""); err != nil {
		test.Fatal(err)
	}
	server := surfaceServer(test, filepath.Join(test.TempDir(), "herdr.sock"), 1)
	endpoint := filepath.Join(test.TempDir(), "herdr.sock")
	tag, err := backend.NewRecoveryTag()
	if err != nil {
		test.Fatal(err)
	}
	workspaceRef := surfaceRef(test, server, tag, "w2R", "w2R:t1", "w2R:p1", "term_1", endpoint)
	if _, _, err := store.BeginWorkspace(context.Background(), project.ID, "fleet", tag, server, backend.EndpointID{}); !errors.Is(err, ErrInvalidInput) {
		test.Fatalf("missing workspace endpoint = %v, want invalid input", err)
	}
	_, workspaceOperation, err := store.BeginWorkspace(context.Background(), project.ID, "fleet", tag, server, backend.EndpointOf(endpoint))
	if err != nil {
		test.Fatal(err)
	}
	incompleteIdentity, err := backend.NewHerdrIdentity("w2R", "", "")
	if err != nil {
		test.Fatal(err)
	}
	incompleteRef, err := backend.NewHerdrRef(backend.HerdrNamedSessionServer(), server, tag, incompleteIdentity)
	if err != nil {
		test.Fatal(err)
	}
	if _, _, err := store.FinishSurface(context.Background(), project.ID, workspaceOperation.ID, SurfaceVerified(incompleteRef)); !errors.Is(err, ErrConflict) {
		test.Fatalf("incomplete workspace finish = %v, want conflict", err)
	}
	wrongServerTag, err := backend.NewRecoveryTag()
	if err != nil {
		test.Fatal(err)
	}
	wrongServerRef := surfaceRef(test, surfaceServer(test, filepath.Join(test.TempDir(), "other.sock"), 2), wrongServerTag, "w2R", "w2R:t1", "w2R:p1", "term_1", endpoint)
	if _, _, err := store.FinishSurface(context.Background(), project.ID, workspaceOperation.ID, SurfaceVerified(wrongServerRef)); !errors.Is(err, ErrConflict) {
		test.Fatalf("wrong server finish = %v, want conflict", err)
	}
	wrongEndpointRef := surfaceRef(test, server, tag, "w2R", "w2R:t1", "w2R:p1", "term_1", filepath.Join(test.TempDir(), "other.sock"))
	if _, _, err := store.FinishSurface(context.Background(), project.ID, workspaceOperation.ID, SurfaceVerified(wrongEndpointRef)); !errors.Is(err, ErrConflict) {
		test.Fatalf("wrong endpoint finish = %v, want conflict", err)
	}
	if _, operation, err := store.FinishSurface(context.Background(), project.ID, workspaceOperation.ID, SurfaceVerified(workspaceRef)); err != nil || operation.Phase != SurfacePhaseVerified {
		test.Fatalf("workspace finish = %#v, %v", operation, err)
	}
	if _, _, err := store.FinishSurface(context.Background(), project.ID, workspaceOperation.ID, SurfaceVerified(workspaceRef)); !errors.Is(err, ErrConflict) {
		test.Fatalf("replayed finish = %v, want conflict", err)
	}

	wrongParentTag, err := backend.NewRecoveryTag()
	if err != nil {
		test.Fatal(err)
	}
	wrongParent := surfaceRef(test, server, wrongParentTag, "other", "other:t1", "other:p1", "term_2", endpoint)
	if _, _, err := store.BeginThread(context.Background(), project.ID, thread.ID, "run-1", "fleet", SurfacePresentationTimeline, wrongParentTag, wrongParent); !errors.Is(err, ErrConflict) {
		test.Fatalf("wrong parent = %v, want conflict", err)
	}

	childTag, err := backend.NewRecoveryTag()
	if err != nil {
		test.Fatal(err)
	}
	_, threadOperation, err := store.BeginThread(context.Background(), project.ID, thread.ID, "run-1", "fleet", SurfacePresentationTimeline, childTag, workspaceRef)
	if err != nil {
		test.Fatal(err)
	}
	sameParentSurface := surfaceRef(test, server, childTag, "w2R", "w2R:t1", "w2R:p1", "term_1", endpoint)
	if _, _, err := store.FinishSurface(context.Background(), project.ID, threadOperation.ID, SurfaceCleaned(sameParentSurface, SurfaceErrorCleanup)); !errors.Is(err, ErrConflict) {
		test.Fatalf("cleaned parent surface = %v, want conflict", err)
	}
	if _, _, err := store.FinishSurface(context.Background(), project.ID, threadOperation.ID, SurfaceUnknown(childTag, SurfaceErrorUnknown, &sameParentSurface)); !errors.Is(err, ErrConflict) {
		test.Fatalf("unknown parent surface = %v, want conflict", err)
	}
	childRef := surfaceRef(test, server, childTag, "w2R", "w2R:t2", "w2R:p2", "term_2", endpoint)
	if _, operation, err := store.FinishSurface(context.Background(), project.ID, threadOperation.ID, SurfaceVerified(childRef)); err != nil || operation.Phase != SurfacePhaseVerified {
		test.Fatalf("thread finish = %#v, %v", operation, err)
	}
	if _, _, err := store.BeginThread(context.Background(), project.ID, thread.ID, "run-1", "fleet", SurfacePresentationTimeline, childTag, workspaceRef); !errors.Is(err, ErrConflict) {
		test.Fatalf("verified duplicate = %v, want conflict", err)
	}
	valid, err := store.Snapshot(context.Background())
	if err != nil {
		test.Fatal(err)
	}
	mutations := map[string]func(*Registry){
		"missing endpoint": func(registry *Registry) { registry.Projects[0].SurfaceOperations[0].ExpectedEndpoint = "" },
		"finished before started": func(registry *Registry) {
			before := registry.Projects[0].SurfaceOperations[0].StartedAt.Add(-time.Second)
			registry.Projects[0].SurfaceOperations[0].FinishedAt = &before
		},
		"run outside thread history":     func(registry *Registry) { registry.Projects[0].SurfaceOperations[1].RunID = "run-missing" },
		"session differs from workspace": func(registry *Registry) { registry.Projects[0].SurfaceOperations[1].Session = "other" },
		"parent differs from workspace":  func(registry *Registry) { registry.Projects[0].SurfaceOperations[1].Parent = refPtr(wrongParent) },
		"duplicate active operation": func(registry *Registry) {
			registry.Projects[0].SurfaceOperations = append(registry.Projects[0].SurfaceOperations, registry.Projects[0].SurfaceOperations[0])
		},
	}
	for name, mutate := range mutations {
		test.Run(name, func(test *testing.T) {
			candidate := valid
			candidate.Projects = append([]Project(nil), valid.Projects...)
			candidate.Projects[0].SurfaceOperations = append([]SurfaceOperation(nil), valid.Projects[0].SurfaceOperations...)
			mutate(&candidate)
			if err := validateRegistry(candidate); err == nil {
				test.Fatal("validateRegistry accepted inconsistent surface operation")
			}
		})
	}
}

func TestBeginThreadRejectsStaleNativeBindingWithoutMutation(test *testing.T) {
	store, worktree := testStore(test)
	_, project, err := store.CreateProject(context.Background(), "Alpha", worktree, "")
	if err != nil {
		test.Fatal(err)
	}
	_, thread, err := store.CreateThread(context.Background(), project.ID, "worker", "")
	if err != nil {
		test.Fatal(err)
	}
	roster := enrollTestRoster(test, store, project.ID, testWorker("worker", false, "run-1", worktree, "native-1"))
	if _, _, err := store.Bind(context.Background(), project.ID, thread.ID, "worker", "run-1", ""); err != nil {
		test.Fatal(err)
	}
	server := surfaceServer(test, filepath.Join(test.TempDir(), "herdr.sock"), 3)
	endpoint := filepath.Join(test.TempDir(), "herdr.sock")
	workspaceTag, _ := backend.NewRecoveryTag()
	workspaceRef := surfaceRef(test, server, workspaceTag, "w2R", "w2R:t1", "w2R:p1", "term_1", endpoint)
	_, workspaceOperation, err := store.BeginWorkspace(context.Background(), project.ID, "fleet", workspaceTag, server, backend.EndpointOf(endpoint))
	if err != nil {
		test.Fatal(err)
	}
	if _, _, err := store.FinishSurface(context.Background(), project.ID, workspaceOperation.ID, SurfaceVerified(workspaceRef)); err != nil {
		test.Fatal(err)
	}
	childTag, _ := backend.NewRecoveryTag()
	if err := roster.Update("worker", func(worker *mail.Worker) error { worker.Coordinator = true; return nil }); err != nil {
		test.Fatal(err)
	}
	if _, _, err := store.BeginThread(context.Background(), project.ID, thread.ID, "run-1", "fleet", SurfacePresentationTimeline, childTag, workspaceRef); !errors.Is(err, ErrConflict) {
		test.Fatalf("stale role = %v, want conflict", err)
	}
	if err := roster.Update("worker", func(worker *mail.Worker) error { worker.Coordinator = false; return nil }); err != nil {
		test.Fatal(err)
	}
	if err := roster.Update("worker", func(worker *mail.Worker) error { worker.ThreadID = "native-2"; return nil }); err != nil {
		test.Fatal(err)
	}
	before, err := store.Snapshot(context.Background())
	if err != nil {
		test.Fatal(err)
	}
	if _, _, err := store.BeginThread(context.Background(), project.ID, thread.ID, "run-1", "fleet", SurfacePresentationTimeline, childTag, workspaceRef); !errors.Is(err, ErrConflict) {
		test.Fatalf("stale native session = %v, want conflict", err)
	}
	after, err := store.Snapshot(context.Background())
	if err != nil {
		test.Fatal(err)
	}
	if after.Revision != before.Revision || len(after.Projects[0].SurfaceOperations) != len(before.Projects[0].SurfaceOperations) {
		test.Fatalf("stale begin mutated registry: before=%d after=%d", before.Revision, after.Revision)
	}
}

func TestThreadSurfaceRetirementLifecycle(test *testing.T) {
	store, worktree := testStore(test)
	_, project, err := store.CreateProject(context.Background(), "Alpha", worktree, "")
	if err != nil {
		test.Fatal(err)
	}
	_, thread, err := store.CreateThread(context.Background(), project.ID, "worker", "")
	if err != nil {
		test.Fatal(err)
	}
	enrollTestRoster(test, store, project.ID, testWorker("worker", false, "run-1", worktree, "native-1"))
	if _, _, err := store.Bind(context.Background(), project.ID, thread.ID, "worker", "run-1", ""); err != nil {
		test.Fatal(err)
	}
	server := surfaceServer(test, filepath.Join(test.TempDir(), "herdr.sock"), 4)
	endpoint := filepath.Join(test.TempDir(), "herdr.sock")
	workspaceTag, _ := backend.NewRecoveryTag()
	workspaceRef := surfaceRef(test, server, workspaceTag, "w2R", "w2R:t1", "w2R:p1", "term_1", endpoint)
	_, workspaceOperation, err := store.BeginWorkspace(context.Background(), project.ID, "fleet", workspaceTag, server, backend.EndpointOf(endpoint))
	if err != nil {
		test.Fatal(err)
	}
	if _, _, err := store.FinishSurface(context.Background(), project.ID, workspaceOperation.ID, SurfaceVerified(workspaceRef)); err != nil {
		test.Fatal(err)
	}

	openThread := func(tab, pane, terminal string) SurfaceOperation {
		test.Helper()
		tag, err := backend.NewRecoveryTag()
		if err != nil {
			test.Fatal(err)
		}
		_, operation, err := store.BeginThread(context.Background(), project.ID, thread.ID, "run-1", "fleet", SurfacePresentationTimeline, tag, workspaceRef)
		if err != nil {
			test.Fatal(err)
		}
		childRef := surfaceRef(test, server, tag, "w2R", tab, pane, terminal, endpoint)
		if _, operation, err = store.FinishSurface(context.Background(), project.ID, operation.ID, SurfaceVerified(childRef)); err != nil {
			test.Fatal(err)
		}
		return operation
	}

	first := openThread("w2R:t2", "w2R:p2", "term_2")
	_, firstRetirement, err := store.BeginThreadClose(context.Background(), project.ID, first.ID)
	if err != nil {
		test.Fatal(err)
	}
	if firstRetirement.Phase != RetirementPending || firstRetirement.ID == "" {
		test.Fatalf("pending retirement = %#v", firstRetirement)
	}
	if _, _, err := store.BeginThread(context.Background(), project.ID, thread.ID, "run-1", "fleet", SurfacePresentationTimeline, backendTag(test), workspaceRef); !errors.Is(err, ErrConflict) {
		test.Fatalf("pending retirement allowed duplicate surface: %v", err)
	}
	if _, _, err := store.FinishThreadClose(context.Background(), project.ID, first.ID, firstRetirement.ID, RetirementClosedOutcome()); err != nil {
		test.Fatal(err)
	}
	if _, operation, err := store.FinishSurface(context.Background(), project.ID, first.ID, SurfaceVerified(*first.Ref)); !errors.Is(err, ErrConflict) || operation.Phase != "" {
		test.Fatalf("retirement changed original operation: operation=%#v err=%v", operation, err)
	}
	registry, err := store.Snapshot(context.Background())
	if err != nil {
		test.Fatal(err)
	}
	var savedFirst SurfaceOperation
	for _, operation := range registry.Projects[0].SurfaceOperations {
		if operation.ID == first.ID {
			savedFirst = operation
		}
	}
	if savedFirst.Phase != SurfacePhaseVerified || savedFirst.Ref == nil || !refsEqual(*savedFirst.Ref, *first.Ref) || savedFirst.Retirement == nil || savedFirst.Retirement.Phase != RetirementClosed {
		test.Fatalf("closed operation lost creation evidence: %#v", savedFirst)
	}

	second := openThread("w2R:t3", "w2R:p3", "term_3")
	_, secondRetirement, err := store.BeginThreadClose(context.Background(), project.ID, second.ID)
	if err != nil {
		test.Fatal(err)
	}
	wrongRetirementID, err := newID()
	if err != nil {
		test.Fatal(err)
	}
	if _, _, err := store.FinishThreadClose(context.Background(), project.ID, second.ID, wrongRetirementID, RetirementAlreadyGoneOutcome()); !errors.Is(err, ErrConflict) {
		test.Fatalf("wrong retirement ID = %v, want conflict", err)
	}
	if _, _, err := store.FinishThreadClose(context.Background(), project.ID, second.ID, secondRetirement.ID, RetirementAlreadyGoneOutcome()); err != nil {
		test.Fatal(err)
	}
	if _, _, err := store.FinishThreadClose(context.Background(), project.ID, second.ID, secondRetirement.ID, RetirementAlreadyGoneOutcome()); !errors.Is(err, ErrConflict) {
		test.Fatalf("replayed retirement finish = %v, want conflict", err)
	}

	third := openThread("w2R:t4", "w2R:p4", "term_4")
	_, thirdRetirement, err := store.BeginThreadClose(context.Background(), project.ID, third.ID)
	if err != nil {
		test.Fatal(err)
	}
	if _, _, err := store.FinishThreadClose(context.Background(), project.ID, third.ID, thirdRetirement.ID, RetirementUnknownOutcome(SurfaceErrorUnknown)); err != nil {
		test.Fatal(err)
	}
	if _, _, err := store.BeginThread(context.Background(), project.ID, thread.ID, "run-1", "fleet", SurfacePresentationTimeline, backendTag(test), workspaceRef); !errors.Is(err, ErrConflict) {
		test.Fatalf("unknown retirement allowed duplicate surface: %v", err)
	}
	if _, _, err := store.BeginThreadClose(context.Background(), project.ID, workspaceOperation.ID); !errors.Is(err, ErrConflict) {
		test.Fatalf("workspace retirement = %v, want conflict", err)
	}
}

func backendTag(test *testing.T) backend.RecoveryTag {
	test.Helper()
	tag, err := backend.NewRecoveryTag()
	if err != nil {
		test.Fatal(err)
	}
	return tag
}
