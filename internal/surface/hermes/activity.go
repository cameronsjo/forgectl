//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package hermes

import (
	"strings"
	"time"

	"github.com/cameronsjo/forgectl/internal/surface/activity"
)

func (receiver *supervisor) setNativeSession(session, model, mode, version string) {
	now := time.Now().UTC()
	receiver.mu.Lock()
	defer receiver.mu.Unlock()
	sessionStartedAt := receiver.activitySessionStart
	if sessionStartedAt.IsZero() {
		sessionStartedAt = now
	}
	if receiver.sessionID != session || receiver.activitySince.IsZero() {
		receiver.activitySince = sessionStartedAt
		receiver.activitySpans = make([]activity.Span, 0, activity.MaxSpans)
		receiver.activityPartial = false
	}
	receiver.activitySessionStart = time.Time{}
	receiver.sessionID, receiver.model, receiver.mode, receiver.nativeVer = session, model, mode, version
}

func (receiver *supervisor) beginActivity(id string, startedAt time.Time) {
	receiver.mu.Lock()
	defer receiver.mu.Unlock()
	if receiver.activitySince.IsZero() {
		receiver.activitySince = startedAt
	}
	for index := range receiver.activitySpans {
		if receiver.activitySpans[index].State == activity.Running {
			receiver.activitySpans[index].State = activity.Unknown
		}
	}
	provider, model, _ := strings.Cut(receiver.model, ":")
	span := activity.Span{ID: id, TurnID: id, StartedAt: startedAt, State: activity.Running}
	if provider != "" && model != "" {
		span.Provider, span.Model = provider, model
	}
	receiver.activitySpans = append(receiver.activitySpans, span)
	if len(receiver.activitySpans) > activity.MaxSpans {
		receiver.activitySpans = append([]activity.Span(nil), receiver.activitySpans[len(receiver.activitySpans)-activity.MaxSpans:]...)
		receiver.activityPartial = true
	}
}

func (receiver *supervisor) finishActivity(id string, state activity.State, endedAt time.Time) {
	receiver.mu.Lock()
	defer receiver.mu.Unlock()
	for index := len(receiver.activitySpans) - 1; index >= 0; index-- {
		span := &receiver.activitySpans[index]
		if span.ID != id || span.State != activity.Running {
			continue
		}
		if state == activity.Running {
			state = activity.Unknown
		}
		span.EndedAt = &endedAt
		span.State = state
		return
	}
}

func (receiver *supervisor) resolveActivity(id string, state activity.State) {
	receiver.mu.Lock()
	defer receiver.mu.Unlock()
	for index := len(receiver.activitySpans) - 1; index >= 0; index-- {
		span := &receiver.activitySpans[index]
		if span.ID == id && span.State == activity.Unknown && span.EndedAt != nil {
			if state == activity.Running {
				return
			}
			span.State = state
			return
		}
	}
}

func (receiver *supervisor) activitySnapshotLocked(observedAt time.Time) *activity.Snapshot {
	if receiver.activitySince.IsZero() {
		return nil
	}
	snapshot := activity.Snapshot{
		Version:    activity.Version,
		Source:     activity.SourceHermes,
		Coverage:   activity.CoverageCurrentProcess,
		ObservedAt: observedAt,
		Since:      timePointer(receiver.activitySince),
		Spans:      make([]activity.Span, len(receiver.activitySpans)),
		Partial:    receiver.activityPartial,
	}
	for index, span := range receiver.activitySpans {
		snapshot.Spans[index] = span
		if span.EndedAt != nil {
			endedAt := *span.EndedAt
			snapshot.Spans[index].EndedAt = &endedAt
		}
	}
	if err := activity.Validate(snapshot); err != nil {
		return nil
	}
	return &snapshot
}

func timePointer(value time.Time) *time.Time {
	cloned := value
	return &cloned
}

func hermesActivityState(err error, stopReason string, permissionCount, unsupportedCount uint64) activity.State {
	if permissionCount != 0 || unsupportedCount != 0 {
		return activity.Unknown
	}
	if err != nil {
		return activity.Unknown
	}
	if stopReason == "end_turn" {
		return activity.Finished
	}
	if stopReason != "" {
		return activity.Incomplete
	}
	return activity.Unknown
}
