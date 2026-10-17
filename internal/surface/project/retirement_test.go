//go:build unix

package project

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/cameronsjo/forgectl/internal/surface/backend"
)

type retirementFixture struct {
	store              Store
	project            Project
	thread             Thread
	workspaceRef       backend.Ref
	workspaceOperation SurfaceOperation
	threadOperation    SurfaceOperation
	registry           Registry
}

func newRetirementFixture(test *testing.T) retirementFixture {
	test.Helper()
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
	endpoint := filepath.Join(test.TempDir(), "herdr.sock")
	server := surfaceServer(test, endpoint, 11)
	workspaceTag := backendTag(test)
	workspaceRef := surfaceRef(test, server, workspaceTag, "w2R", "w2R:t1", "w2R:p1", "term_1", endpoint)
	_, workspaceOperation, err := store.BeginWorkspace(context.Background(), project.ID, "fleet", workspaceTag, server, backend.EndpointOf(endpoint))
	if err != nil {
		test.Fatal(err)
	}
	if _, _, err := store.FinishSurface(context.Background(), project.ID, workspaceOperation.ID, SurfaceVerified(workspaceRef)); err != nil {
		test.Fatal(err)
	}
	childTag := backendTag(test)
	_, threadOperation, err := store.BeginThread(context.Background(), project.ID, thread.ID, "run-1", "fleet", SurfacePresentationTimeline, childTag, workspaceRef)
	if err != nil {
		test.Fatal(err)
	}
	childRef := surfaceRef(test, server, childTag, "w2R", "w2R:t2", "w2R:p2", "term_2", endpoint)
	registry, finishedOperation, err := store.FinishSurface(context.Background(), project.ID, threadOperation.ID, SurfaceVerified(childRef))
	if err != nil {
		test.Fatal(err)
	}
	threadOperation = finishedOperation
	return retirementFixture{
		store: store, project: project, thread: thread, workspaceRef: workspaceRef,
		workspaceOperation: workspaceOperation, threadOperation: threadOperation, registry: registry,
	}
}

func TestRetirementSerializedStateRejectsAdversarialShapes(test *testing.T) {
	fixture := newRetirementFixture(test)
	_, retirement, err := fixture.store.BeginThreadClose(context.Background(), fixture.project.ID, fixture.threadOperation.ID)
	if err != nil {
		test.Fatal(err)
	}
	if _, _, err := fixture.store.FinishThreadClose(context.Background(), fixture.project.ID, fixture.threadOperation.ID, retirement.ID, RetirementClosedOutcome()); err != nil {
		test.Fatal(err)
	}
	fixture.registry, err = fixture.store.Snapshot(context.Background())
	if err != nil {
		test.Fatal(err)
	}
	registryPath := filepath.Join(fixture.store.Root, registryName)
	original := readTestFile(test, registryPath)
	cases := map[string]func(map[string]json.RawMessage){
		"unknown retirement key": func(retirement map[string]json.RawMessage) {
			retirement["unexpected"] = json.RawMessage(`true`)
		},
		"null retirement":         nil,
		"null presentation":       func(_ map[string]json.RawMessage) {},
		"null operation finished": func(_ map[string]json.RawMessage) {},
		"null retirement finished": func(retirement map[string]json.RawMessage) {
			retirement["finished_at"] = json.RawMessage(`null`)
		},
		"pending operation with reference":  func(_ map[string]json.RawMessage) {},
		"workspace kind with thread fields": func(_ map[string]json.RawMessage) {},
		"retirement without presentation":   func(_ map[string]json.RawMessage) {},
		"retirement started before surface finished": func(retirement map[string]json.RawMessage) {
			retirement["started_at"] = json.RawMessage(`"1970-01-01T00:00:00Z"`)
		},
		"retirement finished before started": func(retirement map[string]json.RawMessage) {
			retirement["finished_at"] = json.RawMessage(`"1970-01-01T00:00:00Z"`)
		},
		"impossible retirement phase": func(retirement map[string]json.RawMessage) {
			retirement["phase"] = json.RawMessage(`"pending"`)
		},
	}
	for name, mutate := range cases {
		test.Run(name, func(test *testing.T) {
			candidate := mutateRetirementFixtureJSON(test, original, func(operation map[string]json.RawMessage, retirement map[string]json.RawMessage) {
				switch name {
				case "null retirement":
					operation["retirement"] = json.RawMessage(`null`)
				case "null presentation":
					operation["presentation"] = json.RawMessage(`null`)
				case "null operation finished":
					operation["finished_at"] = json.RawMessage(`null`)
				case "pending operation with reference":
					operation["phase"] = json.RawMessage(`"pending"`)
				case "workspace kind with thread fields":
					operation["kind"] = json.RawMessage(`"workspace"`)
				case "retirement without presentation":
					delete(operation, "presentation")
				default:
					if mutate != nil {
						mutate(retirement)
					}
				}
			})
			expectedLiteral := map[string]string{
				"unknown retirement key":                     `"unexpected":true`,
				"null retirement":                            `"retirement":null`,
				"null presentation":                          `"presentation":null`,
				"null operation finished":                    `"finished_at":null`,
				"null retirement finished":                   `"finished_at":null`,
				"retirement started before surface finished": `1970-01-01T00:00:00Z`,
				"retirement finished before started":         `1970-01-01T00:00:00Z`,
				"impossible retirement phase":                `"phase":"pending"`,
			}[name]
			if expectedLiteral != "" && !strings.Contains(string(candidate), expectedLiteral) {
				test.Fatalf("candidate lost expected mutation %q: %s", expectedLiteral, candidate)
			}
			writeTestFile(test, registryPath, candidate, 0o600)
			if _, err := fixture.store.Snapshot(context.Background()); !errors.Is(err, ErrInvalidState) {
				test.Fatalf("Snapshot = %v, want ErrInvalidState", err)
			}
			if got := readTestFile(test, registryPath); string(got) != string(candidate) {
				test.Fatalf("invalid state was rewritten: %q", got)
			}
			writeTestFile(test, registryPath, original, 0o600)
		})
	}
}

func TestRetirementIDsMustBeUniqueAcrossOperations(test *testing.T) {
	fixture := newRetirementFixture(test)
	_, retirement, err := fixture.store.BeginThreadClose(context.Background(), fixture.project.ID, fixture.threadOperation.ID)
	if err != nil {
		test.Fatal(err)
	}
	if _, _, err := fixture.store.FinishThreadClose(context.Background(), fixture.project.ID, fixture.threadOperation.ID, retirement.ID, RetirementClosedOutcome()); err != nil {
		test.Fatal(err)
	}
	registry, err := fixture.store.Snapshot(context.Background())
	if err != nil {
		test.Fatal(err)
	}
	duplicate := registry.Projects[0].SurfaceOperations[1]
	duplicate.ID = newTestID(test)
	registry.Projects[0].SurfaceOperations = append(registry.Projects[0].SurfaceOperations, duplicate)
	if err := validateRegistry(registry); err == nil {
		test.Fatal("validateRegistry accepted duplicate retirement ID")
	}
}

func TestLegacyThreadPresentationIsReadableButNotClosable(test *testing.T) {
	fixture := newRetirementFixture(test)
	registryPath := filepath.Join(fixture.store.Root, registryName)
	original := readTestFile(test, registryPath)
	legacy := mutateRetirementFixtureJSON(test, original, func(operation map[string]json.RawMessage, _ map[string]json.RawMessage) {
		delete(operation, "presentation")
	})
	writeTestFile(test, registryPath, legacy, 0o600)
	registry, err := fixture.store.Snapshot(context.Background())
	if err != nil {
		test.Fatalf("legacy Snapshot = %v", err)
	}
	if registry.Revision != fixture.registry.Revision || registry.Projects[0].SurfaceOperations[1].Presentation != "" {
		test.Fatalf("legacy registry = %#v", registry)
	}
	if _, _, err := fixture.store.BeginThreadClose(context.Background(), fixture.project.ID, fixture.threadOperation.ID); !errors.Is(err, ErrConflict) {
		test.Fatalf("legacy BeginThreadClose = %v, want conflict", err)
	}
	if got := readTestFile(test, registryPath); string(got) != string(legacy) {
		test.Fatalf("legacy close changed bytes: %q", got)
	}
}

func TestCanceledRetirementMutationsPreserveBytesAndRevision(test *testing.T) {
	fixture := newRetirementFixture(test)
	registryPath := filepath.Join(fixture.store.Root, registryName)
	beforeBytes := readTestFile(test, registryPath)
	before, err := fixture.store.Snapshot(context.Background())
	if err != nil {
		test.Fatal(err)
	}
	beginContext, cancelBegin := context.WithCancel(context.Background())
	cancelBegin()
	if _, _, err := fixture.store.BeginThreadClose(beginContext, fixture.project.ID, fixture.threadOperation.ID); !errors.Is(err, context.Canceled) {
		test.Fatalf("canceled BeginThreadClose = %v", err)
	}
	assertRetirementStateUnchanged(test, fixture.store, registryPath, beforeBytes, before.Revision)

	_, retirement, err := fixture.store.BeginThreadClose(context.Background(), fixture.project.ID, fixture.threadOperation.ID)
	if err != nil {
		test.Fatal(err)
	}
	pendingBytes := readTestFile(test, registryPath)
	pending, err := fixture.store.Snapshot(context.Background())
	if err != nil {
		test.Fatal(err)
	}
	finishContext, cancelFinish := context.WithCancel(context.Background())
	cancelFinish()
	if _, _, err := fixture.store.FinishThreadClose(finishContext, fixture.project.ID, fixture.threadOperation.ID, retirement.ID, RetirementClosedOutcome()); !errors.Is(err, context.Canceled) {
		test.Fatalf("canceled FinishThreadClose = %v", err)
	}
	assertRetirementStateUnchanged(test, fixture.store, registryPath, pendingBytes, pending.Revision)
}

func TestConcurrentBeginThreadCloseUsesOneCASWinner(test *testing.T) {
	fixture := newRetirementFixture(test)
	const attempts = 8
	results := make(chan error, attempts)
	var waitGroup sync.WaitGroup
	for index := 0; index < attempts; index++ {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			_, _, err := fixture.store.BeginThreadClose(context.Background(), fixture.project.ID, fixture.threadOperation.ID)
			results <- err
		}()
	}
	waitGroup.Wait()
	close(results)
	winners := 0
	for err := range results {
		if err == nil {
			winners++
			continue
		}
		if !errors.Is(err, ErrConflict) {
			test.Fatalf("concurrent BeginThreadClose = %v, want conflict for losers", err)
		}
	}
	if winners != 1 {
		test.Fatalf("concurrent BeginThreadClose winners = %d, want 1", winners)
	}
	registry, err := fixture.store.Snapshot(context.Background())
	if err != nil {
		test.Fatal(err)
	}
	if retirement := registry.Projects[0].SurfaceOperations[1].Retirement; retirement == nil || retirement.Phase != RetirementPending {
		test.Fatalf("retirement after concurrent begin = %#v", retirement)
	}
}

func mutateRetirementFixtureJSON(test *testing.T, original []byte, mutate func(map[string]json.RawMessage, map[string]json.RawMessage)) []byte {
	test.Helper()
	var root map[string]json.RawMessage
	if err := json.Unmarshal(original, &root); err != nil {
		test.Fatal(err)
	}
	var projects []json.RawMessage
	if err := json.Unmarshal(root["projects"], &projects); err != nil {
		test.Fatal(err)
	}
	var project map[string]json.RawMessage
	if err := json.Unmarshal(projects[0], &project); err != nil {
		test.Fatal(err)
	}
	var operations []json.RawMessage
	if err := json.Unmarshal(project["surface_operations"], &operations); err != nil {
		test.Fatal(err)
	}
	var operation map[string]json.RawMessage
	if err := json.Unmarshal(operations[1], &operation); err != nil {
		test.Fatal(err)
	}
	var retirement map[string]json.RawMessage
	retirementWasExplicitNull := false
	if raw, ok := operation["retirement"]; ok && bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		retirementWasExplicitNull = true
	}
	if raw, ok := operation["retirement"]; ok && !retirementWasExplicitNull {
		if err := json.Unmarshal(raw, &retirement); err != nil {
			test.Fatal(err)
		}
	}
	mutate(operation, retirement)
	if retirement != nil && !bytes.Equal(bytes.TrimSpace(operation["retirement"]), []byte("null")) {
		updatedRetirement, err := json.Marshal(retirement)
		if err != nil {
			test.Fatal(err)
		}
		operation["retirement"] = updatedRetirement
	}
	updatedOperation, err := json.Marshal(operation)
	if err != nil {
		test.Fatal(err)
	}
	operations[1] = updatedOperation
	project["surface_operations"], err = json.Marshal(operations)
	if err != nil {
		test.Fatal(err)
	}
	projects[0], err = json.Marshal(project)
	if err != nil {
		test.Fatal(err)
	}
	root["projects"], err = json.Marshal(projects)
	if err != nil {
		test.Fatal(err)
	}
	updated, err := json.Marshal(root)
	if err != nil {
		test.Fatal(err)
	}
	return updated
}

func assertRetirementStateUnchanged(test *testing.T, store Store, registryPath string, expectedBytes []byte, expectedRevision uint64) {
	test.Helper()
	if got := readTestFile(test, registryPath); string(got) != string(expectedBytes) {
		test.Fatalf("state bytes changed: got %q want %q", got, expectedBytes)
	}
	registry, err := store.Snapshot(context.Background())
	if err != nil {
		test.Fatal(err)
	}
	if registry.Revision != expectedRevision {
		test.Fatalf("revision changed: got %d want %d", registry.Revision, expectedRevision)
	}
}

func newTestID(test *testing.T) string {
	test.Helper()
	id, err := newID()
	if err != nil {
		test.Fatal(err)
	}
	return id
}
