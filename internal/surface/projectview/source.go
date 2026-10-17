// Package projectview adapts project-owned, read-only observations to Desk's
// runview renderer. Transport, native turns, and checked artifact bytes remain
// distinct evidence; none establishes overall correctness or process liveness.
package projectview

import (
	"context"
	"fmt"
	"time"

	"github.com/cameronsjo/forgectl/internal/runview"
	"github.com/cameronsjo/forgectl/internal/surface/project"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

const (
	projectStateStep = "project-state"
	mailEventStep    = "mail-transport"
	nativeTurnStep   = "native-turn"
	artifactStep     = "artifact-bytes"
)

type Source struct {
	store     project.Store
	projectID string
	name      string
}

// NewSource returns a source with one aggregate project run and one exact run
// per logical thread. Construction performs no filesystem I/O.
func NewSource(store project.Store, projectID string) runview.Source {
	return &Source{store: store, projectID: projectID, name: "project/" + termsafe.SafeLineMax(projectID, 128)}
}

func (source *Source) Name() string { return source.name }

// Spec deliberately has no completion actions. Mail observations are not a
// model-turn or artifact result, even when a transport status is accepted.
func (source *Source) Spec(runview.RunRef) *runview.Spec { return &runview.Spec{} }

func (source *Source) List() ([]runview.RunRef, error) {
	timeline, err := source.store.MailTimeline(context.Background(), source.projectID)
	if err != nil {
		return nil, err
	}
	refs := make([]runview.RunRef, 0, len(timeline.ThreadSummaries)+1)
	refs = append(refs, source.ref(source.projectID, timeline.ObservedAt))
	for _, thread := range timeline.ThreadSummaries {
		refs = append(refs, source.ref(thread.ThreadID, timeline.ObservedAt))
	}
	return refs, nil
}

func (source *Source) ref(name string, updated time.Time) runview.RunRef {
	return runview.RunRef{Source: source.Name(), Name: name, Kind: runview.KindLog, Updated: updated, Live: runview.LiveUnknown}
}

func (source *Source) Load(ref runview.RunRef, _ *runview.Cursor) (runview.Delta, error) {
	if ref.Source != "" && ref.Source != source.Name() {
		return runview.Delta{}, fmt.Errorf("project view: source mismatch")
	}
	if ref.Kind != "" && ref.Kind != runview.KindLog {
		return runview.Delta{}, fmt.Errorf("project view: unsupported run kind")
	}
	timeline, err := source.store.MailTimeline(context.Background(), source.projectID)
	if err != nil {
		return runview.Delta{}, err
	}
	if ref.Name != source.projectID && !hasThread(timeline, ref.Name) {
		return runview.Delta{}, fmt.Errorf("project view: unknown thread %s", termsafe.SafeLineMax(ref.Name, 128))
	}
	delta := runview.Delta{
		Reset: true,
		Live:  runview.LiveUnknown,
		Defs: []runview.StepDef{
			{ID: projectStateStep, Note: "project state evidence"},
			{ID: mailEventStep, Note: "mail transport evidence"},
			{ID: nativeTurnStep, Note: "native terminal-turn evidence, not task correctness"},
			{ID: artifactStep, Note: "checked file bytes, not authorship or correctness"},
		},
		Dropped: timeline.Skipped,
		Partial: timeline.Partial,
		Held:    timeline.Held,
	}
	for _, thread := range timeline.ThreadSummaries {
		if ref.Name != source.projectID && thread.ThreadID != ref.Name {
			continue
		}
		event := stateEvent(thread)
		event.Fields = append(event.Fields, runview.Field{Key: "project_name", Value: safe(timeline.ProjectName)})
		event.Seq = len(delta.Events) + 1
		delta.Events = append(delta.Events, event)
	}
	for _, observation := range timeline.Entries {
		if ref.Name != source.projectID && !observationForThread(observation, ref.Name) {
			continue
		}
		event := eventForObservation(observation)
		event.Seq = len(delta.Events) + 1
		delta.Events = append(delta.Events, event)
	}
	for _, evidence := range timeline.Evidence {
		if ref.Name != source.projectID && evidence.ThreadID != ref.Name {
			continue
		}
		event := eventForEvidence(evidence, timeline.ThreadSummaries)
		event.Seq = len(delta.Events) + 1
		delta.Events = append(delta.Events, event)
	}
	if timeline.Skipped > 0 {
		delta.Partial = true
		delta.Err = fmt.Errorf("project mailbox contains %d malformed or skipped record(s)", timeline.Skipped)
	}
	return delta, nil
}

func observationForThread(observation project.MailObservation, threadID string) bool {
	return observation.FromThreadID == threadID || observation.ToThreadID == threadID
}

func hasThread(timeline project.MailTimeline, threadID string) bool {
	for _, thread := range timeline.ThreadSummaries {
		if thread.ThreadID == threadID {
			return true
		}
	}
	return false
}

func stateEvent(thread project.MailThreadSummary) runview.Event {
	event := runview.Event{
		Name: stateEventName(thread.Enrollment),
		Step: projectStateStep,
		Fields: []runview.Field{
			{Key: "thread_id", Value: safe(thread.ThreadID)},
			{Key: "thread_name", Value: safe(thread.Name)},
			{Key: "active_run_id", Value: safe(thread.ActiveRunID)},
			{Key: "role", Value: safe(string(thread.Role))},
			{Key: "enrollment", Value: safe(string(thread.Enrollment))},
			{Key: "native_session_known", Value: fmt.Sprintf("%t", thread.NativeSessionKnown)},
			{Key: "process_liveness", Value: safe(string(thread.ProcessLiveness))},
		},
	}
	if thread.Reason != "" {
		event.Fields = append(event.Fields, runview.Field{Key: "reason", Value: safe(thread.Reason)})
	}
	return event
}

func stateEventName(enrollment project.Enrollment) string {
	switch enrollment {
	case project.EnrollmentCurrent:
		return "PROJECT-THREAD-CURRENT"
	case project.EnrollmentStale:
		return "PROJECT-THREAD-STALE"
	default:
		return "PROJECT-THREAD-UNBOUND"
	}
}

func eventForObservation(observation project.MailObservation) runview.Event {
	event := runview.Event{
		Name: eventName(observation.Status),
		Time: observation.At,
		Step: mailEventStep,
		Fields: []runview.Field{
			{Key: "message_id", Value: safe(observation.MessageID)},
			{Key: "from", Value: safe(observation.From)},
			{Key: "to", Value: safe(observation.To)},
			{Key: "status", Value: safe(string(observation.Status))},
		},
	}
	if observation.FromRunID != "" {
		event.Fields = append(event.Fields, runview.Field{Key: "from_run_id", Value: safe(observation.FromRunID)})
	}
	if observation.ToRunID != "" {
		event.Fields = append(event.Fields, runview.Field{Key: "to_run_id", Value: safe(observation.ToRunID)})
	}
	return event
}

func eventName(status project.MailStatus) string {
	switch status {
	case project.MailWaiting:
		return "MAIL-WAITING"
	case project.MailTransportAccepted:
		return "MAIL-TRANSPORT-ACCEPTED"
	case project.MailDeliveryFailed:
		return "MAIL-DELIVERY-FAILED"
	default:
		return "MAIL-DELIVERY-UNKNOWN"
	}
}

func safe(value string) string { return termsafe.SafeLineMax(value, 256) }

func eventForEvidence(evidence project.Evidence, threads []project.MailThreadSummary) runview.Event {
	event := runview.Event{Time: evidence.ObservedAt, Step: nativeTurnStep, Name: "NATIVE-TURN-UNKNOWN"}
	current := false
	for _, thread := range threads {
		if thread.ThreadID == evidence.ThreadID && thread.ActiveRunID == evidence.RunID && thread.Enrollment == project.EnrollmentCurrent {
			current = true
		}
	}
	event.Fields = []runview.Field{
		{Key: "thread_id", Value: safe(evidence.ThreadID)},
		{Key: "run_id", Value: safe(evidence.RunID)},
		{Key: "native_session_id", Value: safe(evidence.NativeSessionID)},
		{Key: "source", Value: safe(string(evidence.Source))},
		{Key: "status", Value: safe(string(evidence.Status))},
		{Key: "currently_enrolled", Value: fmt.Sprintf("%t", current)},
	}
	if evidence.Source == project.EvidenceSourceArtifactDigest {
		event.Step = artifactStep
		event.Name = "ARTIFACT-BYTES-MATCHED"
		event.Fields = append(event.Fields,
			runview.Field{Key: "relative_path", Value: safe(evidence.Artifact.RelativePath)},
			runview.Field{Key: "sha256", Value: evidence.Artifact.SHA256},
			runview.Field{Key: "size", Value: fmt.Sprintf("%d", evidence.Artifact.Size)},
			runview.Field{Key: "scope", Value: "byte identity at verification time; not correctness or authorship"},
		)
		return event
	}
	switch evidence.Status {
	case project.EvidenceCompleted:
		event.Name = "NATIVE-TURN-COMPLETED"
	case project.EvidenceFailed:
		event.Name = "NATIVE-TURN-FAILED"
	case project.EvidenceIncomplete:
		event.Name = "NATIVE-TURN-INCOMPLETE"
	}
	event.Fields = append(event.Fields, runview.Field{Key: "turn_id", Value: safe(evidence.TurnID)}, runview.Field{Key: "scope", Value: "native turn outcome; not task correctness"})
	return event
}
