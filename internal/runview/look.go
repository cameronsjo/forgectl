// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package runview

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// How a run or step is drawn: one legend, shared by the dashboard and the
// text the agent verbs print, so the two cannot disagree.

// Glyphs are the run and step marks. Each is drawn beside a word, so a mark
// is never the only carrier of a state. Spin holds the running mark's
// animation frames, one rune each.
type Glyphs struct {
	Pending     string
	Running     string
	Done        string
	Failed      string
	Interrupted string
	Rejected    string
	Resumed     string
	Stale       string
	Lost        string
	Skipped     string
	Spin        string
}

// IconGlyphs and ASCIIGlyphs are the two legends: Unicode marks, and the
// fallback for --no-icons.
var (
	IconGlyphs = Glyphs{
		Pending: "·", Running: "◐", Done: "✓", Failed: "✗", Interrupted: "⊘", Rejected: "↺",
		Resumed: "⟲", Stale: "!", Lost: "?", Skipped: "–", Spin: "◐◓◑◒",
	}
	ASCIIGlyphs = Glyphs{
		Pending: ".", Running: "*", Done: "+", Failed: "x", Interrupted: "/", Rejected: "r",
		Resumed: "~", Stale: "!", Lost: "?", Skipped: "-", Spin: "*",
	}
)

// PickGlyphs is the ASCII legend when ascii is set, else the icons.
func PickGlyphs(ascii bool) Glyphs {
	if ascii {
		return ASCIIGlyphs
	}
	return IconGlyphs
}

// Tone is the colour family a mark is drawn in; the renderer maps it to its
// own styles.
type Tone int

// The tones.
const (
	ToneDim Tone = iota
	ToneMuted
	ToneActive
	ToneOK
	ToneDanger
	ToneWarn
)

// Mark is a state's glyph, word and tone.
type Mark struct {
	Glyph, Word string
	Tone        Tone
}

// RunMark is a run's mark from its live state and exit. newRun is a stale
// run that never had a log, which reads as new rather than stale.
func RunMark(g Glyphs, live LiveState, exit *int, newRun bool) Mark {
	switch live {
	case LiveLive:
		return Mark{g.Running, "live", ToneActive}
	case LiveRunning:
		return Mark{g.Running, "running", ToneActive}
	case LiveEnded:
		switch {
		case exit == nil:
			return Mark{g.Pending, "ended", ToneMuted}
		case *exit == 0:
			return Mark{g.Done, "ok", ToneOK}
		}
		return Mark{g.Failed, strconv.Itoa(*exit), ToneDanger}
	case LiveStale:
		if newRun {
			return Mark{g.Pending, "new", ToneDim}
		}
		return Mark{g.Stale, "stale", ToneWarn}
	case LiveLost:
		return Mark{g.Lost, "lost", ToneDanger}
	case LiveWaiting:
		return Mark{g.Pending, "waiting", ToneDim}
	case LiveSkipped:
		return Mark{g.Skipped, "skipped", ToneDim}
	}
	return Mark{g.Pending, "pending", ToneDim}
}

// StepMark is a step's mark from its status and, for a desk batch, the
// runner's own word for it (StepTiming.State): a step the runner skipped or
// cancelled says so.
func StepMark(g Glyphs, st StepStatus, runnerState string) Mark {
	switch {
	case runnerState == "skipped" && st == StepPending:
		return Mark{g.Skipped, "skipped", ToneDim}
	case runnerState == "cancelled" && st != StepClosed:
		return Mark{g.Skipped, "cancelled", ToneDim}
	}
	switch st {
	case StepRunning:
		return Mark{g.Running, "running", ToneActive}
	case StepClosed:
		return Mark{g.Done, "done", ToneOK}
	case StepFailed:
		return Mark{g.Failed, "failed", ToneDanger}
	case StepInterrupted:
		return Mark{g.Interrupted, "interrupted", ToneWarn}
	}
	return Mark{g.Pending, "pending", ToneDim}
}

// RunnerState is the runner's word for step id in timing, or "".
func RunnerState(timing []StepTiming, id string) string {
	for _, t := range timing {
		if t.Step == id {
			return t.State
		}
	}
	return ""
}

// Age is a duration in one unit: 12s, 4m, 3h, 2d. Zero and negative
// durations are empty.
func Age(d time.Duration) string {
	switch {
	case d <= 0:
		return ""
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d/time.Second))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	}
	return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
}

// RunOrder is what the run order compares: live runs first, then the newest
// activity, then name and source.
type RunOrder struct {
	Live         bool
	Activity     time.Time
	Name, Source string
}

// CompareRuns orders two runs as the dashboard lists them.
func CompareRuns(a, b RunOrder) int {
	if a.Live != b.Live {
		if a.Live {
			return -1
		}
		return 1
	}
	if !a.Activity.Equal(b.Activity) {
		if a.Activity.After(b.Activity) {
			return -1
		}
		return 1
	}
	return strings.Compare(a.Name+"\x00"+a.Source, b.Name+"\x00"+b.Source)
}
