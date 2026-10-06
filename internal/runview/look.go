// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package runview

import (
	"strconv"
	"strings"
	"time"
)

// How a run or step is drawn: one legend, shared by the dashboard and the
// text `desk runs` and `desk show` print, so the two cannot disagree.

// Glyphs are the run and step marks. Each is drawn beside a word, so a mark
// is never the only carrier of a state.
type Glyphs struct {
	Pending     string
	Running     string
	Done        string
	Failed      string
	Interrupted string
	Lost        string
	Skipped     string
	Changed     string
}

// IconGlyphs and ASCIIGlyphs are the two legends: Unicode marks, and the
// fallback for a terminal that cannot draw them.
var (
	IconGlyphs = Glyphs{
		Pending: "·", Running: "◐", Done: "✓", Failed: "✗", Interrupted: "⊘", Lost: "?", Skipped: "–", Changed: "!",
	}
	ASCIIGlyphs = Glyphs{
		Pending: ".", Running: "*", Done: "+", Failed: "x", Interrupted: "/", Lost: "?", Skipped: "-", Changed: "!",
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

// RunMark is a run's mark from its live state and exit.
func RunMark(g Glyphs, live LiveState, exit *int) Mark {
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
		return Mark{g.Failed, "exit " + strconv.Itoa(*exit), ToneDanger}
	case LiveLost:
		return Mark{g.Lost, "lost", ToneDanger}
	case LiveWaiting:
		return Mark{g.Pending, "waiting", ToneDim}
	case LiveSkipped:
		return Mark{g.Skipped, "skipped", ToneDim}
	case LiveChanged:
		return Mark{g.Changed, "changed", ToneWarn}
	case LiveUnknown:
		return Mark{g.Pending, "log", ToneMuted}
	}
	return Mark{g.Pending, "pending", ToneDim}
}

// StepMark is a step's mark from its status and, for a desk batch, the
// runner's own word for it (StepTiming.State): a step the runner skipped or
// cancelled says so.
func StepMark(g Glyphs, st StepStatus, runnerState string) Mark {
	switch {
	case st == StepSkipped || (runnerState == "skipped" && st == StepPending):
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

// StepDur is how long step id took or has taken: the timing's duration when
// it has one, else End minus Start from the fold, else zero.
func StepDur(timing []StepTiming, st StepState) time.Duration {
	for _, t := range timing {
		if t.Step == st.ID && t.Dur > 0 {
			return t.Dur
		}
	}
	if !st.Start.IsZero() && !st.End.IsZero() {
		return st.End.Sub(st.Start)
	}
	return 0
}

// RunOrder is what the run order compares: live runs first, then the newest
// activity, then name and source.
type RunOrder struct {
	Live         bool
	Activity     time.Time
	Name, Source string
}

// CompareRuns orders two runs as `desk runs` and the dashboard list them.
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
