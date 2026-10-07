// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package runview

import (
	"testing"
	"time"
)

func intp(n int) *int { return &n }

func TestPickGlyphs(t *testing.T) {
	if PickGlyphs(true) != ASCIIGlyphs || PickGlyphs(false) != IconGlyphs {
		t.Errorf("PickGlyphs does not pick the legend asked for")
	}
}

func TestRunMark(t *testing.T) {
	g := ASCIIGlyphs
	for _, c := range []struct {
		name string
		live LiveState
		exit *int
		want Mark
	}{
		{"live", LiveLive, nil, Mark{g.Running, "live", ToneActive}},
		{"running", LiveRunning, nil, Mark{g.Running, "running", ToneActive}},
		{"ended no exit", LiveEnded, nil, Mark{g.Pending, "ended", ToneMuted}},
		{"ended ok", LiveEnded, intp(0), Mark{g.Done, "ok", ToneOK}},
		{"ended exit 3", LiveEnded, intp(3), Mark{g.Failed, "exit 3", ToneDanger}},
		{"lost", LiveLost, nil, Mark{g.Lost, "lost", ToneDanger}},
		{"waiting", LiveWaiting, nil, Mark{g.Pending, "waiting", ToneDim}},
		{"skipped", LiveSkipped, nil, Mark{g.Skipped, "skipped", ToneDim}},
		{"unknown", LiveUnknown, nil, Mark{g.Pending, "log", ToneMuted}},
		{"empty", "", nil, Mark{g.Pending, "pending", ToneDim}},
	} {
		if got := RunMark(g, c.live, c.exit); got != c.want {
			t.Errorf("%s: RunMark = %+v, want %+v", c.name, got, c.want)
		}
	}
}

func TestStepMark(t *testing.T) {
	g := IconGlyphs
	for _, c := range []struct {
		name   string
		st     StepStatus
		runner string
		want   Mark
	}{
		{"pending", StepPending, "", Mark{g.Pending, "pending", ToneDim}},
		{"running", StepRunning, "running", Mark{g.Running, "running", ToneActive}},
		{"closed", StepClosed, "ok", Mark{g.Done, "done", ToneOK}},
		{"failed", StepFailed, "failed", Mark{g.Failed, "failed", ToneDanger}},
		{"interrupted", StepInterrupted, "", Mark{g.Interrupted, "interrupted", ToneWarn}},
		{"skipped status", StepSkipped, "", Mark{g.Skipped, "skipped", ToneDim}},
		{"skipped status wins over a runner word", StepSkipped, "failed", Mark{g.Skipped, "skipped", ToneDim}},
		{"runner skipped, pending", StepPending, "skipped", Mark{g.Skipped, "skipped", ToneDim}},
		{"runner skipped, failed", StepFailed, "skipped", Mark{g.Failed, "failed", ToneDanger}},
		{"runner cancelled, running", StepRunning, "cancelled", Mark{g.Skipped, "cancelled", ToneDim}},
		{"runner cancelled, closed", StepClosed, "cancelled", Mark{g.Done, "done", ToneOK}},
	} {
		if got := StepMark(g, c.st, c.runner); got != c.want {
			t.Errorf("%s: StepMark = %+v, want %+v", c.name, got, c.want)
		}
	}
}

func TestRunnerState(t *testing.T) {
	timing := []StepTiming{{Step: "a", State: "ok"}, {Step: "b", State: "skipped"}}
	if got := RunnerState(timing, "b"); got != "skipped" {
		t.Errorf("RunnerState(b) = %q", got)
	}
	if got := RunnerState(timing, "zz"); got != "" {
		t.Errorf("RunnerState(zz) = %q, want empty", got)
	}
	if got := RunnerState(nil, "a"); got != "" {
		t.Errorf("RunnerState(nil) = %q, want empty", got)
	}
}

func TestStepDur(t *testing.T) {
	timing := []StepTiming{{Step: "a", Dur: 3 * time.Second}, {Step: "b"}}
	withTimes := StepState{ID: "b", Start: at(1), End: at(8)}
	for _, c := range []struct {
		name string
		st   StepState
		want time.Duration
	}{
		{"timing duration wins", StepState{ID: "a", Start: at(1), End: at(9)}, 3 * time.Second},
		{"zero timing duration falls back to the fold", withTimes, 7 * time.Second},
		{"no timing row uses the fold", StepState{ID: "c", Start: at(2), End: at(4)}, 2 * time.Second},
		{"running step has no duration", StepState{ID: "c", Start: at(2)}, 0},
		{"unstarted step has no duration", StepState{ID: "c"}, 0},
	} {
		if got := StepDur(timing, c.st); got != c.want {
			t.Errorf("%s: StepDur = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestCompareRuns(t *testing.T) {
	live := RunOrder{Live: true, Activity: at(1), Name: "z", Source: "desk"}
	newer := RunOrder{Activity: at(9), Name: "b", Source: "desk"}
	older := RunOrder{Activity: at(2), Name: "a", Source: "desk"}
	for _, c := range []struct {
		name string
		a, b RunOrder
		want int
	}{
		{"live before not live", live, newer, -1},
		{"not live after live", newer, live, 1},
		{"newer activity first", newer, older, -1},
		{"older activity last", older, newer, 1},
		{"name breaks a tie", RunOrder{Activity: at(1), Name: "a"}, RunOrder{Activity: at(1), Name: "b"}, -1},
		{"source breaks a name tie", RunOrder{Activity: at(1), Name: "a", Source: "desk"}, RunOrder{Activity: at(1), Name: "a", Source: "log"}, -1},
		{"equal", older, older, 0},
	} {
		if got := CompareRuns(c.a, c.b); got != c.want {
			t.Errorf("%s: CompareRuns = %d, want %d", c.name, got, c.want)
		}
	}
	// A name that is a prefix of another sorts first despite the source.
	if got := CompareRuns(RunOrder{Name: "a", Source: "z"}, RunOrder{Name: "ab", Source: "a"}); got != -1 {
		t.Errorf("prefix name: CompareRuns = %d, want -1", got)
	}
}

func TestGist(t *testing.T) {
	t0 := time.Date(2026, 10, 7, 1, 0, 0, 0, time.UTC)
	exit1, exit0 := 1, 0
	failEv := Event{Step: "docs", Name: "connection reset", Time: t0.Add(time.Minute), Fields: []Field{{LensActionField, "fail"}}}
	for _, c := range []struct {
		name   string
		s      RunState
		live   LiveState
		events []Event
		now    time.Time
		want   string
	}{
		{"failed with why", RunState{Steps: []StepState{{ID: "photos", Status: StepClosed}, {ID: "docs", Status: StepFailed}}, Exit: &exit1},
			LiveEnded, []Event{failEv}, t0, "docs failed at 01:01:00: connection reset · exit 1"},
		{"finished", RunState{Steps: []StepState{{ID: "a", Status: StepClosed}}, Exit: &exit0}, LiveEnded, nil, t0, "finished: 1 of 1 steps done · exit 0"},
		{"running", RunState{Steps: []StepState{{ID: "a", Status: StepRunning, Start: t0}}}, LiveLive, nil, t0.Add(3 * time.Minute), "running a for 3m0s"},
		{"replay has no clock", RunState{Steps: []StepState{{ID: "a", Status: StepRunning, Start: t0}}}, LiveLive, nil, time.Time{}, "running a"},
		{"desk failure says no why", RunState{Steps: []StepState{{ID: "b", Status: StepFailed}}}, LiveRunning, []Event{{Step: "b", Name: "STEP-FAIL"}}, t0, "b failed"},
		{"lost", RunState{}, LiveLost, nil, t0, "lost: it stopped with no end"},
		{"nothing yet", RunState{}, LiveUnknown, nil, t0, ""},
	} {
		if got := Gist(c.s, c.live, c.events, c.now); got != c.want {
			t.Errorf("%s: Gist = %q, want %q", c.name, got, c.want)
		}
	}
}
