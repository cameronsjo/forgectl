//go:build unix

package project

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func testStore(test *testing.T) (Store, string) {
	test.Helper()
	parent := test.TempDir()
	if err := os.Chmod(parent, 0o755); err != nil { //nolint:gosec // G302: intentionally broad parent proves placement does not require privacy
		test.Fatal(err)
	}
	return Store{Root: filepath.Join(parent, "registry")}, test.TempDir()
}

func TestStoreCreatesPrivateRootUnderBroadParent(test *testing.T) {
	store, worktree := testStore(test)
	registry, created, err := store.CreateProject(context.Background(), "Alpha", worktree, "instructions")
	if err != nil {
		test.Fatal(err)
	}
	if registry.Version != Version || registry.Revision != 1 || len(registry.Projects) != 1 {
		test.Fatalf("registry = %#v", registry)
	}
	if created.CoordinatorThreadID != created.Threads[0].ID || created.Threads[0].Role != RoleCoordinator {
		test.Fatalf("coordinator = %#v", created.Threads)
	}
	rootInfo, err := os.Stat(store.Root)
	if err != nil {
		test.Fatal(err)
	}
	if rootInfo.Mode().Perm() != 0o700 {
		test.Fatalf("root mode = %o, want 700", rootInfo.Mode().Perm())
	}
	if _, err := os.Stat(filepath.Join(store.Root, created.ID)); !errors.Is(err, os.ErrNotExist) {
		test.Fatalf("logical creation materialized team directory: %v", err)
	}
}

func TestStoreSnapshotAbsentDoesNotWrite(test *testing.T) {
	store, _ := testStore(test)
	registry, err := store.Snapshot(context.Background())
	if err != nil {
		test.Fatal(err)
	}
	if registry.Version != Version || registry.Revision != 0 || len(registry.Projects) != 0 {
		test.Fatalf("empty snapshot = %#v", registry)
	}
	if _, err := os.Stat(store.Root); !errors.Is(err, os.ErrNotExist) {
		test.Fatalf("snapshot materialized root: %v", err)
	}
}

func TestStoreRefusesCorruptRegistryWithoutReplacement(test *testing.T) {
	store, worktree := testStore(test)
	if _, _, err := store.CreateProject(context.Background(), "Alpha", worktree, ""); err != nil {
		test.Fatal(err)
	}
	registryPath := filepath.Join(store.Root, registryName)
	original := readTestFile(test, registryPath)
	corruptions := map[string]string{
		"duplicate key": strings.Replace(string(original), `"version":1`, `"version":1,"version":1`, 1),
		"unknown key":   strings.Replace(string(original), `"version":1`, `"version":1,"unknown":0`, 1),
		"trailing":      string(original) + `{"unexpected":true}`,
	}
	for name, contents := range corruptions {
		test.Run(name, func(test *testing.T) {
			writeTestFile(test, registryPath, []byte(contents), 0o600)
			if _, err := store.Snapshot(context.Background()); !errors.Is(err, ErrInvalidState) {
				test.Fatalf("Snapshot = %v, want ErrInvalidState", err)
			}
			observed := readTestFile(test, registryPath)
			if string(observed) != contents {
				test.Fatalf("Snapshot changed corrupt state: %q", observed)
			}
			writeTestFile(test, registryPath, original, 0o600)
		})
	}
}

func TestStoreRejectsPublicRootWithoutRepairingIt(test *testing.T) {
	store, _ := testStore(test)
	if err := os.Mkdir(store.Root, 0o755); err != nil { //nolint:gosec // G301: intentionally public root proves refusal without repair
		test.Fatal(err)
	}
	if _, err := store.Snapshot(context.Background()); !errors.Is(err, ErrInvalidState) {
		test.Fatalf("Snapshot = %v, want ErrInvalidState", err)
	}
	info, err := os.Stat(store.Root)
	if err != nil {
		test.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		test.Fatalf("public root was repaired to %o", info.Mode().Perm())
	}
}

func TestStoreConcurrentThreadsAdvanceRevision(test *testing.T) {
	store, worktree := testStore(test)
	registry, createdProject, err := store.CreateProject(context.Background(), "Alpha", worktree, "")
	if err != nil {
		test.Fatal(err)
	}
	const writers = 8
	var waitGroup sync.WaitGroup
	errorsFound := make(chan error, writers)
	for writer := 0; writer < writers; writer++ {
		waitGroup.Add(1)
		go func(writer int) {
			defer waitGroup.Done()
			_, _, err := store.CreateThread(context.Background(), createdProject.ID, "worker", strings.Repeat("b", writer))
			if err != nil {
				errorsFound <- err
			}
		}(writer)
	}
	waitGroup.Wait()
	close(errorsFound)
	for err := range errorsFound {
		test.Fatal(err)
	}
	final, err := store.Snapshot(context.Background())
	if err != nil {
		test.Fatal(err)
	}
	if final.Revision != registry.Revision+writers || len(final.Projects[0].Threads) != writers+1 {
		test.Fatalf("final registry revision=%d threads=%d, want revision=%d threads=%d", final.Revision, len(final.Projects[0].Threads), registry.Revision+writers, writers+1)
	}
}

func TestStoreRefusesPublicStateFilesWithoutRepairingThem(test *testing.T) {
	store, worktree := testStore(test)
	if _, _, err := store.CreateProject(context.Background(), "Alpha", worktree, ""); err != nil {
		test.Fatal(err)
	}
	registryPath := filepath.Join(store.Root, registryName)
	lockPath := filepath.Join(store.Root, registryLock)
	for _, path := range []string{registryPath, lockPath} {
		if err := os.Chmod(path, 0o644); err != nil { //nolint:gosec // G302: intentionally public state fixture proves refusal without repair
			test.Fatal(err)
		}
		if _, err := store.Snapshot(context.Background()); !errors.Is(err, ErrInvalidState) {
			test.Fatalf("Snapshot with public %s = %v, want ErrInvalidState", filepath.Base(path), err)
		}
		info, err := os.Stat(path)
		if err != nil {
			test.Fatal(err)
		}
		if info.Mode().Perm() != 0o644 {
			test.Fatalf("Snapshot repaired %s to mode %o", filepath.Base(path), info.Mode().Perm())
		}
		if err := os.Chmod(path, 0o600); err != nil {
			test.Fatal(err)
		}
	}
}

func TestStoreRefusesSymlinkAndHardlinkState(test *testing.T) {
	cases := []struct {
		name  string
		setup func(test *testing.T, store Store, original []byte)
	}{
		{
			name: "root symlink",
			setup: func(test *testing.T, store Store, original []byte) {
				linkRoot := filepath.Join(filepath.Dir(store.Root), "root-link")
				if err := os.Symlink(store.Root, linkRoot); err != nil {
					test.Fatal(err)
				}
				checkStorePathRefused(test, Store{Root: linkRoot}, filepath.Join(store.Root, registryName), original)
			},
		},
		{
			name: "root hardlink",
			setup: func(test *testing.T, store Store, original []byte) {
				linkRoot := filepath.Join(filepath.Dir(store.Root), "root-file-link")
				target := filepath.Join(filepath.Dir(store.Root), "root-file")
				if err := os.WriteFile(target, []byte("root target"), 0o600); err != nil {
					test.Fatal(err)
				}
				if err := os.Link(target, linkRoot); err != nil {
					test.Fatal(err)
				}
				checkStorePathRefused(test, Store{Root: linkRoot}, target, []byte("root target"))
			},
		},
		{
			name: "registry symlink",
			setup: func(test *testing.T, store Store, original []byte) {
				registryPath := filepath.Join(store.Root, registryName)
				target := filepath.Join(filepath.Dir(store.Root), "registry-target")
				writeTestFile(test, target, original, 0o600)
				if err := os.Remove(registryPath); err != nil {
					test.Fatal(err)
				}
				if err := os.Symlink(target, registryPath); err != nil {
					test.Fatal(err)
				}
				checkStorePathRefused(test, store, target, original)
			},
		},
		{
			name: "registry hardlink",
			setup: func(test *testing.T, store Store, original []byte) {
				registryPath := filepath.Join(store.Root, registryName)
				target := filepath.Join(filepath.Dir(store.Root), "registry-target")
				writeTestFile(test, target, original, 0o600)
				if err := os.Remove(registryPath); err != nil {
					test.Fatal(err)
				}
				if err := os.Link(target, registryPath); err != nil {
					test.Fatal(err)
				}
				checkStorePathRefused(test, store, target, original)
			},
		},
		{
			name: "lock symlink",
			setup: func(test *testing.T, store Store, original []byte) {
				lockPath := filepath.Join(store.Root, registryLock)
				target := filepath.Join(filepath.Dir(store.Root), "lock-target")
				if err := os.WriteFile(target, []byte("lock target"), 0o600); err != nil {
					test.Fatal(err)
				}
				if err := os.Remove(lockPath); err != nil {
					test.Fatal(err)
				}
				if err := os.Symlink(target, lockPath); err != nil {
					test.Fatal(err)
				}
				checkStorePathRefused(test, store, target, []byte("lock target"))
			},
		},
		{
			name: "lock hardlink",
			setup: func(test *testing.T, store Store, original []byte) {
				lockPath := filepath.Join(store.Root, registryLock)
				target := filepath.Join(filepath.Dir(store.Root), "lock-target")
				if err := os.WriteFile(target, []byte("lock target"), 0o600); err != nil {
					test.Fatal(err)
				}
				if err := os.Remove(lockPath); err != nil {
					test.Fatal(err)
				}
				if err := os.Link(target, lockPath); err != nil {
					test.Fatal(err)
				}
				checkStorePathRefused(test, store, target, []byte("lock target"))
			},
		},
	}
	for _, testCase := range cases {
		test.Run(testCase.name, func(test *testing.T) {
			store, worktree := testStore(test)
			if _, _, err := store.CreateProject(context.Background(), "Alpha", worktree, ""); err != nil {
				test.Fatal(err)
			}
			original, err := os.ReadFile(filepath.Join(store.Root, registryName))
			if err != nil {
				test.Fatal(err)
			}
			testCase.setup(test, store, original)
		})
	}
}

func checkStorePathRefused(test *testing.T, store Store, preservedPath string, original []byte) {
	test.Helper()
	if _, err := store.Snapshot(context.Background()); !errors.Is(err, ErrInvalidState) {
		test.Fatalf("Snapshot = %v, want ErrInvalidState", err)
	}
	observed := readTestFile(test, preservedPath)
	if string(observed) != string(original) {
		test.Fatalf("refused state changed preserved target to %q", observed)
	}
}

func TestStoreRejectsInvalidStructuredStateWithoutMutation(test *testing.T) {
	store, worktree := testStore(test)
	_, createdProject, err := store.CreateProject(context.Background(), "Alpha", worktree, "")
	if err != nil {
		test.Fatal(err)
	}
	registryPath := filepath.Join(store.Root, registryName)
	original := readTestFile(test, registryPath)
	var decoded Registry
	if err := json.Unmarshal(original, &decoded); err != nil {
		test.Fatal(err)
	}
	decoded.Projects = append(decoded.Projects, decoded.Projects[0])
	duplicateIDs, err := json.Marshal(decoded)
	if err != nil {
		test.Fatal(err)
	}
	deepJSON := "0"
	for depth := 0; depth <= maxJSONDepth; depth++ {
		deepJSON = "[" + deepJSON + "]"
	}
	invalidStates := map[string][]byte{
		"duplicate IDs":       duplicateIDs,
		"unsupported version": []byte(strings.Replace(string(original), `"version":1`, `"version":2`, 1)),
		"null projects":       []byte(`{"version":1,"revision":0,"projects":null}`),
		"deep JSON":           []byte(`{"version":1,"revision":0,"projects":[],"unknown":` + deepJSON + `}`),
		"malformed":           []byte("{broken"),
	}
	for name, contents := range invalidStates {
		test.Run(name, func(test *testing.T) {
			writeTestFile(test, registryPath, contents, 0o600)
			if _, err := store.Snapshot(context.Background()); !errors.Is(err, ErrInvalidState) {
				test.Fatalf("Snapshot = %v, want ErrInvalidState", err)
			}
			if _, _, err := store.CreateThread(context.Background(), createdProject.ID, "worker", "brief"); !errors.Is(err, ErrInvalidState) {
				test.Fatalf("CreateThread = %v, want ErrInvalidState", err)
			}
			observed := readTestFile(test, registryPath)
			if string(observed) != string(contents) {
				test.Fatalf("invalid state was replaced: %q", observed)
			}
			writeTestFile(test, registryPath, original, 0o600)
		})
	}
}

func TestStoreEvidenceTimingSchemaRejectsUnknownAndNullWithoutMutation(test *testing.T) {
	fixture := newRetirementFixture(test)
	observedAt := time.Now().UTC().Truncate(time.Microsecond)
	startedAt := observedAt.Add(-time.Minute)
	endedAt := observedAt.Add(-30 * time.Second)
	if _, _, err := fixture.store.RecordNativeEvidence(context.Background(), fixture.project.ID, fixture.thread.ID, "run-1", NativeEvidenceInput{
		NativeSessionID: "native-1", Source: EvidenceSourceCodexThreadRead, TurnID: "turn-schema",
		Status: EvidenceCompleted, ObservedAt: observedAt, StartedAt: &startedAt, EndedAt: &endedAt,
	}); err != nil {
		test.Fatal(err)
	}
	registryPath := filepath.Join(fixture.store.Root, registryName)
	original := readTestFile(test, registryPath)
	observedJSON, err := json.Marshal(observedAt)
	if err != nil {
		test.Fatal(err)
	}
	startedJSON, err := json.Marshal(startedAt)
	if err != nil {
		test.Fatal(err)
	}
	endedJSON, err := json.Marshal(endedAt)
	if err != nil {
		test.Fatal(err)
	}
	needle := `,"observed_at":` + string(observedJSON) + `,"started_at":` + string(startedJSON) + `,"ended_at":` + string(endedJSON)
	if !strings.Contains(string(original), needle) {
		test.Fatalf("timed evidence JSON did not contain expected fields: %s", original)
	}
	unknown := strings.Replace(string(original), needle, strings.Replace(needle, `,"started_at":`, `,"unexpected":0,"started_at":`, 1), 1)
	nullStarted := strings.Replace(string(original), needle, strings.Replace(needle, `,"started_at":`+string(startedJSON), `,"started_at":null`, 1), 1)
	invalidStates := map[string]string{
		"unknown timing sibling": unknown,
		"null optional timing":   nullStarted,
	}
	for name, contents := range invalidStates {
		test.Run(name, func(test *testing.T) {
			writeTestFile(test, registryPath, []byte(contents), 0o600)
			if _, err := fixture.store.Snapshot(context.Background()); !errors.Is(err, ErrInvalidState) {
				test.Fatalf("Snapshot = %v, want ErrInvalidState", err)
			}
			if got := readTestFile(test, registryPath); string(got) != contents {
				test.Fatal("rejected schema changed the private registry file")
			}
			writeTestFile(test, registryPath, original, 0o600)
		})
	}
}

func TestStoreUnknownAndCanceledThreadAbsentRootDoNotCreateState(test *testing.T) {
	store, _ := testStore(test)
	validProjectID := "00000000-0000-0000-0000-000000000001"
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := store.CreateThread(ctx, validProjectID, "worker", ""); !errors.Is(err, context.Canceled) {
		test.Fatalf("canceled CreateThread = %v, want context.Canceled", err)
	}
	if _, err := os.Stat(store.Root); !errors.Is(err, os.ErrNotExist) {
		test.Fatalf("canceled CreateThread created root: %v", err)
	}
	if _, _, err := store.CreateThread(context.Background(), validProjectID, "worker", ""); !errors.Is(err, ErrNotFound) {
		test.Fatalf("unknown CreateThread = %v, want ErrNotFound", err)
	}
	if _, err := os.Stat(store.Root); !errors.Is(err, os.ErrNotExist) {
		test.Fatalf("unknown CreateThread created root: %v", err)
	}
}

func readTestFile(test *testing.T, path string) []byte {
	test.Helper()
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		test.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	contents, err := root.ReadFile(filepath.Base(path))
	if err != nil {
		test.Fatal(err)
	}
	return contents
}

func writeTestFile(test *testing.T, path string, contents []byte, mode os.FileMode) {
	test.Helper()
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		test.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	if err := root.WriteFile(filepath.Base(path), contents, mode); err != nil {
		test.Fatal(err)
	}
}
