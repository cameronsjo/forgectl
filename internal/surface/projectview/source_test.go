//go:build unix

package projectview

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/runview"
	"github.com/cameronsjo/forgectl/internal/surface/mail"
	"github.com/cameronsjo/forgectl/internal/surface/project"
)

func TestSourceListsAggregateAndUnboundThreadsWithoutWrites(test *testing.T) {
	store, created := sourceProject(test)
	before := snapshotTree(test, store.Root)
	source := NewSource(store, created.ID)
	refs, err := source.List()
	if err != nil {
		test.Fatal(err)
	}
	if len(refs) != 3 || refs[0].Name != created.ID || refs[1].Name != created.CoordinatorThreadID {
		test.Fatalf("refs = %#v", refs)
	}
	if refs[2].Name == created.CoordinatorThreadID || refs[2].Live != runview.LiveUnknown {
		test.Fatalf("thread refs = %#v", refs)
	}
	if _, err := source.Load(refs[2], &runview.Cursor{}); err != nil {
		test.Fatal(err)
	}
	if after := snapshotTree(test, store.Root); before != after {
		test.Fatalf("read-only source changed project state: before=%s after=%s", before, after)
	}
}

func TestSourceExposesStateAndTransportOnlyEvidence(test *testing.T) {
	store, created := sourceProject(test)
	teamPath := filepath.Join(store.Root, created.ID, "team")
	if err := os.MkdirAll(teamPath, 0o700); err != nil {
		test.Fatal(err)
	}
	roster := mail.FileRoster{Dir: teamPath}
	worker := mail.Worker{
		Name: "lead", Harness: mail.HarnessCodex, Worktree: created.Worktree,
		Coordinator: true, RunID: "run-lead", ThreadID: "native-lead", StateAt: time.Now().UTC(),
	}
	if err := roster.Put(worker); err != nil {
		test.Fatal(err)
	}
	if _, _, err := store.Bind(context.Background(), created.ID, created.CoordinatorThreadID, worker.Name, worker.RunID, ""); err != nil {
		test.Fatal(err)
	}
	now := time.Now().UTC().Add(time.Second)
	box := mail.Mailbox{Dir: teamPath}
	if err := box.Locked(func(tx *mail.Tx) error {
		message := mail.Message{V: 1, ID: "m-1", From: "lead", To: "lead", FromRunID: "run-lead", ToRunID: "run-lead", Body: "secret prompt", CreatedAt: now}
		if err := tx.Enqueue(message, now); err != nil {
			return err
		}
		return tx.Mark(message.ID, mail.StatusSent, "secret detail", false, now.Add(time.Second))
	}); err != nil {
		test.Fatal(err)
	}
	source := NewSource(store, created.ID)
	refs, err := source.List()
	if err != nil {
		test.Fatal(err)
	}
	delta, err := source.Load(refs[0], &runview.Cursor{})
	if err != nil {
		test.Fatal(err)
	}
	if delta.Live != runview.LiveUnknown || len(delta.Defs) < 2 || len(delta.Events) < 2 {
		test.Fatalf("delta = %#v", delta)
	}
	if !hasEvent(delta.Events, "PROJECT-THREAD-CURRENT") || !hasEvent(delta.Events, "MAIL-TRANSPORT-ACCEPTED") {
		test.Fatalf("events = %#v", delta.Events)
	}
	threadDelta, err := source.Load(refs[1], &runview.Cursor{})
	if err != nil {
		test.Fatal(err)
	}
	if !hasEvent(threadDelta.Events, "MAIL-TRANSPORT-ACCEPTED") {
		test.Fatalf("exact thread lost exact envelope event: %#v", threadDelta.Events)
	}
	workerDelta, err := source.Load(refs[2], &runview.Cursor{})
	if err != nil {
		test.Fatal(err)
	}
	if hasEvent(workerDelta.Events, "MAIL-TRANSPORT-ACCEPTED") {
		test.Fatalf("unbound thread inherited aggregate mail: %#v", workerDelta.Events)
	}
	encoded, err := json.Marshal(delta.Events)
	if err != nil {
		test.Fatal(err)
	}
	if strings.Contains(string(encoded), "secret prompt") || strings.Contains(string(encoded), "secret detail") || strings.Contains(string(encoded), "COMPLETED") {
		test.Fatalf("source exposed unsafe/completion text: %s", encoded)
	}

	if err := roster.Update(worker.Name, func(current *mail.Worker) error {
		current.RunID = "run-replaced"
		return nil
	}); err != nil {
		test.Fatal(err)
	}
	delta, err = source.Load(refs[0], &runview.Cursor{})
	if err != nil {
		test.Fatal(err)
	}
	if !hasEvent(delta.Events, "PROJECT-THREAD-STALE") || !hasEvent(delta.Events, "MAIL-TRANSPORT-ACCEPTED") {
		test.Fatalf("stale delta = %#v", delta.Events)
	}
}

func TestSourceMakesMalformedMailboxPartialAndVisible(test *testing.T) {
	store, created := sourceProject(test)
	teamPath := filepath.Join(store.Root, created.ID, "team")
	if err := os.MkdirAll(teamPath, 0o700); err != nil {
		test.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(teamPath, "mailbox.jsonl"), []byte("{\"op\":\"msg\",\"op\":\"msg\"}\n{\"op\":\"msg\"}"), 0o600); err != nil {
		test.Fatal(err)
	}
	source := NewSource(store, created.ID)
	refs, err := source.List()
	if err != nil {
		test.Fatal(err)
	}
	delta, err := source.Load(refs[0], &runview.Cursor{})
	if err != nil {
		test.Fatal(err)
	}
	if !delta.Partial || delta.Err == nil || len(delta.Events) == 0 {
		test.Fatalf("malformed delta = %#v", delta)
	}
}

func TestSourceKeepsNativeTurnsArtifactsAndHistoricalRunsDistinct(test *testing.T) {
	store, created := sourceProject(test)
	teamPath := filepath.Join(store.Root, created.ID, "team")
	if err := os.MkdirAll(teamPath, 0o700); err != nil {
		test.Fatal(err)
	}
	roster := mail.FileRoster{Dir: teamPath}
	worker := mail.Worker{Name: "lead", Harness: mail.HarnessCodex, Worktree: created.Worktree, Coordinator: true, RunID: "original-run", ThreadID: "original-native"}
	if err := roster.Put(worker); err != nil {
		test.Fatal(err)
	}
	if _, _, err := store.Bind(test.Context(), created.ID, created.CoordinatorThreadID, worker.Name, worker.RunID, ""); err != nil {
		test.Fatal(err)
	}
	input := project.NativeEvidenceInput{NativeSessionID: worker.ThreadID, Source: project.EvidenceSourceCodexThreadRead, TurnID: "native-turn", Status: project.EvidenceCompleted, ObservedAt: time.Now().UTC()}
	if _, _, err := store.RecordNativeEvidence(test.Context(), created.ID, created.CoordinatorThreadID, worker.RunID, input); err != nil {
		test.Fatal(err)
	}
	contents := []byte("source independently checked elsewhere\n")
	if err := os.WriteFile(filepath.Join(created.Worktree, "result.txt"), contents, 0o600); err != nil {
		test.Fatal(err)
	}
	digest := sha256.Sum256(contents)
	if _, _, err := store.RecordVerifiedArtifact(test.Context(), created.ID, created.CoordinatorThreadID, worker.RunID, "result.txt", hex.EncodeToString(digest[:])); err != nil {
		test.Fatal(err)
	}
	source := NewSource(store, created.ID)
	refs, err := source.List()
	if err != nil {
		test.Fatal(err)
	}
	before := snapshotTree(test, store.Root)
	delta, err := source.Load(refs[1], &runview.Cursor{})
	if err != nil {
		test.Fatal(err)
	}
	if !hasEvent(delta.Events, "NATIVE-TURN-COMPLETED") || !hasEvent(delta.Events, "ARTIFACT-BYTES-MATCHED") || delta.Live != runview.LiveUnknown || len(source.(*Source).Spec(refs[1]).On) != 0 {
		test.Fatalf("missing or overclaimed evidence: %#v", delta)
	}
	for _, event := range delta.Events {
		if event.Step == nativeTurnStep || event.Step == artifactStep {
			if eventField(event, "currently_enrolled") != "true" || eventField(event, "run_id") != "original-run" || eventField(event, "native_session_id") != "original-native" {
				test.Fatalf("evidence identity lost: %#v", event)
			}
		}
	}
	if after := snapshotTree(test, store.Root); after != before {
		test.Fatal("timeline mutated durable evidence")
	}
	unrelated, err := source.Load(refs[2], &runview.Cursor{})
	if err != nil || hasEvent(unrelated.Events, "NATIVE-TURN-COMPLETED") || hasEvent(unrelated.Events, "ARTIFACT-BYTES-MATCHED") {
		test.Fatalf("unrelated thread inherited evidence: %v %#v", err, unrelated)
	}
	worker.RunID, worker.ThreadID = "replacement-run", "replacement-native"
	if err := roster.Put(worker); err != nil {
		test.Fatal(err)
	}
	if _, _, err := store.Bind(test.Context(), created.ID, created.CoordinatorThreadID, worker.Name, worker.RunID, "original-run"); err != nil {
		test.Fatal(err)
	}
	delta, err = source.Load(refs[1], &runview.Cursor{})
	if err != nil {
		test.Fatal(err)
	}
	if !hasEvent(delta.Events, "PROJECT-THREAD-CURRENT") || !hasEvent(delta.Events, "NATIVE-TURN-COMPLETED") {
		test.Fatalf("replacement lost history or current state: %#v", delta)
	}
	for _, event := range delta.Events {
		if event.Step == nativeTurnStep || event.Step == artifactStep {
			if eventField(event, "currently_enrolled") != "false" || eventField(event, "run_id") != "original-run" {
				test.Fatalf("old evidence rebound to replacement: %#v", event)
			}
		}
	}
	valid := snapshotTree(test, store.Root)
	corrupt := strings.Replace(valid, `"status":"completed"`, `"status":"invented-success"`, 1)
	if corrupt == valid {
		test.Fatal("negative control did not alter the serialized evidence status")
	}
	if err := os.WriteFile(filepath.Join(store.Root, "registry.json"), []byte(corrupt), 0o600); err != nil {
		test.Fatal(err)
	}
	invalid, err := source.Load(refs[1], &runview.Cursor{})
	if err == nil || len(invalid.Events) != 0 {
		test.Fatalf("malformed evidence became rendered success: %v %#v", err, invalid)
	}
}

func eventField(event runview.Event, key string) string {
	for _, field := range event.Fields {
		if field.Key == key {
			return field.Value
		}
	}
	return ""
}

func sourceProject(test *testing.T) (project.Store, project.Project) {
	test.Helper()
	parent := test.TempDir()
	worktree := test.TempDir()
	store := project.Store{Root: filepath.Join(parent, "registry")}
	_, created, err := store.CreateProject(context.Background(), "Alpha", worktree, "")
	if err != nil {
		test.Fatal(err)
	}
	if _, _, err := store.CreateThread(context.Background(), created.ID, "worker", ""); err != nil {
		test.Fatal(err)
	}
	return store, created
}

func snapshotTree(test *testing.T, root string) string {
	test.Helper()
	directory, err := os.OpenRoot(root)
	if err != nil {
		test.Fatal(err)
	}
	defer func() { _ = directory.Close() }()
	data, err := directory.ReadFile("registry.json")
	if err != nil {
		test.Fatal(err)
	}
	return string(data)
}

func hasEvent(events []runview.Event, name string) bool {
	for _, event := range events {
		if event.Name == name {
			return true
		}
	}
	return false
}
