package tui

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/cameronsjo/forgectl/internal/desk"
	"github.com/cameronsjo/forgectl/internal/termsafe/termsafetest"
	"github.com/cameronsjo/forgectl/internal/theme"
)

// timelineSnapshot is busySnapshot spread over three days, with a skip made
// by the operator and a lost run that was cleared, so every kind of entry
// and every kind of heading shows.
func timelineSnapshot() (*desk.Snapshot, DeskFrameOptions) {
	snap, opts := busySnapshot()
	skipped := item("11-prune-branches", desk.KindScript, desk.StateSkipped)
	skipped.Headers = desk.Headers{What: "Prune merged branches", Why: "Tidy up"}
	skipped.Meta = desk.Meta{AddedAt: agoPtr(26 * time.Hour), SkipReason: desk.SkipOperator, SkippedBy: "dashboard",
		SkipNote: "superseded by 12", SkippedAt: agoPtr(25 * time.Hour)}
	cleared := item("08-deploy-docs", desk.KindBatch, desk.StateSkipped)
	cleared.Headers = desk.Headers{What: "Deploy the docs site", Why: "Release day"}
	cleared.Meta = desk.Meta{AddedAt: agoPtr(50 * time.Hour), SkipReason: desk.SkipLost, SkippedAt: agoPtr(49 * time.Hour)}
	snap.Skipped = append(snap.Skipped, skipped, cleared)
	return snap, opts
}

// tlSeenAt is the fixtures' last look at the timeline: the failed canary
// probe and both running items came after it.
var tlSeenAt = ago(22 * time.Minute)

func renderTimeline(snap *desk.Snapshot, opts DeskFrameOptions, width, height, cursor int) string {
	out, _ := tlFrame{snap: snap, steps: opts.Steps, width: width, height: height, now: deskNow, seen: tlSeenAt, theme: opts.Theme, cursor: cursor}.render()
	return out
}

func TestGoldenDeskTimeline(t *testing.T) {
	forceTrueColor(t)
	for _, w := range []int{60, 120} {
		snap, opts := timelineSnapshot()
		assertGolden(t, fmt.Sprintf("desk_timeline_%d", w), renderTimeline(snap, opts, w, 40, 0))
	}
}

func TestGoldenDeskTimelineNoColor(t *testing.T) {
	for _, w := range []int{60, 120} {
		snap, opts := timelineSnapshot()
		var buf bytes.Buffer
		wr := theme.Default().Writer(&buf, []string{"NO_COLOR=1", "TERM=xterm-256color"})
		if _, err := wr.Write([]byte(renderTimeline(snap, opts, w, 40, 0))); err != nil {
			t.Fatal(err)
		}
		assertGolden(t, fmt.Sprintf("desk_timeline_%d_nocolor", w), buf.String())
	}
}

// What needs the operator leads, under its own heading, whatever its day;
// the rest follows newest first, grouped by day.
func TestDeskTimeline_NeedsYouLeadsThenNewestFirst(t *testing.T) {
	snap, _ := timelineSnapshot()
	var names []string
	for _, e := range deskTimeline(snap) {
		names = append(names, e.row.item.Name)
	}
	want := []string{
		"18-refresh-credentials", "17-merge-feature-branch", "19-old-cleanup", // needs you
		"14-sync-mirror", "16-nightly-batch", "12-canary-probe", "15-merge-1169", "13-rotate-keys", "09-sync-mirror", // today
		"11-prune-branches", // yesterday
		"08-deploy-docs",    // Sat 3 Oct
	}
	if strings.Join(names, " ") != strings.Join(want, " ") {
		t.Errorf("order:\n got %v\nwant %v", names, want)
	}
	out := ansi.Strip(renderTimelineDefault())
	for _, h := range []string{"Needs you", "Today · Mon 5 Oct", "Yesterday · Sun 4 Oct", "Sat 3 Oct"} {
		if !strings.Contains(out, h) {
			t.Errorf("no %q heading:\n%s", h, out)
		}
	}
}

// renderTimelineDefault is the fixture at 120x60 with the newest entry
// selected.
func renderTimelineDefault() string {
	snap, opts := timelineSnapshot()
	return renderTimeline(snap, opts, 120, 60, 0)
}

// Each entry says what became of it in plain words: no exit codes without
// "failed", no raw skip reasons.
func TestDeskTimeline_SaysWhatHappenedInPlainWords(t *testing.T) {
	out := ansi.Strip(renderTimelineDefault())
	for _, want := range []string{
		"waiting for you · queued 12m ago",
		"waiting for you · queued 5m ago · runs in the desk's pane",
		"waiting for you · queued 30h ago · stale",
		"running · 4 of 7 steps · 3:00 so far",
		"failed · exit 1 after 0:18",
		"ran ok in 0:41",
		"not run · its bytes changed after it was queued",
		"skipped by you: superseded by 12",
		"cleared after it was lost mid-run",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("timeline lacks %q:\n%s", want, out)
		}
	}
}

// "new" marks what happened after the last look, and nothing else; an
// entry that needs you never carries it.
func TestDeskTimeline_NewMarksOnlyWhatCameAfterTheLastLook(t *testing.T) {
	out := ansi.Strip(renderTimelineDefault())
	newOn := map[string]bool{}
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "• new") {
			for _, title := range []string{"Sync the package mirror", "Rebuild the nightly artifacts", "Probe the canary", "Merge 1169", "Merge the feature branch"} {
				if strings.Contains(l, title) {
					newOn[title] = true
				}
			}
		}
	}
	for title, want := range map[string]bool{
		"Sync the package mirror": true, "Rebuild the nightly artifacts": true, "Probe the canary": true,
		"Merge 1169": false, "Merge the feature branch": false,
	} {
		if newOn[title] != want {
			t.Errorf("%s: new = %v, want %v\n%s", title, newOn[title], want, out)
		}
	}
	if !strings.Contains(strings.SplitN(out, "\n", 2)[0], "◌ 3 need you · ● 2 running · • 3 new") {
		t.Errorf("header: %q", strings.SplitN(out, "\n", 2)[0])
	}
}

// The rail joins a day's entries and stops at its last one.
func TestDeskTimeline_RailStopsAtTheLastEntryOfADay(t *testing.T) {
	lines := strings.Split(ansi.Strip(renderTimelineDefault()), "\n")
	for i, l := range lines {
		if !strings.Contains(l, "ran ok in 2:00") && !strings.Contains(l, "ran ok in 0:41") {
			continue
		}
		rail := strings.Contains(l, "│")
		last := strings.Contains(l, "2:00") // 09 sync-mirror closes today
		if rail == last {
			t.Errorf("line %d %q: rail %v, last of its day %v", i, l, rail, last)
		}
	}
}

// Every line fits the window, the frame is exactly its height, and the
// selection stays on screen as it moves down a timeline taller than the
// window, which says how much is below.
func TestDeskTimeline_FitsAndScrollsToTheSelection(t *testing.T) {
	snap, opts := timelineSnapshot()
	n := len(deskTimeline(snap))
	for _, w := range []int{40, 60, 120} {
		offset := 0
		for c := range n {
			f := tlFrame{snap: snap, steps: opts.Steps, width: w, height: 14, now: deskNow, seen: tlSeenAt, theme: opts.Theme, cursor: c, offset: offset}
			out, off := f.render()
			offset = off
			lines := strings.Split(ansi.Strip(out), "\n")
			if len(lines) != 14 {
				t.Fatalf("%d cols, cursor %d: %d lines, want 14", w, c, len(lines))
			}
			for _, l := range lines {
				if ansi.StringWidth(l) > w {
					t.Errorf("%d cols: line wider than the window: %q", w, l)
				}
			}
			if !strings.Contains(strings.Join(lines, "\n"), "▸") {
				t.Errorf("%d cols, cursor %d: the selection is off screen:\n%s", w, c, strings.Join(lines, "\n"))
			}
		}
	}
	out := ansi.Strip(renderTimeline(snap, opts, 120, 14, 0))
	if !strings.Contains(out, "more below") {
		t.Errorf("a timeline taller than the window does not say so:\n%s", out)
	}
}

// --no-icons swaps every glyph the timeline draws for one ASCII cell, so the
// layout does not move.
func TestDeskTimeline_ASCIIKeepsTheLayout(t *testing.T) {
	out := ansi.Strip(renderTimelineDefault())
	ascii := asciiFrame(out, true)
	for i, l := range strings.Split(ascii, "\n") {
		for _, r := range l {
			if r > 0x7e && r != '·' && r != '…' {
				t.Errorf("line %d keeps a non-ASCII mark %q: %q", i, r, l)
				break
			}
		}
	}
	a, b := strings.Split(out, "\n"), strings.Split(ascii, "\n")
	for i := range a {
		if ansi.StringWidth(a[i]) != ansi.StringWidth(b[i]) {
			t.Errorf("line %d changed width: %q vs %q", i, a[i], b[i])
		}
	}
}

// Untrusted text (names, WHAT, refusals, skip notes) is drawn inert.
func TestDeskTimeline_DrawsNothingUnsafe(t *testing.T) {
	snap, opts := hostileSnapshot()
	snap.Skipped[0].Meta.SkipNote = termsafetest.Hostile("note")
	snap.Skipped[0].Meta.SkipReason = desk.SkipOperator
	for _, w := range []int{60, 120} {
		for c := range deskTimeline(snap) {
			termsafetest.AssertInert(t, fmt.Sprintf("timeline %d cols, cursor %d", w, c), renderTimeline(snap, opts, w, 60, c))
		}
	}
	if out := renderTimeline(snap, opts, 200, 60, 0); !strings.Contains(out, `what\x1b[31m`) {
		t.Fatalf("the hostile WHAT is not in the timeline; the inertness check proves nothing:\n%s", ansi.Strip(out))
	}
}

func TestDeskTimeline_EmptySaysHowItFills(t *testing.T) {
	snap, opts := emptySnapshot()
	out := ansi.Strip(renderTimeline(snap, opts, 100, 12, 0))
	if !strings.Contains(out, "forgectl desk add") || !strings.Contains(out, "all caught up") {
		t.Errorf("empty timeline:\n%s", out)
	}
}
