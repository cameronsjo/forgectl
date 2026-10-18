package activity

import (
	"strings"
	"testing"
	"time"
)

func activityFixture() Snapshot {
	start := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	end := start.Add(time.Minute)
	return Snapshot{
		Version: Version, Source: SourceCodex, Coverage: CoverageNativeHistory, ObservedAt: end,
		Spans: []Span{{ID: "span-1", TurnID: "turn-1", StartedAt: start, EndedAt: &end, State: Finished}},
	}
}

func TestActivitySnapshotValidation(test *testing.T) {
	if err := Validate(activityFixture()); err != nil {
		test.Fatal(err)
	}
	mutations := map[string]func(*Snapshot){
		"version":       func(snapshot *Snapshot) { snapshot.Version++ },
		"source":        func(snapshot *Snapshot) { snapshot.Source = "other" },
		"coverage":      func(snapshot *Snapshot) { snapshot.Coverage = CoverageCurrentProcess },
		"observed time": func(snapshot *Snapshot) { snapshot.ObservedAt = time.Time{} },
		"missing spans": func(snapshot *Snapshot) { snapshot.Spans = nil },
		"too many spans": func(snapshot *Snapshot) {
			snapshot.Spans = make([]Span, MaxSpans+1)
		},
		"duplicate identity": func(snapshot *Snapshot) { snapshot.Spans = append(snapshot.Spans, snapshot.Spans[0]) },
		"invalid identity":   func(snapshot *Snapshot) { snapshot.Spans[0].ID = "unsafe\nidentity" },
		"invalid turn":       func(snapshot *Snapshot) { snapshot.Spans[0].TurnID = strings.Repeat("a", 129) },
		"missing start":      func(snapshot *Snapshot) { snapshot.Spans[0].StartedAt = time.Time{} },
		"reversed time": func(snapshot *Snapshot) {
			snapshot.Spans[0].StartedAt = snapshot.ObservedAt.Add(time.Second)
		},
		"future interval": func(snapshot *Snapshot) {
			snapshot.Spans[0].EndedAt = nil
			snapshot.Spans[0].State = Running
			snapshot.Spans[0].StartedAt = snapshot.ObservedAt.Add(time.Minute)
		},
		"running with end": func(snapshot *Snapshot) { snapshot.Spans[0].State = Running },
		"settled without end": func(snapshot *Snapshot) {
			snapshot.Spans[0].EndedAt = nil
		},
		"unknown state": func(snapshot *Snapshot) { snapshot.Spans[0].State = "success" },
		"missing model": func(snapshot *Snapshot) { snapshot.Spans[0].Provider = "openai" },
		"unsafe model": func(snapshot *Snapshot) {
			snapshot.Spans[0].Provider, snapshot.Spans[0].Model = "openai", "model\x1b[31m"
		},
		"oversized model": func(snapshot *Snapshot) {
			snapshot.Spans[0].Provider, snapshot.Spans[0].Model = "openai", strings.Repeat("a", 257)
		},
		"multiple running": func(snapshot *Snapshot) {
			snapshot.Spans[0].State, snapshot.Spans[0].EndedAt = Running, nil
			second := snapshot.Spans[0]
			second.ID = "span-2"
			snapshot.Spans = append(snapshot.Spans, second)
		},
	}
	for name, mutate := range mutations {
		test.Run(name, func(test *testing.T) {
			snapshot := activityFixture()
			mutate(&snapshot)
			if err := Validate(snapshot); err == nil {
				test.Fatal("invalid native activity was accepted")
			}
		})
	}
}

func TestProcessActivityCoverageAndUnknownOutcome(test *testing.T) {
	snapshot := activityFixture()
	since := snapshot.Spans[0].StartedAt
	snapshot.Source, snapshot.Coverage, snapshot.Since = SourceHermes, CoverageCurrentProcess, &since
	snapshot.Spans[0].State, snapshot.Spans[0].EndedAt = Unknown, nil
	snapshot.Partial = true
	if err := Validate(snapshot); err != nil {
		test.Fatal(err)
	}
	snapshot.Since = nil
	if err := Validate(snapshot); err == nil {
		test.Fatal("process coverage without a boundary was accepted")
	}
	afterStart := since.Add(time.Second)
	snapshot.Since = &afterStart
	if err := Validate(snapshot); err == nil {
		test.Fatal("activity from before process coverage was accepted")
	}
	snapshot.Since, snapshot.Spans = &since, []Span{}
	if err := Validate(snapshot); err != nil {
		test.Fatal(err)
	}
}

func TestActivityCloneDoesNotShareMutableState(test *testing.T) {
	snapshot := activityFixture()
	since := snapshot.Spans[0].StartedAt
	snapshot.Since = &since
	cloned := Clone(snapshot)
	cloned.Spans[0].State = Failed
	*cloned.Spans[0].EndedAt = cloned.Spans[0].StartedAt
	*cloned.Since = time.Time{}
	if snapshot.Spans[0].State != Finished || snapshot.Spans[0].EndedAt.Equal(snapshot.Spans[0].StartedAt) || snapshot.Since.IsZero() {
		test.Fatal("cloned activity mutated its source")
	}
	if Clone(Snapshot{}).Spans != nil {
		test.Fatal("clone repaired malformed missing spans")
	}
}
