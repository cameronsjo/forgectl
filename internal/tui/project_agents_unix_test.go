//go:build unix

package tui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/cameronsjo/forgectl/internal/surface/activity"
	"github.com/cameronsjo/forgectl/internal/surface/projectview"
	"github.com/cameronsjo/forgectl/internal/theme"
)

func TestRenderProjectAgentsKnownAndMissingGeometry(test *testing.T) {
	now := time.Date(2026, 10, 17, 12, 30, 0, 0, time.UTC)
	ended := now.Add(-10 * time.Minute)
	snapshot := projectview.AgentsSnapshot{
		ProjectName: "project",
		Rows: []projectview.AgentRow{{
			ThreadID:   "thread-1",
			Name:       "build",
			Harness:    "pi",
			Provider:   "openai",
			Model:      "gpt-5",
			State:      "active",
			ObservedAt: now.Add(-time.Minute),
			Spans: []projectview.AgentSpan{
				{Source: activity.SourcePi, Span: activity.Span{ID: "one", StartedAt: now.Add(-20 * time.Minute), EndedAt: &ended, State: activity.Finished}},
				{Source: activity.SourceHermes, Span: activity.Span{ID: "two", State: activity.Unknown}},
			},
		}},
	}
	for _, ascii := range []bool{false, true} {
		out := RenderProjectAgents(snapshot, 100, 10, "thread-1", 30*time.Minute, now, ascii, theme.Default())
		plain := ansi.Strip(out)
		if !strings.Contains(plain, "build") || !strings.Contains(plain, "Working") || !strings.Contains(plain, "pi / gpt-5") {
			test.Fatalf("renderer omitted row metadata (ascii=%v): %q", ascii, plain)
		}
		if ascii && !strings.Contains(plain, "=") {
			test.Fatalf("ASCII renderer omitted the known span: %q", plain)
		}
		if !ascii && !strings.Contains(plain, "━") {
			test.Fatalf("Unicode renderer omitted the known span: %q", plain)
		}
		for _, line := range strings.Split(out, "\n") {
			if ansi.StringWidth(line) > 100 {
				test.Fatalf("line overflow: %d: %q", ansi.StringWidth(line), line)
			}
		}
	}
}

func TestRenderProjectAgentsKeepsNativeModelsVisible(test *testing.T) {
	now := time.Now().UTC()
	for _, harness := range []string{"codex", "pi", "hermes"} {
		row := projectview.AgentRow{ThreadID: "thread", Name: "worker", Harness: harness, Provider: "openai-codex", Model: "gpt-5.6-sol", State: "idle", ObservedAt: now, Coverage: activity.CoverageCurrentProcess}
		for _, width := range []int{80, 132} {
			plain := ansi.Strip(RenderProjectAgents(projectview.AgentsSnapshot{ProjectName: "project", Rows: []projectview.AgentRow{row}}, width, 10, row.ThreadID, 30*time.Minute, now, false, theme.Default()))
			if !strings.Contains(plain, harness+" / gpt-5.6-sol") {
				test.Fatalf("native model clipped at width %d: %q", width, plain)
			}
		}
	}
}

func TestProjectAgentsUnboundLayoutHasBreathingRoom(test *testing.T) {
	now := time.Now().UTC()
	snapshot := projectview.AgentsSnapshot{ProjectName: "Projects preview", Rows: []projectview.AgentRow{
		{ThreadID: "coordinator", Name: "Projects preview", Role: "coordinator", State: "unbound", Enrollment: "unbound", Reason: "thread-unbound"},
		{ThreadID: "worker", Name: "Implementation", Role: "worker", State: "unbound", Enrollment: "unbound", Reason: "thread-unbound"},
		{ThreadID: "review", Name: "Review", Role: "worker", State: "unbound", Enrollment: "unbound", Reason: "thread-unbound"},
	}}
	for _, width := range []int{50, 80, 132} {
		plain := ansi.Strip(RenderProjectAgents(snapshot, width, 24, "worker", projectAgentsDefaultView, now, true, theme.Default()))
		for _, want := range []string{"No agents connected", "Not connected", "Coordinator", "Implementation", "Review"} {
			if !strings.Contains(plain, want) {
				test.Fatalf("width %d omitted %q: %q", width, want, plain)
			}
		}
		for _, forbidden := range []string{"harness unknown", "model unknown", "coverage unknown", "reason/", "native activity:", "thread-unbound"} {
			if strings.Contains(plain, forbidden) {
				test.Fatalf("width %d leaked diagnostic %q: %q", width, forbidden, plain)
			}
		}
		lines := strings.Split(plain, "\n")
		if len(lines) != 24 || strings.TrimSpace(lines[0]) != "" {
			test.Fatalf("width %d lost viewport or top margin: %q", width, plain)
		}
		for lineIndex, line := range lines {
			if ansi.StringWidth(line) > width {
				test.Fatalf("width %d overflowed: %q", width, line)
			}
			if strings.Contains(line, "Implementation") && (lineIndex+2 >= len(lines) || strings.TrimSpace(lines[lineIndex+2]) != "") {
				test.Fatalf("width %d has no gap after the selected row: %q", width, plain)
			}
		}
	}
}

func TestProjectAgentsAxisAndBarsShareColumn(test *testing.T) {
	now := time.Date(2026, 10, 18, 12, 30, 0, 0, time.UTC)
	ended := now
	snapshot := projectview.AgentsSnapshot{Rows: []projectview.AgentRow{{
		ThreadID: "thread", Name: "Implementation", Harness: "pi", Model: "gpt-5.6-sol", State: "idle", ObservedAt: now,
		Spans: []projectview.AgentSpan{{Source: activity.SourcePi, Span: activity.Span{StartedAt: now.Add(-30 * time.Minute), EndedAt: &ended, State: activity.Finished}}},
	}}}
	plain := ansi.Strip(RenderProjectAgents(snapshot, 100, 24, "thread", projectAgentsDefaultView, now, true, theme.Default()))
	axisColumn, barColumn := -1, -1
	for _, line := range strings.Split(plain, "\n") {
		if strings.Contains(line, "12:00") {
			axisColumn = strings.Index(line, "12:00")
		}
		if strings.Contains(line, "Implementation") {
			barColumn = strings.Index(line, "=")
		}
	}
	if axisColumn < 0 || barColumn < 0 || axisColumn != barColumn {
		test.Fatalf("axis column %d does not match bar column %d: %q", axisColumn, barColumn, plain)
	}
}

func TestProjectAgentsSpacingDoesNotHideRowsInShortViewports(test *testing.T) {
	now := time.Now().UTC()
	for _, state := range []string{"idle", "unbound"} {
		rows := []projectview.AgentRow{
			{ThreadID: "one", Name: "Coordinator", State: state},
			{ThreadID: "two", Name: "Implementation", State: state},
			{ThreadID: "three", Name: "Review", State: state},
		}
		for height := 9; height <= 24; height++ {
			plain := ansi.Strip(RenderProjectAgents(projectview.AgentsSnapshot{ProjectName: "preview", Rows: rows}, 80, height, "three", projectAgentsDefaultView, now, true, theme.Default()))
			for _, row := range rows {
				if !strings.Contains(plain, row.Name) {
					test.Fatalf("height %d state %s hid %s: %q", height, state, row.Name, plain)
				}
			}
		}
	}
}

func TestProjectAgentsUnboundLabelDoesNotDependOnActivityState(test *testing.T) {
	for _, state := range []string{"", "unknown", "stale"} {
		row := projectview.AgentRow{Enrollment: "unbound", State: state}
		if got := projectAgentsStateLabel(row, time.Now(), false); got != "Not connected" {
			test.Fatalf("unbound enrollment with state %q rendered %q", state, got)
		}
	}
}

func TestProjectAgentsSummaryKeepsCoverageWithoutRawDiagnostics(test *testing.T) {
	now := time.Now().UTC()
	row := projectview.AgentRow{State: "idle", ObservedAt: now, Coverage: activity.CoverageCurrentProcess, Partial: true, Reason: "native-read-failed"}
	label := projectAgentsSummary(row, now, false, true)
	if label != "Idle - current session only - partial data" {
		test.Fatalf("unexpected labels: %s", label)
	}
}

func TestProjectAgentsGeometryDoesNotInventBars(test *testing.T) {
	now := time.Date(2026, 10, 17, 12, 30, 0, 0, time.UTC)
	start := now.Add(-5 * time.Minute)
	left, right, point, visible := projectAgentsSpanGeometry(activity.Span{ID: "missing-end", StartedAt: start, State: activity.Unknown}, now.Add(-30*time.Minute), now, 20, time.Time{}, false)
	if !visible || !point || left != right {
		test.Fatalf("missing endpoint became a bar: left=%d right=%d point=%v visible=%v", left, right, point, visible)
	}
	_, _, _, visible = projectAgentsSpanGeometry(activity.Span{ID: "outside", StartedAt: now.Add(-2 * time.Hour), EndedAt: timePtr(now.Add(-90 * time.Minute)), State: activity.Finished}, now.Add(-30*time.Minute), now, 20, time.Time{}, false)
	if visible {
		test.Fatal("out-of-window span was rendered")
	}
	_, _, point, visible = projectAgentsSpanGeometry(activity.Span{ID: "invalid", State: activity.Unknown}, now.Add(-30*time.Minute), now, 20, time.Time{}, false)
	if !visible || !point {
		test.Fatal("missing start did not use a point marker")
	}
}

func TestProjectAgentsStaleSpanClipsAtObservation(test *testing.T) {
	now := time.Date(2026, 10, 17, 12, 30, 0, 0, time.UTC)
	start := now.Add(-20 * time.Minute)
	observedThrough := now.Add(-5 * time.Minute)
	left, right, point, visible := projectAgentsSpanGeometry(activity.Span{ID: "stale", StartedAt: start, State: activity.Unknown}, now.Add(-30*time.Minute), now, 31, observedThrough, true)
	if !visible || point || left != 10 || right != 25 {
		test.Fatalf("stale span geometry = %d,%d point=%v visible=%v", left, right, point, visible)
	}
	left, right, point, visible = projectAgentsSpanGeometry(activity.Span{ID: "fresh", StartedAt: start, State: activity.Running}, now.Add(-30*time.Minute), now, 31, now, true)
	if !visible || point || left != 10 || right != 30 {
		test.Fatalf("fresh running geometry = %d,%d point=%v visible=%v", left, right, point, visible)
	}
	endedAfterWindow := now.Add(10 * time.Minute)
	left, right, point, visible = projectAgentsSpanGeometry(activity.Span{ID: "crossing", StartedAt: start, EndedAt: &endedAfterWindow, State: activity.Finished}, now.Add(-30*time.Minute), now, 31, time.Time{}, false)
	if !visible || point || left != 10 || right != 30 {
		test.Fatalf("crossing span geometry = %d,%d point=%v visible=%v", left, right, point, visible)
	}
}

func TestProjectAgentsActiveCountNeedsFreshObservation(test *testing.T) {
	now := time.Date(2026, 10, 17, 12, 30, 0, 0, time.UTC)
	snapshot := projectview.AgentsSnapshot{Rows: []projectview.AgentRow{
		{ThreadID: "fresh", State: "active", ObservedAt: now.Add(-time.Minute)},
		{ThreadID: "stale", State: "active", ObservedAt: now.Add(-projectAgentsFreshFor - time.Second)},
	}}
	out := ansi.Strip(RenderProjectAgents(snapshot, 80, 6, "fresh", projectAgentsDefaultView, now, true, theme.Default()))
	if !strings.Contains(out, "1 active") {
		test.Fatalf("fresh active count missing: %q", out)
	}
	if strings.Contains(out, "2 active") {
		test.Fatalf("stale active row was counted: %q", out)
	}
}

func TestProjectAgentsStaleAndUnavailableActiveRowsAreUnknown(test *testing.T) {
	now := time.Date(2026, 10, 17, 12, 30, 0, 0, time.UTC)
	snapshot := projectview.AgentsSnapshot{Rows: []projectview.AgentRow{{
		ThreadID:   "stale",
		Name:       "stale-agent",
		State:      "active",
		ObservedAt: now.Add(-projectAgentsFreshFor - time.Second),
		Spans: []projectview.AgentSpan{{
			Source: activity.SourcePi,
			Span:   activity.Span{ID: "stale-turn", StartedAt: now.Add(-10 * time.Minute), State: activity.Running},
		}},
	}}}
	stale := ansi.Strip(RenderProjectAgents(snapshot, 100, 8, "stale", projectAgentsDefaultView, now, true, theme.Default()))
	if !strings.Contains(stale, "unknown") || !strings.Contains(stale, "?") {
		test.Fatalf("stale active row made a live claim: %q", stale)
	}
	if strings.Contains(stale, "1 active") {
		test.Fatalf("stale active row counted as live: %q", stale)
	}
	unavailable := ansi.Strip(renderProjectAgents(snapshot, 100, 8, "stale", projectAgentsDefaultView, now, true, theme.Default(), "socket offline", false, ""))
	if !strings.Contains(unavailable, "unknown") || !strings.Contains(unavailable, "?") {
		test.Fatalf("unavailable active row made a live claim: %q", unavailable)
	}
}

func TestProjectAgentsCoverageAndPartialStatusStayVisible(test *testing.T) {
	now := time.Date(2026, 10, 17, 12, 30, 0, 0, time.UTC)
	snapshot := projectview.AgentsSnapshot{
		ProjectName:    strings.Repeat("project-", 12),
		Partial:        true,
		DroppedThreads: 4,
		DroppedSpans:   8,
		Rows: []projectview.AgentRow{{
			ThreadID:   "thread",
			Name:       "build",
			Harness:    "pi",
			Provider:   "openai",
			Model:      strings.Repeat("model-name-", 8),
			State:      "idle",
			Coverage:   activity.CoverageCurrentProcess,
			ObservedAt: now,
		}},
	}
	out := ansi.Strip(RenderProjectAgents(snapshot, 80, 8, "thread", projectAgentsDefaultView, now, true, theme.Default()))
	if !strings.Contains(out, "partial") || !strings.Contains(out, "dropped 4 threads") || !strings.Contains(out, "dropped 8 spans") {
		test.Fatalf("partial coverage was hidden by project label: %q", out)
	}
	if !strings.Contains(out, "Idle") || !strings.Contains(out, "current session only") {
		test.Fatalf("row state/coverage metadata was hidden: %q", out)
	}
}

func TestProjectAgentsWideBarIncludesBoundedNativeIdentifier(test *testing.T) {
	now := time.Date(2026, 10, 17, 12, 30, 0, 0, time.UTC)
	ended := now.Add(-time.Minute)
	snapshot := projectview.AgentsSnapshot{Rows: []projectview.AgentRow{{
		ThreadID:   "thread",
		Name:       "build",
		State:      "idle",
		ObservedAt: now,
		Spans: []projectview.AgentSpan{{
			Source: activity.SourceCodex,
			Span:   activity.Span{ID: "native-turn-identifier", StartedAt: now.Add(-20 * time.Minute), EndedAt: &ended, State: activity.Finished},
		}},
	}}}
	out := ansi.Strip(RenderProjectAgents(snapshot, 120, 8, "thread", projectAgentsDefaultView, now, true, theme.Default()))
	if !strings.Contains(out, "native-turn-ide") {
		test.Fatalf("wide bar omitted bounded native identifier: %q", out)
	}
	if strings.Contains(out, "native-turn-identifier") {
		test.Fatalf("wide bar exceeded identifier bound: %q", out)
	}
}

func TestProjectAgentsNarrowViewAndSafety(test *testing.T) {
	now := time.Date(2026, 10, 17, 12, 30, 0, 0, time.UTC)
	snapshot := projectview.AgentsSnapshot{Rows: []projectview.AgentRow{{
		ThreadID: "thread-1",
		Name:     "bad\x1b[31mname\nnext",
		Harness:  "harness",
		State:    "idle",
	}}}
	out := RenderProjectAgents(snapshot, 50, 6, "thread-1", projectAgentsDefaultView, now, true, theme.Default())
	if strings.Contains(out, "\x1b[31m") || strings.Contains(out, "\nnext") {
		test.Fatalf("unsafe label escaped the renderer: %q", out)
	}
	if strings.Contains(ansi.Strip(out), "native activity:") {
		test.Fatalf("narrow view rendered the timeline detail: %q", out)
	}
	for _, line := range strings.Split(out, "\n") {
		if ansi.StringWidth(line) > 50 {
			test.Fatalf("narrow line overflow: %d: %q", ansi.StringWidth(line), line)
		}
	}
}

func TestProjectAgentsPartialCoveragePointsAndASCII(test *testing.T) {
	now := time.Date(2026, 10, 17, 12, 30, 0, 0, time.UTC)
	snapshot := projectview.AgentsSnapshot{
		Partial:        true,
		DroppedThreads: 2,
		DroppedSpans:   3,
		Reason:         "bounded source",
		Rows: []projectview.AgentRow{{
			ThreadID: "thread-1",
			Name:     "build",
			Harness:  "pi",
			State:    "stale",
			Coverage: activity.CoverageCurrentProcess,
			Points: []projectview.AgentPoint{{
				At:     now.Add(-2 * time.Minute),
				State:  activity.Unknown,
				Source: activity.SourcePi,
			}},
			Spans: []projectview.AgentSpan{{
				Source: activity.SourcePi,
				Span:   activity.Span{ID: "finished", StartedAt: now.Add(-10 * time.Minute), EndedAt: timePtr(now.Add(-8 * time.Minute)), State: activity.Finished},
			}},
		}},
	}
	out := ansi.Strip(RenderProjectAgents(snapshot, 100, 10, "thread-1", projectAgentsDefaultView, now, true, theme.Default()))
	for _, want := range []string{"partial", "dropped 2 threads", "dropped 3 spans", "current session only", "Session changed", "=", "o"} {
		if !strings.Contains(out, want) {
			test.Fatalf("ASCII renderer omitted %q: %q", want, out)
		}
	}
	for _, forbidden := range []string{"●", "·", "↑", "↓", "…", "━"} {
		if strings.Contains(out, forbidden) {
			test.Fatalf("ASCII renderer emitted %q: %q", forbidden, out)
		}
	}
}

func TestProjectAgentsSelectionSurvivesReorder(test *testing.T) {
	model := newProjectAgentsModel(context.Background(), ProjectAgentsOptions{Load: func(context.Context) (projectview.AgentsSnapshot, error) { return projectview.AgentsSnapshot{}, nil }})
	model.snapshot.Rows = []projectview.AgentRow{{ThreadID: "one"}, {ThreadID: "two"}}
	model.selected = "two"
	model.snapshot.Rows = []projectview.AgentRow{{ThreadID: "two"}, {ThreadID: "one"}}
	model.reconcileSelection()
	if model.selected != "two" {
		test.Fatalf("selection changed identity after reorder: %q", model.selected)
	}
}

func TestProjectAgentsActionsUseExactIdentity(test *testing.T) {
	var gotThread, gotRun string
	model := newProjectAgentsModel(context.Background(), ProjectAgentsOptions{
		Load: func(context.Context) (projectview.AgentsSnapshot, error) { return projectview.AgentsSnapshot{}, nil },
		Focus: func(_ context.Context, threadID, runID string) error {
			gotThread, gotRun = threadID, runID
			return nil
		},
	})
	model.snapshot.Rows = []projectview.AgentRow{{ThreadID: "thread", RunID: "run"}}
	model.selected = "thread"
	updatedModel, command := model.Update(tea.KeyPressMsg{Text: "enter"})
	if command == nil || updatedModel != model {
		test.Fatal("enter did not schedule focus")
	}
	if message := command(); message == nil {
		test.Fatal("focus command returned no result")
	}
	if gotThread != "thread" || gotRun != "run" {
		test.Fatalf("focus callback got %q/%q", gotThread, gotRun)
	}
}

func TestProjectAgentsUnboundActionExplainsWithoutCallingSurface(test *testing.T) {
	called := false
	callback := func(context.Context, string, string) error {
		called = true
		return nil
	}
	model := newProjectAgentsModel(context.Background(), ProjectAgentsOptions{Focus: callback, OpenEvidence: callback})
	model.snapshot.Rows = []projectview.AgentRow{{ThreadID: "thread", State: "unbound", Enrollment: "unbound"}}
	model.selected = "thread"
	for _, evidence := range []bool{false, true} {
		if command := model.startAction(evidence); command != nil {
			command()
			test.Fatal("unbound row scheduled a surface action")
		}
	}
	if called || !strings.Contains(ansi.Strip(model.footer), "No session connected") {
		test.Fatalf("missing honest unbound explanation: called=%v footer=%q", called, model.footer)
	}
}

func TestProjectAgentsLoadErrorFreezesActiveClaim(test *testing.T) {
	now := time.Date(2026, 10, 17, 12, 30, 0, 0, time.UTC)
	model := newProjectAgentsModel(context.Background(), ProjectAgentsOptions{Load: func(context.Context) (projectview.AgentsSnapshot, error) {
		return projectview.AgentsSnapshot{}, errors.New("offline")
	}})
	model.snapshot = projectview.AgentsSnapshot{Rows: []projectview.AgentRow{{ThreadID: "thread", State: "active", ObservedAt: now}}}
	model.hasSnapshot = true
	model.loading = true
	model.loadSequence = 3
	model.now = func() time.Time { return now }
	updatedModel, _ := model.Update(projectAgentsLoadMsg{sequence: 3, err: errors.New("offline")})
	if updatedModel != model || model.sourceError == "" {
		test.Fatal("load error did not remain visible")
	}
	out := ansi.Strip(model.View().Content)
	if strings.Contains(out, "1 active") {
		test.Fatalf("cached active claim survived load error: %q", out)
	}
}

func TestProjectAgentsStaleLoadIgnored(test *testing.T) {
	model := newProjectAgentsModel(context.Background(), ProjectAgentsOptions{Load: func(context.Context) (projectview.AgentsSnapshot, error) { return projectview.AgentsSnapshot{}, nil }})
	model.loadSequence = 2
	model.snapshot = projectview.AgentsSnapshot{ProjectName: "new"}
	model.hasSnapshot = true
	updatedModel, _ := model.Update(projectAgentsLoadMsg{sequence: 1, snapshot: projectview.AgentsSnapshot{ProjectName: "old"}})
	if updatedModel != model || model.snapshot.ProjectName != "new" {
		test.Fatalf("stale load replaced current snapshot: %q", model.snapshot.ProjectName)
	}
}

func timePtr(value time.Time) *time.Time { return &value }
