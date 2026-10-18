//go:build unix

package projectview

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/surface/activity"
	"github.com/cameronsjo/forgectl/internal/surface/mail"
	"github.com/cameronsjo/forgectl/internal/surface/project"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

func TestAgentsSourceReadsNativeIntervalsWithoutWrites(test *testing.T) {
	store, record := sourceProject(test)
	worker := bindAgent(test, store, record, record.CoordinatorThreadID, "lead")
	now := time.Now().UTC()
	ended := now.Add(-time.Minute)
	observation := NativeActivity{NativeSessionID: worker.ThreadID, Provider: "openai", Model: "current-model", Activity: &activity.Snapshot{Version: activity.Version, Source: activity.SourceCodex, Coverage: activity.CoverageNativeHistory, ObservedAt: now, Spans: []activity.Span{
		{ID: "finished", TurnID: "finished", StartedAt: now.Add(-2 * time.Minute), EndedAt: &ended, State: activity.Finished},
		{ID: "active", StartedAt: now.Add(-30 * time.Second), State: activity.Running},
	}}}
	source := NewAgentsSource(store, record.ID, func(_ context.Context, input mail.Worker) (NativeActivity, error) {
		if input.RunID != worker.RunID || input.ThreadID != worker.ThreadID {
			test.Error("native inspection received the wrong identity")
		}
		return observation, nil
	})
	before := snapshotTree(test, store.Root)
	snapshot, err := source.Load(test.Context())
	if err != nil || len(snapshot.Rows) != 2 || snapshot.Rows[0].State != "active" || snapshot.Rows[0].Model != "current-model" || len(snapshot.Rows[0].Spans) != 2 || snapshot.Rows[1].State != "unbound" {
		test.Fatalf("snapshot=%+v err=%v", snapshot, err)
	}
	if snapshot.Rows[0].Spans[0].Span.Model != "" || snapshot.Rows[0].Spans[1].Span.Model != "" {
		test.Fatal("current model was invented as a historical producer")
	}
	if before != snapshotTree(test, store.Root) {
		test.Fatal("read-only snapshot rewrote registry")
	}
	snapshot.Rows[0].Spans[0].Span.ID = "caller-mutated"
	source.observe = func(context.Context, mail.Worker) (NativeActivity, error) {
		return NativeActivity{}, errors.New("secret-prompt-error")
	}
	failed, err := source.Load(test.Context())
	if err != nil || failed.Rows[0].State != "unknown" || !failed.Partial || failed.Rows[0].Model != "" {
		test.Fatalf("failed poll=%+v err=%v", failed, err)
	}
	for _, span := range failed.Rows[0].Spans {
		if span.Span.ID == "active" && (span.Span.State != activity.Unknown || span.ObservedThrough == nil || !span.ObservedThrough.Equal(now) || span.Span.EndedAt != nil) {
			test.Fatalf("lost observation invented an end or retained active: %+v", span)
		}
		if span.Span.ID == "finished" && (span.Span.State != activity.Finished || span.Span.EndedAt == nil || !span.Span.EndedAt.Equal(ended)) {
			test.Fatalf("finished interval changed: %+v", span)
		}
		if span.Span.ID == "caller-mutated" {
			test.Fatal("caller mutation reached source cache")
		}
	}
	var encoded bytes.Buffer
	if err := termsafe.JSONEncoder(&encoded).Encode(failed); err != nil {
		test.Fatal(err)
	}
	if strings.Contains(encoded.String(), "secret-prompt-error") {
		test.Fatal("native error leaked into snapshot")
	}
}

func TestAgentsSourceRejectsReplacedBindingAndMalformedActivity(test *testing.T) {
	for _, scenario := range []string{"binding", "session", "source", "reversed", "future", "duplicate", "missing"} {
		test.Run(scenario, func(test *testing.T) {
			store, record := sourceProject(test)
			worker := bindAgent(test, store, record, record.CoordinatorThreadID, "lead")
			source := NewAgentsSource(store, record.ID, func(context.Context, mail.Worker) (NativeActivity, error) {
				now := time.Now().UTC()
				observation := NativeActivity{NativeSessionID: worker.ThreadID, Activity: &activity.Snapshot{Version: activity.Version, Source: activity.SourceCodex, Coverage: activity.CoverageNativeHistory, ObservedAt: now, Spans: []activity.Span{{ID: "active", StartedAt: now.Add(-time.Minute), State: activity.Running}}}}
				switch scenario {
				case "binding":
					replacement := worker
					replacement.RunID, replacement.ThreadID = "replacement-run", "replacement-native"
					if err := (mail.FileRoster{Dir: filepath.Join(store.Root, record.ID, "team")}).Put(replacement); err != nil {
						test.Error(err)
					}
				case "session":
					observation.NativeSessionID = "other-native"
				case "source":
					observation.Activity.Source = activity.SourcePi
				case "reversed":
					ended := now.Add(-2 * time.Minute)
					observation.Activity.Spans[0].EndedAt, observation.Activity.Spans[0].State = &ended, activity.Finished
				case "future":
					observation.Activity.ObservedAt = now.Add(time.Hour)
				case "duplicate":
					observation.Activity.Spans = append(observation.Activity.Spans, observation.Activity.Spans[0])
				case "missing":
					observation.Activity = nil
				}
				return observation, nil
			})
			snapshot, err := source.Load(test.Context())
			if err != nil || !snapshot.Partial || len(snapshot.Rows[0].Spans) != 0 || snapshot.Rows[0].State == "active" || snapshot.Rows[0].State == "idle" {
				test.Fatalf("bad observation became healthy: snapshot=%+v err=%v", snapshot, err)
			}
		})
	}
}

func TestAgentsSourcePersistsOnlyExplicitEvidenceAndKeepsPointsDistinct(test *testing.T) {
	store, record := sourceProject(test)
	worker := bindAgent(test, store, record, record.CoordinatorThreadID, "lead")
	now := time.Now().UTC()
	started, ended := now.Add(-2*time.Minute), now.Add(-time.Minute)
	for _, input := range []project.NativeEvidenceInput{
		{NativeSessionID: worker.ThreadID, Source: project.EvidenceSourceCodexThreadRead, TurnID: "timed", Status: project.EvidenceCompleted, ObservedAt: now, StartedAt: &started, EndedAt: &ended, Provider: "openai", Model: "captured-model"},
		{NativeSessionID: worker.ThreadID, Source: project.EvidenceSourceCodexThreadRead, TurnID: "untimed", Status: project.EvidenceFailed, ObservedAt: now},
	} {
		if _, _, err := store.RecordNativeEvidence(test.Context(), record.ID, record.CoordinatorThreadID, worker.RunID, input); err != nil {
			test.Fatal(err)
		}
	}
	before := snapshotTree(test, store.Root)
	snapshot, err := NewAgentsSource(store, record.ID, nil).Load(test.Context())
	if err != nil || len(snapshot.Rows[0].Spans) != 1 || len(snapshot.Rows[0].Points) != 1 || snapshot.Rows[0].Spans[0].Span.Model != "captured-model" || snapshot.Rows[0].Points[0].TurnID != "untimed" {
		test.Fatalf("explicit evidence=%+v err=%v", snapshot, err)
	}
	if before != snapshotTree(test, store.Root) {
		test.Fatal("read-only source changed persisted evidence")
	}
	if snapshot.Rows[0].State != "unknown" {
		test.Fatal("persisted completion established live state")
	}
}

func TestAgentsSourceConcurrencyDeadlineAndAggregateBounds(test *testing.T) {
	store, record := sourceProject(test)
	for index := 0; index < 10; index++ {
		_, thread, err := store.CreateThread(test.Context(), record.ID, fmt.Sprintf("worker-%d", index), "")
		if err != nil {
			test.Fatal(err)
		}
		bindAgent(test, store, record, thread.ID, fmt.Sprintf("worker-%d", index))
	}
	var concurrent, maximum atomic.Int32
	source := NewAgentsSource(store, record.ID, func(ctx context.Context, _ mail.Worker) (NativeActivity, error) {
		count := concurrent.Add(1)
		defer concurrent.Add(-1)
		for previous := maximum.Load(); count > previous; previous = maximum.Load() {
			if maximum.CompareAndSwap(previous, count) {
				break
			}
		}
		<-ctx.Done()
		return NativeActivity{}, ctx.Err()
	})
	source.request, source.refresh = 50*time.Millisecond, 75*time.Millisecond
	snapshot, err := source.Load(test.Context())
	if err != nil || !snapshot.Partial || maximum.Load() > 4 {
		test.Fatalf("bounded collection: concurrency=%d snapshot=%+v err=%v", maximum.Load(), snapshot, err)
	}
	large := AgentsSnapshot{Version: AgentsVersion, Rows: []AgentRow{}}
	now := time.Now().UTC()
	for index := 0; index < MaxAgentRows; index++ {
		row := AgentRow{ThreadID: fmt.Sprintf("thread-%d", index), Spans: []AgentSpan{}}
		for spanIndex := 0; spanIndex < activity.MaxSpans; spanIndex++ {
			row.Spans = append(row.Spans, AgentSpan{RunID: "run", Source: activity.SourceCodex, Span: activity.Span{ID: fmt.Sprintf("span-%d", spanIndex), StartedAt: now, EndedAt: &now, State: activity.Finished}})
		}
		large.Rows = append(large.Rows, row)
	}
	boundAgentSnapshot(&large)
	count := 0
	for _, row := range large.Rows {
		count += len(row.Spans) + len(row.Points)
	}
	var encoded bytes.Buffer
	if err := termsafe.JSONEncoder(&encoded).Encode(large); err != nil {
		test.Fatal(err)
	}
	if count != MaxAgentSpans || large.DroppedSpans != MaxAgentRows*activity.MaxSpans-MaxAgentSpans || !large.Partial || encoded.Len() > MaxAgentsJSONBytes {
		test.Fatalf("aggregate bounds: count=%d dropped=%d bytes=%d partial=%t", count, large.DroppedSpans, encoded.Len(), large.Partial)
	}
}

func TestAgentsSourceAppliesNativeRequestDeadline(test *testing.T) {
	ctx, cancel := context.WithTimeout(test.Context(), 5*time.Second)
	defer cancel()
	called := make(chan bool, 1)
	source := NewAgentsSource(project.Store{}, "", func(request context.Context, _ mail.Worker) (NativeActivity, error) {
		deadline, exists := request.Deadline()
		called <- exists && time.Until(deadline) <= 50*time.Millisecond
		<-request.Done()
		return NativeActivity{}, request.Err()
	})
	source.request, source.refresh = 50*time.Millisecond, 2*time.Second
	results := source.collect(ctx, map[int]mail.Worker{0: {RunID: "run"}})
	select {
	case bounded := <-called:
		if !bounded {
			test.Fatal("native request inherited the outer deadline instead of its bounded timeout")
		}
	default:
		test.Fatal("native deadline test never invoked its observer")
	}
	result, exists := results[0]
	if !exists || !errors.Is(result.err, context.DeadlineExceeded) || ctx.Err() != nil {
		test.Fatalf("native deadline did not cancel the request: result=%+v exists=%t outer=%v", result, exists, ctx.Err())
	}
}

func TestAgentsSourceRejectsConflictingSettledInterval(test *testing.T) {
	store, record := sourceProject(test)
	worker := bindAgent(test, store, record, record.CoordinatorThreadID, "lead")
	now := time.Now().UTC()
	ended := now.Add(-time.Second)
	observation := NativeActivity{NativeSessionID: worker.ThreadID, Activity: &activity.Snapshot{Version: activity.Version, Source: activity.SourceCodex, Coverage: activity.CoverageNativeHistory, ObservedAt: now, Spans: []activity.Span{{ID: "settled", StartedAt: now.Add(-time.Minute), EndedAt: &ended, State: activity.Finished}}}}
	source := NewAgentsSource(store, record.ID, func(context.Context, mail.Worker) (NativeActivity, error) { return observation, nil })
	if _, err := source.Load(test.Context()); err != nil {
		test.Fatal(err)
	}
	observation.Activity.Spans[0].State = activity.Failed
	snapshot, err := source.Load(test.Context())
	if err != nil || snapshot.Rows[0].State != "unknown" || snapshot.Rows[0].Reason != "conflicting-native-activity" || snapshot.Rows[0].Spans[0].Span.State != activity.Finished {
		test.Fatalf("conflict replaced known outcome: %+v err=%v", snapshot, err)
	}
}

func TestAgentsSourceBoundsRowsWithoutErasingThreadTotal(test *testing.T) {
	store, record := sourceProject(test)
	for index := 2; index <= MaxAgentRows; index++ {
		if _, _, err := store.CreateThread(test.Context(), record.ID, fmt.Sprintf("thread-%d", index), ""); err != nil {
			test.Fatal(err)
		}
	}
	before := snapshotTree(test, store.Root)
	snapshot, err := NewAgentsSource(store, record.ID, nil).Load(test.Context())
	if err != nil || snapshot.TotalThreads != MaxAgentRows+1 || len(snapshot.Rows) != MaxAgentRows || snapshot.DroppedThreads != 1 || !snapshot.Partial || snapshot.Reason != "thread-limit" {
		test.Fatalf("row bound=%+v err=%v", snapshot, err)
	}
	if before != snapshotTree(test, store.Root) {
		test.Fatal("row truncation rewrote registry")
	}
}

func TestAgentsSourceRevalidatesInitiallyUnboundRows(test *testing.T) {
	store, record := sourceProject(test)
	worker := bindAgent(test, store, record, record.CoordinatorThreadID, "lead")
	_, initial, _, err := store.Inspect(test.Context(), record.ID)
	if err != nil {
		test.Fatal(err)
	}
	if len(initial.Threads) != 2 || initial.Threads[1].ActiveRunID != "" {
		test.Fatal("negative control does not start with an unbound worker")
	}
	source := NewAgentsSource(store, record.ID, func(context.Context, mail.Worker) (NativeActivity, error) {
		bindAgent(test, store, record, initial.Threads[1].ID, "late-worker")
		return NativeActivity{NativeSessionID: worker.ThreadID, Activity: &activity.Snapshot{Version: activity.Version, Source: activity.SourceCodex, Coverage: activity.CoverageNativeHistory, ObservedAt: time.Now().UTC(), Spans: []activity.Span{}}}, nil
	})
	snapshot, err := source.Load(test.Context())
	if err != nil || !snapshot.Partial || snapshot.Rows[1].State != "stale" || snapshot.Rows[1].Reason != "binding-changed" || snapshot.Rows[1].Enrollment != string(project.EnrollmentStale) {
		test.Fatalf("changed unbound row became authoritative: %+v err=%v", snapshot, err)
	}
}

func TestCloneAgentRowDoesNotShareCoverageStart(test *testing.T) {
	since := time.Now().UTC()
	original := AgentRow{Since: &since}
	cloned := cloneAgentRow(original)
	*cloned.Since = since.Add(time.Hour)
	if !original.Since.Equal(since) || original.Since.Equal(*cloned.Since) {
		test.Fatal("clone shared coverage start")
	}
}

func TestAgentSpanCanResolveUnknownOutcomeWithoutChangingBoundaries(test *testing.T) {
	now := time.Now().UTC()
	unknown := AgentSpan{RunID: "run", NativeSessionID: "native", Source: activity.SourceHermes, Span: activity.Span{ID: "input", TurnID: "input", StartedAt: now.Add(-time.Minute), EndedAt: &now, State: activity.Unknown}}
	row := AgentRow{Spans: []AgentSpan{unknown}}
	resolved := unknown
	resolved.Span.State = activity.Finished
	if err := mergeAgentSpan(&row, resolved); err != nil || row.Spans[0].Span.State != activity.Finished || !row.Spans[0].Span.EndedAt.Equal(now) {
		test.Fatalf("unknown outcome did not resolve with stable boundaries: row=%+v err=%v", row, err)
	}
	if err := mergeAgentSpan(&row, unknown); err == nil {
		test.Fatal("known outcome regressed to unknown")
	}
}

func bindAgent(test *testing.T, store project.Store, record project.Project, threadID, name string) mail.Worker {
	test.Helper()
	team := filepath.Join(store.Root, record.ID, "team")
	if err := os.MkdirAll(team, 0o700); err != nil {
		test.Fatal(err)
	}
	worker := mail.Worker{Name: name, Harness: mail.HarnessCodex, RunID: "run-" + name, ThreadID: "native-" + name, Worktree: record.Worktree, Coordinator: threadID == record.CoordinatorThreadID}
	if err := (mail.FileRoster{Dir: team}).Put(worker); err != nil {
		test.Fatal(err)
	}
	if _, _, err := store.Bind(test.Context(), record.ID, threadID, name, worker.RunID, ""); err != nil {
		test.Fatal(err)
	}
	return worker
}
