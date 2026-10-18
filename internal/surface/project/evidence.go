package project

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/cameronsjo/forgectl/internal/surface/mail"
)

const (
	maxEvidenceRecords      = 256
	maxEvidenceTextBytes    = 256
	maxEvidenceArtifactPath = 4096
)

type EvidenceSource string

const (
	EvidenceSourceCodexThreadRead  EvidenceSource = "codex-thread-read"
	EvidenceSourcePiInbox          EvidenceSource = "pi-inbox"
	EvidenceSourceHermesSupervisor EvidenceSource = "hermes-supervisor"
	EvidenceSourceArtifactDigest   EvidenceSource = "artifact-digest"
)

type EvidenceStatus string

const (
	EvidenceCompleted   EvidenceStatus = "completed"
	EvidenceFailed      EvidenceStatus = "failed"
	EvidenceIncomplete  EvidenceStatus = "incomplete"
	EvidenceUnknown     EvidenceStatus = "unknown"
	EvidenceDigestMatch EvidenceStatus = "digest_match"
)

type Evidence struct {
	ThreadID        string          `json:"thread_id"`
	RunID           string          `json:"run_id"`
	NativeSessionID string          `json:"native_session_id,omitempty"`
	Harness         mail.Harness    `json:"harness"`
	Source          EvidenceSource  `json:"source"`
	TurnID          string          `json:"turn_id,omitempty"`
	Status          EvidenceStatus  `json:"status"`
	Provider        string          `json:"provider,omitempty"`
	Model           string          `json:"model,omitempty"`
	ObservedAt      time.Time       `json:"observed_at"`
	StartedAt       *time.Time      `json:"started_at,omitempty"`
	EndedAt         *time.Time      `json:"ended_at,omitempty"`
	Artifact        *ArtifactDigest `json:"artifact,omitempty"`
}

type NativeEvidenceInput struct {
	NativeSessionID string         `json:"native_session_id"`
	Source          EvidenceSource `json:"source"`
	TurnID          string         `json:"turn_id"`
	Status          EvidenceStatus `json:"status"`
	Provider        string         `json:"provider"`
	Model           string         `json:"model"`
	ObservedAt      time.Time      `json:"observed_at"`
	StartedAt       *time.Time     `json:"started_at,omitempty"`
	EndedAt         *time.Time     `json:"ended_at,omitempty"`
}

func (store Store) RecordNativeEvidence(ctx context.Context, projectID, threadID, runID string, input NativeEvidenceInput) (Registry, Evidence, error) {
	if err := checkContext(ctx); err != nil {
		return Registry{}, Evidence{}, err
	}
	root, err := validateRoot(store.Root)
	if err != nil {
		return Registry{}, Evidence{}, err
	}
	if err := validateID(projectID, "project ID"); err != nil {
		return Registry{}, Evidence{}, err
	}
	if err := validateID(threadID, "thread ID"); err != nil {
		return Registry{}, Evidence{}, err
	}
	if err := validateRunID(runID, "evidence run ID"); err != nil {
		return Registry{}, Evidence{}, err
	}
	if runID == "none" {
		return Registry{}, Evidence{}, fmt.Errorf("%w: evidence run ID none is reserved", ErrInvalidInput)
	}
	if err := validateNativeEvidenceInput(input); err != nil {
		return Registry{}, Evidence{}, err
	}

	var recorded Evidence
	registry, err := updateRegistry(ctx, root, false, func(registry *Registry, rootFD int) error {
		project, thread, err := registryThread(registry, projectID, threadID)
		if err != nil {
			return err
		}
		binding, err := currentEvidenceBinding(rootFD, projectID, *thread, runID, input.NativeSessionID)
		if err != nil {
			return err
		}
		if !nativeSourceMatchesHarness(input.Source, binding.Harness) {
			return fmt.Errorf("%w: evidence source does not match current harness", ErrConflict)
		}
		evidence := Evidence{
			ThreadID: threadID, RunID: runID, NativeSessionID: binding.NativeSessionID,
			Harness: binding.Harness, Source: input.Source, TurnID: input.TurnID,
			Status: input.Status, Provider: input.Provider, Model: input.Model,
			ObservedAt: input.ObservedAt.UTC(), StartedAt: input.StartedAt, EndedAt: input.EndedAt,
		}
		if err := validateEvidenceRecord(evidence); err != nil {
			return fmt.Errorf("%w: evidence: %v", ErrInvalidInput, err)
		}
		return appendEvidence(project, evidence, &recorded)
	})
	if err != nil {
		return Registry{}, Evidence{}, err
	}
	return registry, recorded, nil
}

func (store Store) RecordVerifiedArtifact(ctx context.Context, projectID, threadID, runID, path, expectedSHA256 string) (Registry, Evidence, error) {
	if err := checkContext(ctx); err != nil {
		return Registry{}, Evidence{}, err
	}
	root, err := validateRoot(store.Root)
	if err != nil {
		return Registry{}, Evidence{}, err
	}
	if err := validateID(projectID, "project ID"); err != nil {
		return Registry{}, Evidence{}, err
	}
	if err := validateID(threadID, "thread ID"); err != nil {
		return Registry{}, Evidence{}, err
	}
	if err := validateRunID(runID, "evidence run ID"); err != nil {
		return Registry{}, Evidence{}, err
	}
	if runID == "none" {
		return Registry{}, Evidence{}, fmt.Errorf("%w: evidence run ID none is reserved", ErrInvalidInput)
	}

	var recorded Evidence
	registry, err := updateRegistry(ctx, root, false, func(registry *Registry, rootFD int) error {
		project, thread, err := registryThread(registry, projectID, threadID)
		if err != nil {
			return err
		}
		before, err := currentEvidenceBinding(rootFD, projectID, *thread, runID, "")
		if err != nil {
			return err
		}
		digest, err := VerifyArtifact(ctx, before.Worktree, path, expectedSHA256)
		if err != nil {
			return err
		}
		if err := checkContext(ctx); err != nil {
			return err
		}
		after, err := currentEvidenceBinding(rootFD, projectID, *thread, runID, "")
		if err != nil {
			return err
		}
		if !sameEvidenceBinding(before, after) {
			return fmt.Errorf("%w: current binding changed while hashing artifact", ErrConflict)
		}
		evidence := Evidence{
			ThreadID: threadID, RunID: runID, NativeSessionID: after.NativeSessionID,
			Harness: after.Harness, Source: EvidenceSourceArtifactDigest,
			Status: EvidenceDigestMatch, ObservedAt: time.Now().UTC(),
			Artifact: &digest,
		}
		if err := validateEvidenceRecord(evidence); err != nil {
			return fmt.Errorf("%w: evidence: %v", ErrInvalidInput, err)
		}
		return appendEvidence(project, evidence, &recorded)
	})
	if err != nil {
		return Registry{}, Evidence{}, err
	}
	return registry, recorded, nil
}

func validateNativeEvidenceInput(input NativeEvidenceInput) error {
	if input.NativeSessionID == "" {
		return fmt.Errorf("%w: native session ID is required", ErrInvalidInput)
	}
	if err := validateRunID(input.NativeSessionID, "native session ID"); err != nil {
		return err
	}
	if !isNativeEvidenceSource(input.Source) {
		return fmt.Errorf("%w: invalid native evidence source", ErrInvalidInput)
	}
	if !isNativeEvidenceStatus(input.Status) {
		return fmt.Errorf("%w: invalid native evidence status", ErrInvalidInput)
	}
	if err := validateRunID(input.TurnID, "evidence turn ID"); err != nil {
		return err
	}
	if err := validateEvidenceText(input.Provider, "evidence provider"); err != nil {
		return err
	}
	if err := validateEvidenceText(input.Model, "evidence model"); err != nil {
		return err
	}
	if input.ObservedAt.IsZero() {
		return fmt.Errorf("%w: evidence observed time is required", ErrInvalidInput)
	}
	if err := validateEvidenceTiming(input.StartedAt, input.EndedAt, input.ObservedAt, "native evidence"); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	return nil
}

func currentEvidenceBinding(rootFD int, projectID string, thread Thread, runID, observedNativeSessionID string) (Binding, error) {
	if thread.ActiveRunID != runID {
		return Binding{}, fmt.Errorf("%w: run is not the thread's active run", ErrConflict)
	}
	activeIndex := activeBindingIndex(thread)
	if activeIndex < 0 {
		return Binding{}, fmt.Errorf("%w: active binding is missing", ErrInvalidState)
	}
	active := thread.Runs[activeIndex]
	roster, err := readRoster(rootFD, projectID)
	if err != nil {
		return Binding{}, err
	}
	worker, found := roster.byName[active.Member]
	if !roster.present || !found || worker.RunID != active.RunID || !roleMatches(thread.Role, worker) {
		return Binding{}, fmt.Errorf("%w: active member is not current in roster", ErrConflict)
	}
	candidate := bindingFromWorker(worker)
	if !sameBindingIdentity(active, candidate) || active.NativeSessionID != candidate.NativeSessionID {
		return Binding{}, fmt.Errorf("%w: active binding is stale", ErrConflict)
	}
	if observedNativeSessionID != "" && observedNativeSessionID != active.NativeSessionID {
		return Binding{}, fmt.Errorf("%w: observed native session does not match active binding", ErrConflict)
	}
	return active, nil
}

func sameEvidenceBinding(left, right Binding) bool {
	return sameBindingIdentity(left, right) && left.NativeSessionID == right.NativeSessionID
}

func appendEvidence(project *Project, candidate Evidence, recorded *Evidence) error {
	for index, existing := range project.Evidence {
		if !sameEvidenceIdentity(existing, candidate) {
			continue
		}
		if !sameEvidenceOutcome(existing, candidate) {
			return fmt.Errorf("%w: evidence identity already has a different result", ErrConflict)
		}
		enriched := existing
		changed := false
		if err := enrichEvidenceText(&enriched.Provider, candidate.Provider, "provider"); err != nil {
			return err
		}
		if enriched.Provider != existing.Provider {
			changed = true
		}
		if err := enrichEvidenceText(&enriched.Model, candidate.Model, "model"); err != nil {
			return err
		}
		if enriched.Model != existing.Model {
			changed = true
		}
		if existing.StartedAt == nil && candidate.StartedAt != nil {
			startedAt := *candidate.StartedAt
			endedAt := *candidate.EndedAt
			enriched.StartedAt = &startedAt
			enriched.EndedAt = &endedAt
			changed = true
		} else if existing.StartedAt != nil && candidate.StartedAt != nil && !sameEvidenceTiming(existing, candidate) {
			return fmt.Errorf("%w: evidence identity already has different timing", ErrConflict)
		}
		if changed {
			project.Evidence[index] = enriched
			*recorded = enriched
			return nil
		}
		*recorded = existing
		return errNoChange
	}
	if len(project.Evidence) >= maxEvidenceRecords {
		return fmt.Errorf("%w: evidence history exceeds %d records", ErrInvalidInput, maxEvidenceRecords)
	}
	project.Evidence = append(project.Evidence, candidate)
	*recorded = candidate
	return nil
}

func sameEvidenceIdentity(left, right Evidence) bool {
	if left.ThreadID != right.ThreadID || left.RunID != right.RunID || left.NativeSessionID != right.NativeSessionID || left.Source != right.Source {
		return false
	}
	if left.Source == EvidenceSourceArtifactDigest {
		return left.Artifact != nil && right.Artifact != nil && left.Artifact.RelativePath == right.Artifact.RelativePath
	}
	return left.TurnID == right.TurnID
}

func sameEvidenceOutcome(left, right Evidence) bool {
	if !sameEvidenceIdentity(left, right) || left.Harness != right.Harness || left.Status != right.Status {
		return false
	}
	if (left.Artifact == nil) != (right.Artifact == nil) {
		return false
	}
	if left.Artifact != nil && *left.Artifact != *right.Artifact {
		return false
	}
	return true
}

func enrichEvidenceText(existing *string, candidate, label string) error {
	if *existing != "" && candidate != "" && *existing != candidate {
		return fmt.Errorf("%w: evidence identity already has different %s", ErrConflict, label)
	}
	if *existing == "" && candidate != "" {
		*existing = candidate
	}
	return nil
}

func sameEvidenceTiming(left, right Evidence) bool {
	return left.StartedAt != nil && right.StartedAt != nil && left.EndedAt != nil && right.EndedAt != nil && left.StartedAt.Equal(*right.StartedAt) && left.EndedAt.Equal(*right.EndedAt)
}

func validateEvidence(project Project) error {
	if len(project.Evidence) > maxEvidenceRecords {
		return fmt.Errorf("evidence exceeds %d records", maxEvidenceRecords)
	}
	threads := make(map[string]Thread, len(project.Threads))
	for _, thread := range project.Threads {
		threads[thread.ID] = thread
	}
	for index, evidence := range project.Evidence {
		if err := validateEvidenceRecord(evidence); err != nil {
			return fmt.Errorf("record %d: %v", index, err)
		}
		thread, exists := threads[evidence.ThreadID]
		if !exists {
			return fmt.Errorf("record %d names an unknown thread", index)
		}
		bindingFound := false
		for _, binding := range thread.Runs {
			if binding.RunID != evidence.RunID {
				continue
			}
			bindingFound = true
			if binding.NativeSessionID != evidence.NativeSessionID && evidence.NativeSessionID != "" {
				return fmt.Errorf("record %d native session differs from its run binding", index)
			}
			if binding.Harness != evidence.Harness {
				return fmt.Errorf("record %d harness differs from its run binding", index)
			}
			break
		}
		if !bindingFound {
			return fmt.Errorf("record %d names a run outside thread history", index)
		}
		for previousIndex := 0; previousIndex < index; previousIndex++ {
			if sameEvidenceIdentity(project.Evidence[previousIndex], evidence) {
				return fmt.Errorf("record %d duplicates evidence identity", index)
			}
		}
	}
	return nil
}

func validateEvidenceRecord(evidence Evidence) error {
	if err := validateStateID(evidence.ThreadID, "evidence thread ID"); err != nil {
		return err
	}
	if err := validateRunID(evidence.RunID, "evidence run ID"); err != nil {
		return err
	}
	if evidence.RunID == "none" {
		return fmt.Errorf("evidence run ID none is reserved")
	}
	if err := validateHarness(evidence.Harness, "evidence harness"); err != nil {
		return err
	}
	if evidence.ObservedAt.IsZero() {
		return fmt.Errorf("evidence has no observed time")
	}
	switch evidence.Source {
	case EvidenceSourceCodexThreadRead, EvidenceSourcePiInbox, EvidenceSourceHermesSupervisor:
		if !nativeSourceMatchesHarness(evidence.Source, evidence.Harness) {
			return fmt.Errorf("native evidence source does not match harness")
		}
		if !isNativeEvidenceStatus(evidence.Status) {
			return fmt.Errorf("native evidence has an invalid status")
		}
		if evidence.NativeSessionID == "" {
			return fmt.Errorf("native evidence has no native session ID")
		}
		if err := validateRunID(evidence.NativeSessionID, "evidence native session ID"); err != nil {
			return err
		}
		if err := validateRunID(evidence.TurnID, "evidence turn ID"); err != nil {
			return err
		}
		if err := validateEvidenceText(evidence.Provider, "evidence provider"); err != nil {
			return err
		}
		if err := validateEvidenceText(evidence.Model, "evidence model"); err != nil {
			return err
		}
		if err := validateEvidenceTiming(evidence.StartedAt, evidence.EndedAt, evidence.ObservedAt, "native evidence"); err != nil {
			return err
		}
		if evidence.Artifact != nil {
			return fmt.Errorf("native evidence carries an artifact")
		}
	case EvidenceSourceArtifactDigest:
		if evidence.Status != EvidenceDigestMatch {
			return fmt.Errorf("artifact evidence has an invalid status")
		}
		if evidence.TurnID != "" || evidence.Provider != "" || evidence.Model != "" {
			return fmt.Errorf("artifact evidence carries native fields")
		}
		if evidence.StartedAt != nil || evidence.EndedAt != nil {
			return fmt.Errorf("artifact evidence carries native timing")
		}
		if evidence.Artifact == nil {
			return fmt.Errorf("artifact evidence has no digest")
		}
		if err := validateArtifactDigest(*evidence.Artifact); err != nil {
			return err
		}
	default:
		return fmt.Errorf("evidence has an unknown source %q", evidence.Source)
	}
	return nil
}

func validateEvidenceTiming(startedAt, endedAt *time.Time, observedAt time.Time, label string) error {
	if (startedAt == nil) != (endedAt == nil) {
		return fmt.Errorf("%s timing must include both started and ended times", label)
	}
	if startedAt == nil {
		return nil
	}
	if startedAt.IsZero() || endedAt.IsZero() {
		return fmt.Errorf("%s timing cannot contain a zero time", label)
	}
	if endedAt.Before(*startedAt) {
		return fmt.Errorf("%s timing ends before it starts", label)
	}
	if startedAt.After(observedAt) || endedAt.After(observedAt) {
		return fmt.Errorf("%s timing is after its observed time", label)
	}
	return nil
}

func validateArtifactDigest(digest ArtifactDigest) error {
	if digest.RelativePath == "" || digest.RelativePath == "." || !utf8.ValidString(digest.RelativePath) || strings.IndexByte(digest.RelativePath, 0) >= 0 || filepath.IsAbs(digest.RelativePath) || filepath.Clean(digest.RelativePath) != digest.RelativePath || hasParentComponent(digest.RelativePath) {
		return fmt.Errorf("artifact digest has an invalid relative path")
	}
	if len(digest.RelativePath) > maxEvidenceArtifactPath {
		return fmt.Errorf("artifact digest path exceeds %d bytes", maxEvidenceArtifactPath)
	}
	if digest.Size < 0 || digest.Size > maxArtifactBytes {
		return fmt.Errorf("artifact digest has an invalid size")
	}
	if !validDigest(digest.SHA256) {
		return fmt.Errorf("artifact digest has an invalid sha256")
	}
	return nil
}

func validateEvidenceText(value, label string) error {
	if len(value) > maxEvidenceTextBytes {
		return fmt.Errorf("%w: %s exceeds %d bytes", ErrInvalidInput, label, maxEvidenceTextBytes)
	}
	if !utf8.ValidString(value) {
		return fmt.Errorf("%w: %s is not valid UTF-8", ErrInvalidInput, label)
	}
	for _, runeValue := range value {
		if unicode.IsControl(runeValue) || unicode.In(runeValue, unicode.Cf) {
			return fmt.Errorf("%w: %s contains a control character", ErrInvalidInput, label)
		}
	}
	return nil
}

func isNativeEvidenceSource(source EvidenceSource) bool {
	switch source {
	case EvidenceSourceCodexThreadRead, EvidenceSourcePiInbox, EvidenceSourceHermesSupervisor:
		return true
	default:
		return false
	}
}

func nativeSourceMatchesHarness(source EvidenceSource, harness mail.Harness) bool {
	switch source {
	case EvidenceSourceCodexThreadRead:
		return harness == mail.HarnessCodex
	case EvidenceSourcePiInbox:
		return harness == mail.HarnessPi
	case EvidenceSourceHermesSupervisor:
		return harness == mail.HarnessHermes
	default:
		return false
	}
}

func isNativeEvidenceStatus(status EvidenceStatus) bool {
	switch status {
	case EvidenceCompleted, EvidenceFailed, EvidenceIncomplete, EvidenceUnknown:
		return true
	default:
		return false
	}
}
