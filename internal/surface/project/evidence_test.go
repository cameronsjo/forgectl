//go:build unix

package project

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/surface/mail"
)

func TestRecordNativeEvidenceDeduplicatesAndRejectsConflicts(test *testing.T) {
	fixture := newRetirementFixture(test)
	observedAt := time.Now().UTC()
	input := NativeEvidenceInput{
		NativeSessionID: "native-1", Source: EvidenceSourceCodexThreadRead,
		TurnID: "turn-1", Status: EvidenceCompleted, Provider: "openai", Model: "gpt-5",
		ObservedAt: observedAt,
	}
	registry, evidence, err := fixture.store.RecordNativeEvidence(context.Background(), fixture.project.ID, fixture.thread.ID, "run-1", input)
	if err != nil {
		test.Fatal(err)
	}
	if len(registry.Projects[0].Evidence) != 1 || evidence.Status != EvidenceCompleted || evidence.Harness != mail.HarnessCodex {
		test.Fatalf("recorded evidence = %#v", evidence)
	}
	optional := input
	optional.TurnID = "turn-optional-metadata"
	optional.Provider = ""
	optional.Model = ""
	registry, optionalEvidence, err := fixture.store.RecordNativeEvidence(context.Background(), fixture.project.ID, fixture.thread.ID, "run-1", optional)
	if err != nil {
		test.Fatalf("optional provider/model evidence = %v", err)
	}
	if len(registry.Projects[0].Evidence) != 2 || optionalEvidence.Provider != "" || optionalEvidence.Model != "" {
		test.Fatalf("optional provider/model evidence = %#v", optionalEvidence)
	}
	registryPath := filepath.Join(fixture.store.Root, registryName)
	beforeBytes := readTestFile(test, registryPath)
	beforeRevision := registry.Revision
	input.ObservedAt = observedAt.Add(time.Minute)
	repeated, repeatedEvidence, err := fixture.store.RecordNativeEvidence(context.Background(), fixture.project.ID, fixture.thread.ID, "run-1", input)
	if err != nil {
		test.Fatal(err)
	}
	if repeated.Revision != beforeRevision || repeatedEvidence.ObservedAt != evidence.ObservedAt {
		test.Fatalf("repeat changed state: revision=%d evidence=%#v", repeated.Revision, repeatedEvidence)
	}
	if got := readTestFile(test, registryPath); string(got) != string(beforeBytes) {
		test.Fatalf("repeat changed registry bytes")
	}
	input.Status = EvidenceFailed
	if _, _, err := fixture.store.RecordNativeEvidence(context.Background(), fixture.project.ID, fixture.thread.ID, "run-1", input); !errors.Is(err, ErrConflict) {
		test.Fatalf("conflicting turn = %v, want conflict", err)
	}
	input.Status = EvidenceCompleted
	input.Source = EvidenceSourcePiInbox
	if _, _, err := fixture.store.RecordNativeEvidence(context.Background(), fixture.project.ID, fixture.thread.ID, "run-1", input); !errors.Is(err, ErrConflict) {
		test.Fatalf("source/harness mismatch = %v, want conflict", err)
	}
	input.Source = EvidenceSourceCodexThreadRead
	input.NativeSessionID = "other-session"
	if _, _, err := fixture.store.RecordNativeEvidence(context.Background(), fixture.project.ID, fixture.thread.ID, "run-1", input); !errors.Is(err, ErrConflict) {
		test.Fatalf("stale native session = %v, want conflict", err)
	}
}

func TestRecordVerifiedArtifactUsesBoundWorktreeAndDeduplicates(test *testing.T) {
	fixture := newRetirementFixture(test)
	artifactPath := filepath.Join(fixture.project.Worktree, "result.txt")
	contents := []byte("verified result\n")
	if err := os.WriteFile(artifactPath, contents, 0o600); err != nil {
		test.Fatal(err)
	}
	digest := sha256.Sum256(contents)
	expectedSHA256 := hex.EncodeToString(digest[:])
	registry, evidence, err := fixture.store.RecordVerifiedArtifact(context.Background(), fixture.project.ID, fixture.thread.ID, "run-1", "result.txt", expectedSHA256)
	if err != nil {
		test.Fatal(err)
	}
	if len(registry.Projects[0].Evidence) != 1 || evidence.Source != EvidenceSourceArtifactDigest || evidence.Status != EvidenceDigestMatch || evidence.Artifact == nil {
		test.Fatalf("artifact evidence = %#v", evidence)
	}
	if evidence.Artifact.RelativePath != "result.txt" || evidence.Artifact.SHA256 != expectedSHA256 || evidence.Artifact.Size != int64(len(contents)) {
		test.Fatalf("artifact digest = %#v", evidence.Artifact)
	}
	beforeRevision := registry.Revision
	registryPath := filepath.Join(fixture.store.Root, registryName)
	beforeBytes := readTestFile(test, registryPath)
	repeated, _, err := fixture.store.RecordVerifiedArtifact(context.Background(), fixture.project.ID, fixture.thread.ID, "run-1", "result.txt", expectedSHA256)
	if err != nil {
		test.Fatal(err)
	}
	if repeated.Revision != beforeRevision || string(readTestFile(test, registryPath)) != string(beforeBytes) {
		test.Fatalf("artifact repeat changed registry")
	}
	wrongDigest := "0000000000000000000000000000000000000000000000000000000000000000"
	if _, _, err := fixture.store.RecordVerifiedArtifact(context.Background(), fixture.project.ID, fixture.thread.ID, "run-1", "result.txt", wrongDigest); !errors.Is(err, ErrConflict) {
		test.Fatalf("wrong artifact digest = %v, want conflict", err)
	}
	if _, _, err := fixture.store.RecordVerifiedArtifact(context.Background(), fixture.project.ID, fixture.thread.ID, "run-1", "../result.txt", expectedSHA256); !errors.Is(err, ErrInvalidInput) {
		test.Fatalf("artifact traversal = %v, want invalid input", err)
	}
}

func TestEvidenceRequiresCurrentBindingButPreservesHistory(test *testing.T) {
	fixture := newRetirementFixture(test)
	input := NativeEvidenceInput{
		NativeSessionID: "native-1", Source: EvidenceSourceCodexThreadRead,
		TurnID: "turn-old", Status: EvidenceCompleted, Provider: "openai", Model: "gpt-5",
		ObservedAt: time.Now().UTC(),
	}
	if _, _, err := fixture.store.RecordNativeEvidence(context.Background(), fixture.project.ID, fixture.thread.ID, "run-1", input); err != nil {
		test.Fatal(err)
	}
	roster := mail.FileRoster{Dir: filepath.Join(fixture.store.Root, fixture.project.ID, projectDirectoryName)}
	if err := roster.Put(testWorker("worker", false, "run-2", fixture.project.Worktree, "native-2")); err != nil {
		test.Fatal(err)
	}
	if _, _, err := fixture.store.Bind(context.Background(), fixture.project.ID, fixture.thread.ID, "worker", "run-2", "run-1"); err != nil {
		test.Fatal(err)
	}
	input.NativeSessionID = "native-1"
	input.TurnID = "turn-stale"
	if _, _, err := fixture.store.RecordNativeEvidence(context.Background(), fixture.project.ID, fixture.thread.ID, "run-1", input); !errors.Is(err, ErrConflict) {
		test.Fatalf("historical run record = %v, want conflict", err)
	}
	input.NativeSessionID = "native-2"
	input.TurnID = "turn-new"
	registry, _, err := fixture.store.RecordNativeEvidence(context.Background(), fixture.project.ID, fixture.thread.ID, "run-2", input)
	if err != nil {
		test.Fatal(err)
	}
	if len(registry.Projects[0].Evidence) != 2 {
		test.Fatalf("evidence history length = %d, want 2", len(registry.Projects[0].Evidence))
	}
}

func TestRecordVerifiedArtifactRejectsRosterReplacementDuringHash(test *testing.T) {
	fixture := newRetirementFixture(test)
	contents := []byte("checked bytes with a replaced enrollment\n")
	if err := os.WriteFile(filepath.Join(fixture.project.Worktree, "result.txt"), contents, 0o600); err != nil {
		test.Fatal(err)
	}
	digest := sha256.Sum256(contents)
	before := readTestFile(test, filepath.Join(fixture.store.Root, registryName))
	previousHook := artifactAfterOpenHook
	test.Cleanup(func() { artifactAfterOpenHook = previousHook })
	replaced := false
	artifactAfterOpenHook = func(string) {
		roster := mail.FileRoster{Dir: filepath.Join(fixture.store.Root, fixture.project.ID, projectDirectoryName)}
		if err := roster.Put(testWorker("worker", false, "replacement-run", fixture.project.Worktree, "replacement-native")); err != nil {
			test.Fatal(err)
		}
		replaced = true
	}
	if _, _, err := fixture.store.RecordVerifiedArtifact(test.Context(), fixture.project.ID, fixture.thread.ID, "run-1", "result.txt", hex.EncodeToString(digest[:])); !errors.Is(err, ErrConflict) {
		test.Fatalf("replacement during hash was accepted: %v", err)
	}
	if !replaced || string(readTestFile(test, filepath.Join(fixture.store.Root, registryName))) != string(before) {
		test.Fatal("negative control did not replace the roster or stale evidence changed registry bytes")
	}
}

func TestValidateEvidenceRejectsInvalidSerializedShapes(test *testing.T) {
	fixture := newRetirementFixture(test)
	input := NativeEvidenceInput{
		NativeSessionID: "native-1", Source: EvidenceSourceCodexThreadRead,
		TurnID: "turn-1", Status: EvidenceCompleted, Provider: "openai", Model: "gpt-5",
		ObservedAt: time.Now().UTC(),
	}
	if _, _, err := fixture.store.RecordNativeEvidence(context.Background(), fixture.project.ID, fixture.thread.ID, "run-1", input); err != nil {
		test.Fatal(err)
	}
	valid, err := fixture.store.Snapshot(context.Background())
	if err != nil {
		test.Fatal(err)
	}
	mutations := map[string]func(*Registry){
		"unknown source":        func(registry *Registry) { registry.Projects[0].Evidence[0].Source = "made-up" },
		"digest status native":  func(registry *Registry) { registry.Projects[0].Evidence[0].Status = EvidenceDigestMatch },
		"missing history":       func(registry *Registry) { registry.Projects[0].Evidence[0].RunID = "run-missing" },
		"provider control":      func(registry *Registry) { registry.Projects[0].Evidence[0].Provider = "openai\n" },
		"provider invalid utf8": func(registry *Registry) { registry.Projects[0].Evidence[0].Provider = string([]byte{0xff}) },
		"model format":          func(registry *Registry) { registry.Projects[0].Evidence[0].Model = "gpt-5\u200e" },
		"artifact dot path": func(registry *Registry) {
			registry.Projects[0].Evidence[0].Source = EvidenceSourceArtifactDigest
			registry.Projects[0].Evidence[0].Status = EvidenceDigestMatch
			registry.Projects[0].Evidence[0].TurnID = ""
			registry.Projects[0].Evidence[0].Provider = ""
			registry.Projects[0].Evidence[0].Model = ""
			registry.Projects[0].Evidence[0].Artifact = &ArtifactDigest{RelativePath: ".", Size: 1, SHA256: strings.Repeat("0", 64)}
		},
		"artifact invalid utf8 path": func(registry *Registry) {
			registry.Projects[0].Evidence[0].Source = EvidenceSourceArtifactDigest
			registry.Projects[0].Evidence[0].Status = EvidenceDigestMatch
			registry.Projects[0].Evidence[0].TurnID = ""
			registry.Projects[0].Evidence[0].Provider = ""
			registry.Projects[0].Evidence[0].Model = ""
			registry.Projects[0].Evidence[0].Artifact = &ArtifactDigest{RelativePath: string([]byte{0xff}), Size: 1, SHA256: strings.Repeat("0", 64)}
		},
		"artifact nul path": func(registry *Registry) {
			registry.Projects[0].Evidence[0].Source = EvidenceSourceArtifactDigest
			registry.Projects[0].Evidence[0].Status = EvidenceDigestMatch
			registry.Projects[0].Evidence[0].TurnID = ""
			registry.Projects[0].Evidence[0].Provider = ""
			registry.Projects[0].Evidence[0].Model = ""
			registry.Projects[0].Evidence[0].Artifact = &ArtifactDigest{RelativePath: "result\x00.txt", Size: 1, SHA256: strings.Repeat("0", 64)}
		},
		"duplicate identity": func(registry *Registry) {
			registry.Projects[0].Evidence = append(registry.Projects[0].Evidence, registry.Projects[0].Evidence[0])
		},
	}
	for name, mutate := range mutations {
		test.Run(name, func(test *testing.T) {
			candidate := valid
			candidate.Projects = append([]Project(nil), valid.Projects...)
			candidate.Projects[0].Evidence = append([]Evidence(nil), valid.Projects[0].Evidence...)
			mutate(&candidate)
			if err := validateRegistry(candidate); err == nil {
				test.Fatal("validateRegistry accepted invalid evidence")
			}
		})
	}
}
