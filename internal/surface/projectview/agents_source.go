package projectview

import (
	"bytes"
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/cameronsjo/forgectl/internal/surface/activity"
	"github.com/cameronsjo/forgectl/internal/surface/mail"
	"github.com/cameronsjo/forgectl/internal/surface/project"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

type NativeActivity struct {
	NativeSessionID string
	Provider        string
	Model           string
	Activity        *activity.Snapshot
}

type ActivityObserver func(context.Context, mail.Worker) (NativeActivity, error)

type AgentsSource struct {
	store     project.Store
	projectID string
	observe   ActivityObserver
	mu        sync.Mutex
	cache     map[string]AgentRow
	request   time.Duration
	refresh   time.Duration
}

type agentResult struct {
	index  int
	native NativeActivity
	err    error
}

func NewAgentsSource(store project.Store, projectID string, observe ActivityObserver) *AgentsSource {
	return &AgentsSource{store: store, projectID: projectID, observe: observe, cache: make(map[string]AgentRow), request: 2 * time.Second, refresh: 5 * time.Second}
}

func (source *AgentsSource) Load(ctx context.Context) (AgentsSnapshot, error) {
	source.mu.Lock()
	defer source.mu.Unlock()
	_, record, observations, err := source.store.Inspect(ctx, source.projectID)
	if err != nil {
		return AgentsSnapshot{}, err
	}
	snapshot := AgentsSnapshot{Version: AgentsVersion, ProjectID: record.ID, ProjectName: safe(record.Name), ObservedAt: time.Now().UTC(), Rows: []AgentRow{}, TotalThreads: len(record.Threads)}
	threads := record.Threads
	if len(threads) > MaxAgentRows {
		snapshot.DroppedThreads = len(threads) - MaxAgentRows
		snapshot.Partial, snapshot.Reason = true, "thread-limit"
		threads = threads[:MaxAgentRows]
	}
	bindings := make([]project.Binding, len(threads))
	workers := make(map[int]mail.Worker)
	for index, thread := range threads {
		observation := agentObservation(observations, thread.ID)
		binding := agentBinding(thread)
		bindings[index] = binding
		row := AgentRow{ThreadID: thread.ID, Name: safe(thread.Name), Role: string(thread.Role), RunID: thread.ActiveRunID, Harness: string(binding.Harness), Enrollment: string(observation.Enrollment), State: "unknown", ObservedAt: snapshot.ObservedAt, Spans: []AgentSpan{}}
		if cached, exists := source.cache[thread.ID]; exists {
			row.Spans = freezeAgentSpans(cached)
		}
		addAgentEvidence(&row, record.Evidence)
		switch observation.Enrollment {
		case project.EnrollmentUnbound:
			row.State, row.Reason = "unbound", "no-bound-run"
		case project.EnrollmentStale:
			row.State, row.Reason, row.Partial = "stale", "stale-binding", true
		case project.EnrollmentCurrent:
			if binding.NativeSessionID == "" || source.observe == nil {
				row.Reason, row.Partial = "native-observation-unavailable", true
			} else {
				workers[index] = mail.Worker{Name: binding.Member, Harness: binding.Harness, Worktree: binding.Worktree, Socket: binding.Socket, ThreadID: binding.NativeSessionID, RunID: binding.RunID, PaneID: binding.PaneID, Coordinator: thread.Role == project.RoleCoordinator}
			}
		default:
			row.Reason, row.Partial = "enrollment-unavailable", true
		}
		snapshot.Rows = append(snapshot.Rows, row)
	}
	results := source.collect(ctx, workers)
	_, current, currentObservations, err := source.store.Inspect(ctx, source.projectID)
	if err != nil {
		return AgentsSnapshot{}, err
	}
	if len(current.Threads) != len(record.Threads) {
		snapshot.Partial, snapshot.Reason = true, "project-changed"
	}
	for index := range snapshot.Rows {
		row := &snapshot.Rows[index]
		currentThread := agentThread(current.Threads, row.ThreadID)
		if currentThread == nil || agentBinding(*currentThread) != bindings[index] || string(agentObservation(currentObservations, row.ThreadID).Enrollment) != row.Enrollment {
			row.State, row.Enrollment, row.Reason, row.Partial = "stale", string(project.EnrollmentStale), "binding-changed", true
		} else if _, requested := workers[index]; requested {
			if result, exists := results[index]; !exists || result.err != nil {
				row.Reason, row.Partial = "native-observation-unavailable", true
			} else {
				applyAgentNative(row, bindings[index], result.native)
			}
		}
		if row.Partial {
			snapshot.Partial = true
		}
	}
	snapshot.ObservedAt = time.Now().UTC()
	boundAgentSnapshot(&snapshot)
	source.cache = make(map[string]AgentRow, len(snapshot.Rows))
	for _, row := range snapshot.Rows {
		source.cache[row.ThreadID] = cloneAgentRow(row)
	}
	return snapshot, nil
}

func (source *AgentsSource) collect(ctx context.Context, workers map[int]mail.Worker) map[int]agentResult {
	results := make(map[int]agentResult, len(workers))
	if len(workers) == 0 {
		return results
	}
	refresh, cancel := context.WithTimeout(ctx, source.refresh)
	defer cancel()
	jobs := make(chan int, len(workers))
	completed := make(chan agentResult, len(workers))
	for index := range workers {
		jobs <- index
	}
	close(jobs)
	for range min(4, len(workers)) {
		go func() {
			for index := range jobs {
				if refresh.Err() != nil {
					return
				}
				request, requestCancel := context.WithTimeout(refresh, source.request)
				native, err := source.observe(request, workers[index])
				if request.Err() != nil {
					err = request.Err()
				}
				requestCancel()
				completed <- agentResult{index: index, native: native, err: err}
			}
		}()
	}
	for len(results) < len(workers) {
		select {
		case result := <-completed:
			results[result.index] = result
		case <-refresh.Done():
			return results
		}
	}
	return results
}

func applyAgentNative(row *AgentRow, binding project.Binding, native NativeActivity) {
	if native.NativeSessionID != binding.NativeSessionID {
		row.Reason, row.Partial = "native-identity-changed", true
		return
	}
	row.Provider, row.Model = safe(native.Provider), safe(native.Model)
	if native.Activity == nil {
		row.Reason, row.Partial = "activity-unavailable", true
		return
	}
	snapshot := activity.Clone(*native.Activity)
	now := time.Now().UTC()
	if activity.Validate(snapshot) != nil || !agentSourceMatches(snapshot.Source, binding.Harness) || snapshot.ObservedAt.After(now.Add(time.Second)) || now.Sub(snapshot.ObservedAt) > 30*time.Second {
		row.Reason, row.Partial = "invalid-native-activity", true
		return
	}
	candidate := cloneAgentRow(*row)
	candidate.State = "idle"
	for _, span := range snapshot.Spans {
		item := AgentSpan{RunID: binding.RunID, NativeSessionID: binding.NativeSessionID, Source: snapshot.Source, Span: span}
		if err := mergeAgentSpan(&candidate, item); err != nil {
			row.Reason, row.Partial = "conflicting-native-activity", true
			return
		}
		if span.State == activity.Running {
			candidate.State = "active"
		}
	}
	candidate.ObservedAt, candidate.Coverage, candidate.Since = snapshot.ObservedAt, snapshot.Coverage, snapshot.Since
	candidate.Partial = candidate.Partial || snapshot.Partial
	if snapshot.Partial {
		candidate.Reason = "bounded-native-activity"
		if candidate.State != "active" {
			candidate.State = "unknown"
		}
	}
	*row = candidate
}

func freezeAgentSpans(row AgentRow) []AgentSpan {
	cloned := cloneAgentRow(row)
	for index := range cloned.Spans {
		item := &cloned.Spans[index]
		if item.Span.EndedAt == nil {
			item.Span.State = activity.Unknown
			if item.ObservedThrough == nil {
				observed := row.ObservedAt
				item.ObservedThrough = &observed
			}
		}
	}
	return cloned.Spans
}

func cloneAgentRow(row AgentRow) AgentRow {
	cloned := row
	if row.Since != nil {
		since := *row.Since
		cloned.Since = &since
	}
	cloned.Spans = make([]AgentSpan, len(row.Spans))
	cloned.Points = append([]AgentPoint(nil), row.Points...)
	for index, item := range row.Spans {
		cloned.Spans[index] = item
		if item.Span.EndedAt != nil {
			ended := *item.Span.EndedAt
			cloned.Spans[index].Span.EndedAt = &ended
		}
		if item.ObservedThrough != nil {
			observed := *item.ObservedThrough
			cloned.Spans[index].ObservedThrough = &observed
		}
	}
	return cloned
}

func mergeAgentSpan(row *AgentRow, incoming AgentSpan) error {
	for index, existing := range row.Spans {
		if existing.RunID != incoming.RunID || existing.NativeSessionID != incoming.NativeSessionID || existing.Source != incoming.Source || (existing.Span.ID != incoming.Span.ID && (existing.Span.TurnID == "" || existing.Span.TurnID != incoming.Span.TurnID)) {
			continue
		}
		old, next := existing.Span, incoming.Span
		if !old.StartedAt.Equal(next.StartedAt) || (old.TurnID != "" && next.TurnID != "" && old.TurnID != next.TurnID) || (old.Provider != "" && next.Provider != "" && (old.Provider != next.Provider || old.Model != next.Model)) {
			return errors.New("activity interval identity changed")
		}
		if old.EndedAt != nil && (next.EndedAt == nil || !old.EndedAt.Equal(*next.EndedAt) || old.State != next.State && old.State != activity.Unknown) {
			return errors.New("settled activity interval changed")
		}
		if incoming.Span.Provider == "" {
			incoming.Span.Provider, incoming.Span.Model = old.Provider, old.Model
		}
		if incoming.Span.TurnID == "" {
			incoming.Span.TurnID = old.TurnID
		}
		row.Spans[index] = incoming
		return nil
	}
	row.Spans = append(row.Spans, incoming)
	return nil
}

func addAgentEvidence(row *AgentRow, records []project.Evidence) {
	for _, evidence := range records {
		if evidence.ThreadID != row.ThreadID || evidence.Source == project.EvidenceSourceArtifactDigest {
			continue
		}
		source := agentEvidenceSource(evidence.Source)
		state := agentEvidenceState(evidence.Status)
		if evidence.StartedAt == nil || evidence.EndedAt == nil {
			row.Points = append(row.Points, AgentPoint{RunID: evidence.RunID, TurnID: evidence.TurnID, At: evidence.ObservedAt, State: state, Source: source})
			continue
		}
		span := AgentSpan{RunID: evidence.RunID, NativeSessionID: evidence.NativeSessionID, Source: source, Span: activity.Span{ID: evidence.TurnID, TurnID: evidence.TurnID, StartedAt: *evidence.StartedAt, EndedAt: evidence.EndedAt, State: state, Provider: evidence.Provider, Model: evidence.Model}}
		if mergeAgentSpan(row, span) != nil {
			row.Partial, row.Reason = true, "conflicting-persisted-activity"
		}
	}
}

func boundAgentSnapshot(snapshot *AgentsSnapshot) {
	remaining := MaxAgentSpans
	for index := range snapshot.Rows {
		row := &snapshot.Rows[index]
		sort.SliceStable(row.Spans, func(left, right int) bool {
			return row.Spans[left].Span.StartedAt.After(row.Spans[right].Span.StartedAt)
		})
		keep := min(len(row.Spans), activity.MaxSpans, remaining)
		if keep < len(row.Spans) {
			snapshot.DroppedSpans += len(row.Spans) - keep
			row.Partial, row.Reason = true, "span-limit"
			row.Spans = row.Spans[:keep]
		}
		remaining -= keep
		points := row.Points[:0]
		for _, point := range row.Points {
			matched := false
			for _, span := range row.Spans {
				if span.RunID == point.RunID && span.Span.TurnID == point.TurnID && span.Source == point.Source {
					matched = true
					break
				}
			}
			if !matched {
				points = append(points, point)
			}
		}
		row.Points = points
		sort.SliceStable(row.Points, func(left, right int) bool { return row.Points[left].At.After(row.Points[right].At) })
		if len(row.Points) > remaining {
			snapshot.DroppedSpans += len(row.Points) - remaining
			row.Partial, row.Reason = true, "span-limit"
			row.Points = row.Points[:remaining]
		}
		remaining -= len(row.Points)
		if row.Partial {
			snapshot.Partial = true
		}
	}
	for len(snapshot.Rows) > 0 {
		var encoded bytes.Buffer
		if err := termsafe.JSONEncoder(&encoded).Encode(snapshot); err == nil && encoded.Len() <= MaxAgentsJSONBytes {
			break
		}
		removed := snapshot.Rows[len(snapshot.Rows)-1]
		snapshot.Rows = snapshot.Rows[:len(snapshot.Rows)-1]
		snapshot.DroppedThreads++
		snapshot.DroppedSpans += len(removed.Spans) + len(removed.Points)
		snapshot.Partial, snapshot.Reason = true, "encoded-size-limit"
	}
	if snapshot.Partial && snapshot.Reason == "" {
		snapshot.Reason = "partial-source-coverage"
	}
}

func agentObservation(observations []project.Observation, threadID string) project.Observation {
	for _, observation := range observations {
		if observation.ThreadID == threadID {
			return observation
		}
	}
	return project.Observation{}
}

func agentBinding(thread project.Thread) project.Binding {
	for _, binding := range thread.Runs {
		if binding.RunID == thread.ActiveRunID {
			return binding
		}
	}
	return project.Binding{}
}

func agentThread(threads []project.Thread, threadID string) *project.Thread {
	for index := range threads {
		if threads[index].ID == threadID {
			return &threads[index]
		}
	}
	return nil
}

func agentSourceMatches(source activity.Source, harness mail.Harness) bool {
	return (source == activity.SourceCodex && harness == mail.HarnessCodex) || (source == activity.SourcePi && harness == mail.HarnessPi) || (source == activity.SourceHermes && harness == mail.HarnessHermes)
}

func agentEvidenceSource(source project.EvidenceSource) activity.Source {
	switch source {
	case project.EvidenceSourceCodexThreadRead:
		return activity.SourceCodex
	case project.EvidenceSourcePiInbox:
		return activity.SourcePi
	default:
		return activity.SourceHermes
	}
}

func agentEvidenceState(state project.EvidenceStatus) activity.State {
	switch state {
	case project.EvidenceCompleted:
		return activity.Finished
	case project.EvidenceFailed:
		return activity.Failed
	case project.EvidenceIncomplete:
		return activity.Incomplete
	default:
		return activity.Unknown
	}
}
