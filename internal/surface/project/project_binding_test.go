//go:build unix

package project

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/surface/mail"
)

func enrollTestRoster(test *testing.T, store Store, projectID string, workers ...mail.Worker) mail.FileRoster {
	test.Helper()
	teamPath := filepath.Join(store.Root, projectID, projectDirectoryName)
	if err := os.MkdirAll(teamPath, 0o700); err != nil {
		test.Fatal(err)
	}
	roster := mail.FileRoster{Dir: teamPath}
	for _, worker := range workers {
		if err := roster.Put(worker); err != nil {
			test.Fatal(err)
		}
	}
	return roster
}

func testWorker(name string, coordinator bool, runID, worktree, nativeSession string) mail.Worker {
	return mail.Worker{
		Name:        name,
		Harness:     mail.HarnessCodex,
		Worktree:    worktree,
		Coordinator: coordinator,
		RunID:       runID,
		ThreadID:    nativeSession,
		Socket:      filepath.Join(worktree, name+".sock"),
		State:       mail.StateIdle,
		StateAt:     time.Now().UTC(),
	}
}

func TestBindInspectAndSameRunEnrichment(test *testing.T) {
	store, worktree := testStore(test)
	_, project, err := store.CreateProject(context.Background(), "Alpha", worktree, "instructions")
	if err != nil {
		test.Fatal(err)
	}
	_, workerThread, err := store.CreateThread(context.Background(), project.ID, "worker", "brief")
	if err != nil {
		test.Fatal(err)
	}
	roster := enrollTestRoster(test, store, project.ID,
		testWorker("lead", true, "run-lead", worktree, ""),
		testWorker("worker", false, "run-worker", test.TempDir(), ""),
	)
	registry, bound, err := store.Bind(context.Background(), project.ID, project.CoordinatorThreadID, "lead", "run-lead", "")
	if err != nil {
		test.Fatal(err)
	}
	if bound.ActiveRunID != "run-lead" || len(bound.Runs) != 1 || registry.Revision != 3 {
		test.Fatalf("bound=%#v revision=%d", bound, registry.Revision)
	}
	if _, _, observations, err := store.Inspect(context.Background(), project.ID); err != nil {
		test.Fatal(err)
	} else if observations[0].Enrollment != EnrollmentCurrent || observations[0].NativeSessionKnown {
		test.Fatalf("initial observations=%#v", observations)
	}
	if _, _, err := store.Bind(context.Background(), project.ID, workerThread.ID, "lead", "run-lead", ""); !errors.Is(err, ErrConflict) {
		test.Fatalf("worker role bind = %v, want conflict", err)
	}
	if err := roster.Update("lead", func(worker *mail.Worker) error {
		worker.ThreadID = "native-lead"
		return nil
	}); err != nil {
		test.Fatal(err)
	}
	if _, _, observations, err := store.Inspect(context.Background(), project.ID); err != nil {
		test.Fatal(err)
	} else if observations[0].Enrollment != EnrollmentStale || observations[0].Reason != "native-session-mismatch" {
		test.Fatalf("stale observations=%#v", observations)
	}
	registry, bound, err = store.Bind(context.Background(), project.ID, project.CoordinatorThreadID, "lead", "run-lead", "run-lead")
	if err != nil {
		test.Fatal(err)
	}
	if registry.Revision != 4 || len(bound.Runs) != 1 || bound.Runs[0].NativeSessionID != "native-lead" {
		test.Fatalf("enriched bound=%#v revision=%d", bound, registry.Revision)
	}
	registryPath := filepath.Join(store.Root, registryName)
	beforeEndpointChange := readTestFile(test, registryPath)
	if err := roster.Update("lead", func(worker *mail.Worker) error {
		worker.Socket = filepath.Join(worktree, "changed.sock")
		return nil
	}); err != nil {
		test.Fatal(err)
	}
	if _, _, err := store.Bind(context.Background(), project.ID, project.CoordinatorThreadID, "lead", "run-lead", "run-lead"); !errors.Is(err, ErrConflict) {
		test.Fatalf("same-run endpoint change = %v, want conflict", err)
	}
	afterEndpointChange := readTestFile(test, registryPath)
	if string(afterEndpointChange) != string(beforeEndpointChange) {
		test.Fatalf("endpoint refusal changed registry")
	}
	if err := roster.Update("lead", func(worker *mail.Worker) error {
		worker.ThreadID = "native-other"
		return nil
	}); err != nil {
		test.Fatal(err)
	}
	if _, _, err := store.Bind(context.Background(), project.ID, project.CoordinatorThreadID, "lead", "run-lead", "run-lead"); !errors.Is(err, ErrConflict) {
		test.Fatalf("same-run native session change = %v, want conflict", err)
	}
	if afterNativeChange := readTestFile(test, registryPath); string(afterNativeChange) != string(beforeEndpointChange) {
		test.Fatalf("native session refusal changed registry")
	}
	if err := roster.Update("lead", func(worker *mail.Worker) error {
		worker.Socket = filepath.Join(worktree, "lead.sock")
		worker.ThreadID = "native-lead"
		return nil
	}); err != nil {
		test.Fatal(err)
	}
	unchanged, err := store.Snapshot(context.Background())
	if err != nil {
		test.Fatal(err)
	}
	_, _, err = store.Bind(context.Background(), project.ID, project.CoordinatorThreadID, "lead", "run-lead", "run-lead")
	if err != nil {
		test.Fatal(err)
	}
	final, err := store.Snapshot(context.Background())
	if err != nil {
		test.Fatal(err)
	}
	if final.Revision != unchanged.Revision {
		test.Fatalf("no-op revision=%d, before=%d", final.Revision, unchanged.Revision)
	}
}

func TestBindReplacementHistoryAndDifferentWorktree(test *testing.T) {
	store, worktree := testStore(test)
	_, project, err := store.CreateProject(context.Background(), "Alpha", worktree, "")
	if err != nil {
		test.Fatal(err)
	}
	roster := enrollTestRoster(test, store, project.ID, testWorker("lead", true, "run-one", test.TempDir(), "session-one"))
	if _, _, err := store.Bind(context.Background(), project.ID, project.CoordinatorThreadID, "lead", "run-one", ""); err != nil {
		test.Fatal(err)
	}
	replacement := testWorker("lead", true, "run-two", test.TempDir(), "session-two")
	replacement.Harness = mail.HarnessPi
	if err := roster.Put(replacement); err != nil {
		test.Fatal(err)
	}
	registry, bound, err := store.Bind(context.Background(), project.ID, project.CoordinatorThreadID, "lead", "run-two", "run-one")
	if err != nil {
		test.Fatal(err)
	}
	if len(bound.Runs) != 2 || bound.ActiveRunID != "run-two" || registry.Projects[0].Threads[0].Runs[0].Worktree == replacement.Worktree {
		test.Fatalf("replacement=%#v", bound)
	}
	if err := roster.Put(testWorker("lead", true, "run-one", replacement.Worktree, "session-one-again")); err != nil {
		test.Fatal(err)
	}
	if _, _, err := store.Bind(context.Background(), project.ID, project.CoordinatorThreadID, "lead", "run-one", "run-two"); !errors.Is(err, ErrConflict) {
		test.Fatalf("historical reuse = %v, want conflict", err)
	}
	if err := roster.Put(testWorker("lead", true, "run-three", replacement.Worktree, "session-three")); err != nil {
		test.Fatal(err)
	}
	_, _, observations, err := store.Inspect(context.Background(), project.ID)
	if err != nil {
		test.Fatal(err)
	}
	if observations[0].Enrollment != EnrollmentStale || observations[0].Reason != "run-mismatch" {
		test.Fatalf("replacement observations=%#v", observations)
	}
}

func TestBindRejectsCrossThreadHistoricalReuse(test *testing.T) {
	store, worktree := testStore(test)
	_, project, err := store.CreateProject(context.Background(), "Alpha", worktree, "")
	if err != nil {
		test.Fatal(err)
	}
	_, firstThread, err := store.CreateThread(context.Background(), project.ID, "first", "")
	if err != nil {
		test.Fatal(err)
	}
	_, secondThread, err := store.CreateThread(context.Background(), project.ID, "second", "")
	if err != nil {
		test.Fatal(err)
	}
	enrollTestRoster(test, store, project.ID,
		testWorker("lead", true, "run-lead", worktree, "session-lead"),
		testWorker("worker-a", false, "run-a", test.TempDir(), "session-a"),
		testWorker("worker-b", false, "run-b", test.TempDir(), "session-b"),
	)
	if _, _, err := store.Bind(context.Background(), project.ID, firstThread.ID, "worker-a", "run-a", ""); err != nil {
		test.Fatal(err)
	}
	if _, _, err := store.Bind(context.Background(), project.ID, secondThread.ID, "worker-b", "run-b", ""); err != nil {
		test.Fatal(err)
	}
	replacementRoster, err := json.Marshal(rosterDocument{Version: 1, Workers: []mail.Worker{
		testWorker("worker-b", false, "run-a", test.TempDir(), "session-b-reused"),
	}})
	if err != nil {
		test.Fatal(err)
	}
	writeTestFile(test, filepath.Join(store.Root, project.ID, projectDirectoryName, rosterName), replacementRoster, 0o600)
	if _, _, err := store.Bind(context.Background(), project.ID, secondThread.ID, "worker-b", "run-a", "run-b"); !errors.Is(err, ErrConflict) {
		test.Fatalf("cross-thread historical reuse = %v, want conflict", err)
	}
}

func TestBindCASContention(test *testing.T) {
	store, worktree := testStore(test)
	_, project, err := store.CreateProject(context.Background(), "Alpha", worktree, "")
	if err != nil {
		test.Fatal(err)
	}
	enrollTestRoster(test, store, project.ID, testWorker("lead", true, "run-one", worktree, "session-one"))
	results := make(chan error, 2)
	var waitGroup sync.WaitGroup
	for range 2 {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			_, _, bindErr := store.Bind(context.Background(), project.ID, project.CoordinatorThreadID, "lead", "run-one", "")
			results <- bindErr
		}()
	}
	waitGroup.Wait()
	close(results)
	var successes, conflicts int
	for bindErr := range results {
		switch {
		case bindErr == nil:
			successes++
		case errors.Is(bindErr, ErrConflict):
			conflicts++
		default:
			test.Fatalf("CAS bind = %v", bindErr)
		}
	}
	if successes != 1 || conflicts != 1 {
		test.Fatalf("successes=%d conflicts=%d", successes, conflicts)
	}
}

func TestBindCanceledDoesNotMutateRegistry(test *testing.T) {
	store, worktree := testStore(test)
	_, project, err := store.CreateProject(context.Background(), "Alpha", worktree, "")
	if err != nil {
		test.Fatal(err)
	}
	enrollTestRoster(test, store, project.ID, testWorker("lead", true, "run-one", worktree, "session-one"))
	registryPath := filepath.Join(store.Root, registryName)
	original := readTestFile(test, registryPath)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := store.Bind(ctx, project.ID, project.CoordinatorThreadID, "lead", "run-one", ""); !errors.Is(err, context.Canceled) {
		test.Fatalf("canceled Bind = %v, want context.Canceled", err)
	}
	if observed := readTestFile(test, registryPath); !bytes.Equal(observed, original) {
		test.Fatalf("canceled Bind changed registry")
	}
}

func TestInspectMissingRosterAndUnsafeRoster(test *testing.T) {
	store, worktree := testStore(test)
	_, project, err := store.CreateProject(context.Background(), "Alpha", worktree, "")
	if err != nil {
		test.Fatal(err)
	}
	_, _, observations, err := store.Inspect(context.Background(), project.ID)
	if err != nil {
		test.Fatal(err)
	}
	if len(observations) != 1 || observations[0].Enrollment != EnrollmentUnbound {
		test.Fatalf("missing roster observations=%#v", observations)
	}
	teamPath := filepath.Join(store.Root, project.ID, projectDirectoryName)
	if err := os.MkdirAll(teamPath, 0o700); err != nil {
		test.Fatal(err)
	}
	rosterPath := filepath.Join(teamPath, rosterName)
	if err := os.WriteFile(rosterPath, []byte(`{"v":1,"workers":[{"name":"lead"}]}`), 0o600); err != nil {
		test.Fatal(err)
	}
	original := readTestFile(test, rosterPath)
	if _, _, _, err := store.Inspect(context.Background(), project.ID); !errors.Is(err, ErrInvalidState) {
		test.Fatalf("malformed roster = %v, want invalid state", err)
	}
	observed := readTestFile(test, rosterPath)
	if string(observed) != string(original) {
		test.Fatalf("malformed roster changed: %q", observed)
	}
	if err := os.Chmod(rosterPath, 0o644); err != nil { //nolint:gosec // G302: intentionally public fixture proves read refusal
		test.Fatal(err)
	}
	if _, _, _, err := store.Inspect(context.Background(), project.ID); !errors.Is(err, ErrInvalidState) {
		test.Fatalf("public roster = %v, want invalid state", err)
	}
	if err := os.Chmod(rosterPath, 0o600); err != nil {
		test.Fatal(err)
	}
	linkPath := filepath.Join(teamPath, "roster-target")
	if err := os.Rename(rosterPath, linkPath); err != nil {
		test.Fatal(err)
	}
	if err := os.Symlink(linkPath, rosterPath); err != nil {
		test.Fatal(err)
	}
	if _, _, _, err := store.Inspect(context.Background(), project.ID); !errors.Is(err, ErrInvalidState) {
		test.Fatalf("symlink roster = %v, want invalid state", err)
	}
}

func TestInspectBoundMissingRosterReportsStaleWithoutRepair(test *testing.T) {
	store, worktree := testStore(test)
	_, project, err := store.CreateProject(context.Background(), "Alpha", worktree, "")
	if err != nil {
		test.Fatal(err)
	}
	enrollTestRoster(test, store, project.ID, testWorker("lead", true, "run-one", worktree, "session-one"))
	if _, _, err := store.Bind(context.Background(), project.ID, project.CoordinatorThreadID, "lead", "run-one", ""); err != nil {
		test.Fatal(err)
	}
	registryPath := filepath.Join(store.Root, registryName)
	original := readTestFile(test, registryPath)
	teamPath := filepath.Join(store.Root, project.ID, projectDirectoryName)
	if err := os.Remove(filepath.Join(teamPath, rosterName)); err != nil {
		test.Fatal(err)
	}
	_, _, observations, err := store.Inspect(context.Background(), project.ID)
	if err != nil {
		test.Fatal(err)
	}
	if len(observations) != 1 || observations[0].Enrollment != EnrollmentStale || observations[0].Reason != "roster-missing" {
		test.Fatalf("missing bound roster observations=%#v", observations)
	}
	if observed := readTestFile(test, registryPath); !bytes.Equal(observed, original) {
		test.Fatalf("stale inspection changed registry")
	}
	if _, err := os.Stat(teamPath); err != nil {
		test.Fatalf("stale inspection changed team directory: %v", err)
	}
}

func TestInspectRejectsAdversarialRosterDocuments(test *testing.T) {
	store, worktree := testStore(test)
	_, project, err := store.CreateProject(context.Background(), "Alpha", worktree, "")
	if err != nil {
		test.Fatal(err)
	}
	enrollTestRoster(test, store, project.ID, testWorker("lead", true, "run-one", worktree, "session-one"))
	rosterPath := filepath.Join(store.Root, project.ID, projectDirectoryName, rosterName)
	valid := readTestFile(test, rosterPath)
	validDocument := rosterDocument{Version: 1, Workers: []mail.Worker{
		testWorker("lead", true, "run-one", worktree, "session-one"),
	}}
	duplicateName, err := json.Marshal(rosterDocument{Version: 1, Workers: []mail.Worker{
		validDocument.Workers[0],
		testWorker("lead", false, "run-two", worktree, "session-two"),
	}})
	if err != nil {
		test.Fatal(err)
	}
	duplicateRunID, err := json.Marshal(rosterDocument{Version: 1, Workers: []mail.Worker{
		validDocument.Workers[0],
		testWorker("other", false, "run-one", worktree, "session-two"),
	}})
	if err != nil {
		test.Fatal(err)
	}
	unknownKey := strings.Replace(string(valid), `"v": 1`, `"v": 1, "unknown": 0`, 1)
	unknownWorkerKey := strings.Replace(string(valid), `"name": "lead"`, `"name": "lead", "unknown": "x"`, 1)
	cases := map[string][]byte{
		"unknown version":            []byte(strings.Replace(string(valid), `"v": 1`, `"v": 2`, 1)),
		"unknown root key":           []byte(unknownKey),
		"unknown worker key":         []byte(unknownWorkerKey),
		"trailing data":              append(append([]byte(nil), valid...), []byte(`{"unexpected":true}`)...),
		"duplicate names":            duplicateName,
		"duplicate nonempty run IDs": duplicateRunID,
		"oversize":                   bytes.Repeat([]byte("x"), maxRosterBytes+1),
	}
	workerCases := map[string][]mail.Worker{
		"multiple coordinators": {validDocument.Workers[0], testWorker("other", true, "run-two", worktree, "session-two")},
		"unknown state":         {validDocument.Workers[0]},
		"invalid watcher":       {validDocument.Workers[0]},
		"pi without socket":     {validDocument.Workers[0]},
		"hermes without socket": {validDocument.Workers[0]},
	}
	workerCases["unknown state"][0].State = "invented"
	workerCases["invalid watcher"][0].Watchers = []string{"bad/name"}
	workerCases["pi without socket"][0].Harness = mail.HarnessPi
	workerCases["pi without socket"][0].Socket = ""
	workerCases["hermes without socket"][0].Harness = mail.HarnessHermes
	workerCases["hermes without socket"][0].Socket = ""
	for name, workers := range workerCases {
		contents, marshalErr := json.Marshal(rosterDocument{Version: 1, Workers: workers})
		if marshalErr != nil {
			test.Fatal(marshalErr)
		}
		cases[name] = contents
	}
	for name, contents := range cases {
		test.Run(name, func(test *testing.T) {
			writeTestFile(test, rosterPath, contents, 0o600)
			if _, _, _, err := store.Inspect(context.Background(), project.ID); !errors.Is(err, ErrInvalidState) {
				test.Fatalf("Inspect = %v, want ErrInvalidState", err)
			}
			if observed := readTestFile(test, rosterPath); !bytes.Equal(observed, contents) {
				test.Fatalf("Inspect changed hostile roster")
			}
			writeTestFile(test, rosterPath, valid, 0o600)
		})
	}
}

func TestInspectRejectsUnsafeNestedProjectAndTeam(test *testing.T) {
	cases := map[string]func(test *testing.T, store Store, projectID string){
		"project symlink": func(test *testing.T, store Store, projectID string) {
			target := filepath.Join(test.TempDir(), "project-target")
			if err := os.Mkdir(target, 0o700); err != nil {
				test.Fatal(err)
			}
			if err := os.Symlink(target, filepath.Join(store.Root, projectID)); err != nil {
				test.Fatal(err)
			}
		},
		"project public": func(test *testing.T, store Store, projectID string) {
			if err := os.Mkdir(filepath.Join(store.Root, projectID), 0o755); err != nil { //nolint:gosec // G301: intentionally public fixture
				test.Fatal(err)
			}
		},
		"team symlink": func(test *testing.T, store Store, projectID string) {
			projectPath := filepath.Join(store.Root, projectID)
			if err := os.Mkdir(projectPath, 0o700); err != nil {
				test.Fatal(err)
			}
			target := filepath.Join(test.TempDir(), "team-target")
			if err := os.Mkdir(target, 0o700); err != nil {
				test.Fatal(err)
			}
			if err := os.Symlink(target, filepath.Join(projectPath, projectDirectoryName)); err != nil {
				test.Fatal(err)
			}
		},
		"team public": func(test *testing.T, store Store, projectID string) {
			projectPath := filepath.Join(store.Root, projectID)
			if err := os.Mkdir(projectPath, 0o700); err != nil {
				test.Fatal(err)
			}
			if err := os.Mkdir(filepath.Join(projectPath, projectDirectoryName), 0o755); err != nil { //nolint:gosec // G301: intentionally public fixture
				test.Fatal(err)
			}
		},
	}
	for name, setup := range cases {
		test.Run(name, func(test *testing.T) {
			store, worktree := testStore(test)
			_, project, err := store.CreateProject(context.Background(), "Alpha", worktree, "")
			if err != nil {
				test.Fatal(err)
			}
			registryPath := filepath.Join(store.Root, registryName)
			original := readTestFile(test, registryPath)
			setup(test, store, project.ID)
			if _, _, _, err := store.Inspect(context.Background(), project.ID); !errors.Is(err, ErrInvalidState) {
				test.Fatalf("Inspect = %v, want ErrInvalidState", err)
			}
			if observed := readTestFile(test, registryPath); !bytes.Equal(observed, original) {
				test.Fatalf("Inspect changed registry")
			}
		})
	}
}

func TestInspectRejectsUnsafeRosterLocks(test *testing.T) {
	cases := map[string]func(test *testing.T, lockPath string){
		"public": func(test *testing.T, lockPath string) {
			if err := os.Chmod(lockPath, 0o644); err != nil { //nolint:gosec // G302: intentionally public fixture
				test.Fatal(err)
			}
		},
		"symlink": func(test *testing.T, lockPath string) {
			target := filepath.Join(test.TempDir(), "lock-target")
			if err := os.WriteFile(target, nil, 0o600); err != nil {
				test.Fatal(err)
			}
			if err := os.Remove(lockPath); err != nil {
				test.Fatal(err)
			}
			if err := os.Symlink(target, lockPath); err != nil {
				test.Fatal(err)
			}
		},
		"hardlink": func(test *testing.T, lockPath string) {
			target := filepath.Join(test.TempDir(), "lock-target")
			if err := os.WriteFile(target, []byte("lock"), 0o600); err != nil {
				test.Fatal(err)
			}
			if err := os.Remove(lockPath); err != nil {
				test.Fatal(err)
			}
			if err := os.Link(target, lockPath); err != nil {
				test.Fatal(err)
			}
		},
	}
	for name, setup := range cases {
		test.Run(name, func(test *testing.T) {
			store, worktree := testStore(test)
			_, project, err := store.CreateProject(context.Background(), "Alpha", worktree, "")
			if err != nil {
				test.Fatal(err)
			}
			enrollTestRoster(test, store, project.ID, testWorker("lead", true, "run-one", worktree, "session-one"))
			lockPath := filepath.Join(store.Root, project.ID, projectDirectoryName, rosterLockName)
			registryPath := filepath.Join(store.Root, registryName)
			original := readTestFile(test, registryPath)
			setup(test, lockPath)
			if _, _, _, err := store.Inspect(context.Background(), project.ID); !errors.Is(err, ErrInvalidState) {
				test.Fatalf("Inspect = %v, want ErrInvalidState", err)
			}
			if observed := readTestFile(test, registryPath); !bytes.Equal(observed, original) {
				test.Fatalf("Inspect changed registry")
			}
		})
	}
}
