//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package hermes

import (
	"errors"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/surface/activity"
)

func TestHermesActivityLifecycleRingAndClone(test *testing.T) {
	started := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	receiver := &supervisor{
		sessionID:     "session-1",
		model:         "openai-codex:test-model",
		mode:          "default",
		activitySince: started,
		activitySpans: make([]activity.Span, 0, activity.MaxSpans),
	}

	idle := receiver.activitySnapshotLocked(started.Add(time.Second))
	if idle == nil || len(idle.Spans) != 0 || idle.Since == nil {
		test.Fatalf("idle activity = %#v", idle)
	}
	if err := activity.Validate(*idle); err != nil {
		test.Fatal(err)
	}

	turnStart := started.Add(2 * time.Second)
	receiver.beginActivity("message-1", turnStart)
	running := receiver.activitySnapshotLocked(turnStart.Add(time.Second))
	if running == nil || len(running.Spans) != 1 || running.Spans[0].State != activity.Running || running.Spans[0].EndedAt != nil {
		test.Fatalf("running activity = %#v", running)
	}
	span := running.Spans[0]
	if span.ID != "message-1" || span.TurnID != "message-1" || span.Provider != "openai-codex" || span.Model != "test-model" || !span.StartedAt.Equal(turnStart) {
		test.Fatalf("running span identity = %#v", span)
	}

	running.Spans[0].State = activity.Failed
	running.Spans[0].EndedAt = timePointer(turnStart.Add(3 * time.Second))
	stillRunning := receiver.activitySnapshotLocked(turnStart.Add(time.Second))
	if stillRunning.Spans[0].State != activity.Running || stillRunning.Spans[0].EndedAt != nil {
		test.Fatalf("inspection returned mutable activity state: %#v", stillRunning.Spans[0])
	}

	receiver.finishActivity("message-1", activity.Unknown, turnStart.Add(3*time.Second))
	pending := receiver.activitySnapshotLocked(turnStart.Add(3 * time.Second))
	if pending.Spans[0].State != activity.Unknown || pending.Spans[0].EndedAt == nil {
		test.Fatalf("pending activity was not closed unknown: %#v", pending.Spans[0])
	}
	receiver.resolveActivity("message-1", activity.Finished)
	settled := receiver.activitySnapshotLocked(turnStart.Add(4 * time.Second))
	if settled.Spans[0].State != activity.Finished || settled.Spans[0].EndedAt == nil {
		test.Fatalf("settled activity = %#v", settled.Spans[0])
	}
	if err := activity.Validate(*settled); err != nil {
		test.Fatal(err)
	}
	receiver.beginActivity("message-proof", turnStart.Add(4*time.Second))
	receiver.finishActivity("message-proof", activity.Unknown, turnStart.Add(5*time.Second))
	proofFailed := receiver.activitySnapshotLocked(turnStart.Add(6 * time.Second))
	if proofFailed.Spans[1].State != activity.Unknown || proofFailed.Spans[1].EndedAt == nil || !proofFailed.Spans[1].EndedAt.Equal(turnStart.Add(5*time.Second)) {
		test.Fatalf("proof failure did not retain end time: %#v", proofFailed.Spans[1])
	}

	receiver.beginActivity("message-failed", turnStart.Add(5*time.Second))
	receiver.finishActivity("message-failed", hermesActivityState(errors.New("disconnect"), "", 0, 0), turnStart.Add(6*time.Second))
	failed := receiver.activitySnapshotLocked(turnStart.Add(7 * time.Second))
	if failed.Spans[2].State != activity.Unknown || failed.Spans[2].EndedAt == nil {
		test.Fatalf("failed activity = %#v", failed.Spans[2])
	}

	for index := 0; index < activity.MaxSpans+1; index++ {
		id := "ring-" + string(rune('a'+index%26)) + "-" + time.Duration(index).String()
		when := turnStart.Add(time.Duration(index+10) * time.Second)
		receiver.beginActivity(id, when)
		receiver.finishActivity(id, activity.Finished, when.Add(time.Second))
	}
	ring := receiver.activitySnapshotLocked(turnStart.Add(time.Duration(activity.MaxSpans+20) * time.Second))
	if ring == nil || len(ring.Spans) != activity.MaxSpans || !ring.Partial {
		test.Fatalf("ring activity = %#v", ring)
	}
	if err := activity.Validate(*ring); err != nil {
		test.Fatal(err)
	}

	oldSince := *ring.Since
	receiver.setNativeSession("session-2", "openai-codex:replacement", "default", "native-2")
	reset := receiver.activitySnapshotLocked(time.Now().UTC())
	if reset == nil || len(reset.Spans) != 0 || reset.Partial || reset.Since == nil || !reset.Since.After(oldSince) {
		test.Fatalf("session replacement did not reset activity: %#v", reset)
	}
}

func TestHermesActivityRefusesClockRollbackRatherThanInventingTiming(test *testing.T) {
	now := time.Now().UTC()
	receiver := &supervisor{activitySince: now, model: "openai-codex:model"}
	receiver.beginActivity("input", now.Add(time.Second))
	receiver.finishActivity("input", activity.Unknown, now)
	if receiver.activitySnapshotLocked(now.Add(2*time.Second)) != nil {
		test.Fatal("reversed observed timing became a valid activity interval")
	}
	if !receiver.activitySpans[0].EndedAt.Equal(now) {
		test.Fatal("native end timestamp was rewritten")
	}
}

func TestHermesActivityOutcomeMapping(test *testing.T) {
	tests := []struct {
		name                     string
		err                      error
		stopReason               string
		permissions, unsupported uint64
		want                     activity.State
	}{
		{name: "end turn", stopReason: "end_turn", want: activity.Finished},
		{name: "refusal", stopReason: "refusal", want: activity.Incomplete},
		{name: "permission", stopReason: "end_turn", permissions: 1, want: activity.Unknown},
		{name: "unsupported", stopReason: "end_turn", unsupported: 1, want: activity.Unknown},
		{name: "rpc failure", err: errors.New("timeout"), want: activity.Unknown},
		{name: "missing stop reason", want: activity.Unknown},
	}
	for _, scenario := range tests {
		test.Run(scenario.name, func(test *testing.T) {
			if got := hermesActivityState(scenario.err, scenario.stopReason, scenario.permissions, scenario.unsupported); got != scenario.want {
				test.Fatalf("state = %q, want %q", got, scenario.want)
			}
		})
	}
}
