// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

//go:build unix

package tui

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/cameronsjo/forgectl/internal/runview"
	"github.com/cameronsjo/forgectl/internal/theme"
)

const flowLens = `format = "text"
[[step]]
id = "fetch"
note = "clones the repo"
[[step]]
id = "build"
[[step]]
id = "deploy"
note = "ships to staging"
[[rule]]
action = "start"
match = '^go (?P<step>\w+)'
[[rule]]
action = "close"
match = '^ok (?P<step>\w+)'
[[rule]]
action = "end"
match = '^done rc=(?P<exit>\d+)'
`

// flowWatch opens the live view in the flowchart layout on a log read from
// a stream, sized width x height, and loads it.
func flowWatch(t *testing.T, log string, width, height int, ascii bool) string {
	t.Helper()
	lens, err := runview.ParseLens("app", []byte(flowLens))
	if err != nil {
		t.Fatal(err)
	}
	src, err := runview.NewReaderSource("stdin", strings.NewReader(log), lens, runview.LogKeys{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := src.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	w := newRunWatchModel(t.Context(), src, RunWatchOptions{Poll: time.Millisecond, Theme: theme.Default(), ASCII: ascii, Flowchart: true})
	next, _ := w.Update(tea.WindowSizeMsg{Width: width, Height: height})
	w = driveWatch(t, next.(runWatchModel), w.Init()) //nolint:forcetypeassert // test
	return ansi.Strip(w.View().Content)
}

func TestFlowchartDrawsStepsTopDownBesideTheTimeline(t *testing.T) {
	view := flowWatch(t, "go fetch\nok fetch\ngo build\n", 120, 40, false)
	for _, want := range []string{"Start", "End", "┌", "fetch", "✓ done", "clones the repo", "build", "running", "← now", "deploy", "pending", "ships to staging", " │ ", "events", "go build"} {
		if !strings.Contains(view, want) {
			t.Errorf("the flowchart is missing %q:\n%s", want, view)
		}
	}
	lines := strings.Split(view, "\n")
	at := func(s string) int {
		for i, l := range lines {
			if strings.Contains(l, s) {
				return i
			}
		}
		return -1
	}
	// Each step's box line carries its state, so the states give its row.
	rows := []int{at("Start"), at("✓ done"), at("◐ running"), at("· pending"), at("End")}
	if !slices.IsSorted(rows) || slices.Contains(rows, -1) {
		t.Errorf("the steps are not drawn top to bottom from Start to End:\n%s", view)
	}
	// The timeline is beside the chart: an event shares a line with it.
	if l := lines[at("go build")]; !strings.Contains(l, "│") {
		t.Errorf("a wide window should put the timeline beside the chart, got %q", l)
	}
}

func TestFlowchartEndShowsTheExit(t *testing.T) {
	view := flowWatch(t, "go fetch\nok fetch\ngo build\ndone rc=3\n", 120, 40, false)
	if !strings.Contains(view, "exit 3") {
		t.Errorf("the end terminal should show the exit:\n%s", view)
	}
	if strings.Contains(view, "← now") {
		t.Errorf("a finished run has no running step to mark now:\n%s", view)
	}
}

func TestFlowchartNarrowPutsTheTimelineBelow(t *testing.T) {
	view := flowWatch(t, "go fetch\n", 60, 40, false)
	lines := strings.Split(view, "\n")
	end, ev := -1, -1
	for i, l := range lines {
		if strings.Contains(l, "End") && end < 0 {
			end = i
		}
		if strings.Contains(l, "go fetch") {
			ev = i
		}
	}
	if end < 0 || ev < end {
		t.Errorf("a narrow window should put the timeline below the chart (End at %d, event at %d):\n%s", end, ev, view)
	}
}

func TestFlowchartASCII(t *testing.T) {
	view := flowWatch(t, "go fetch\n", 120, 40, true)
	for _, glyph := range []string{"┌", "┐", "└", "┘", "┬", "╭", "↓", "←", "─"} {
		if strings.Contains(view, glyph) {
			t.Errorf("--no-icons left %q in the flowchart:\n%s", glyph, view)
		}
	}
	if !strings.Contains(view, "+------") {
		t.Errorf("the ASCII flowchart should still draw boxes:\n%s", view)
	}
}

// A chart taller than the window keeps the running step in view.
func TestFlowWindowKeepsTheFocusInView(t *testing.T) {
	chart := make([]string, 40)
	for i := range chart {
		chart[i] = string(rune('a' + i%26))
	}
	got := flowWindow(chart, 30, 10)
	if len(got) != 10 || got[0] != chart[27] {
		t.Errorf("flowWindow(focus 30, 10 rows) starts at %q, want %q", got[0], chart[27])
	}
	if got := flowWindow(chart, 39, 10); got[9] != chart[39] {
		t.Errorf("a focus near the end should show the end, got %v", got)
	}
	if got := flowWindow(chart[:5], 2, 10); len(got) != 5 {
		t.Errorf("a short chart should be shown whole, got %d lines", len(got))
	}
}

// Below the side-by-side width, the chart gets the rows a short timeline
// does not need, so a small run's End is not cut off.
func TestFlowchartNarrowGivesTheChartTheSpareRows(t *testing.T) {
	view := flowWatch(t, "go fetch\n", 99, 30, false)
	if !strings.Contains(view, "End") {
		t.Errorf("at 99x30 the End box was cut off:\n%s", view)
	}
}

// A finished run with no running step scrolls the chart to its End.
func TestFlowchartFocusesTheEndOfAFinishedRun(t *testing.T) {
	view := flowWatch(t, "go fetch\nok fetch\ndone rc=0\n", 120, 14, false)
	if !strings.Contains(view, "End") || !strings.Contains(view, "ok") {
		t.Errorf("a finished run's chart should show its End:\n%s", view)
	}
}
