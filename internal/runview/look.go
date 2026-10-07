// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package runview

import (
	"fmt"
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

// Gist is the run in one plain line, the answer to "what is going on": the
// step that failed and why, else how the run ended, else what is running
// and for how long. events are the run's events up to the state shown; now
// is the clock to count a running step from, or zero to leave that out (a
// replay, whose state is not now). It is "" when there is nothing to say yet.
func Gist(s RunState, live LiveState, events []Event, now time.Time) string {
	var parts []string
	for _, st := range s.Steps {
		if st.Status != StepFailed {
			continue
		}
		p := st.ID + " failed"
		if why, at := failure(events, st.ID); why != "" || !at.IsZero() {
			if !at.IsZero() {
				p += " at " + at.UTC().Format("15:04:05") // as the timeline shows times
			}
			if why != "" {
				p += ": " + why
			}
		}
		parts = append(parts, p)
		break // the first failure is the one to read; the flow shows the rest
	}
	total, done := len(s.Steps), 0
	var running []string
	var since time.Time
	for _, st := range s.Steps {
		switch st.Status {
		case StepClosed:
			done++
		case StepRunning:
			running = append(running, st.ID)
			if since.IsZero() || (!st.Start.IsZero() && st.Start.Before(since)) {
				since = st.Start
			}
		}
	}
	switch live {
	case LiveEnded:
		if len(parts) == 0 {
			p := "finished"
			if total > 0 {
				p += fmt.Sprintf(": %d of %d steps done", done, total)
			}
			parts = append(parts, p)
		}
		if s.Exit != nil {
			parts = append(parts, fmt.Sprintf("exit %d", *s.Exit))
		}
	case LiveLost:
		parts = append(parts, "lost: it stopped with no end")
	case LiveSkipped, LiveChanged, LiveWaiting:
	default:
		if len(running) > 0 {
			p := "running " + strings.Join(running, ", ")
			// Only a run its source calls live has been running until now; a
			// log with no end rule may be yesterday's.
			if (live == LiveLive || live == LiveRunning) && !now.IsZero() && !since.IsZero() && now.After(since) {
				p = "running " + strings.Join(running, ", ") + " for " + now.Sub(since).Round(time.Second).String()
			}
			parts = append(parts, p)
		} else if total > 0 && len(parts) == 0 {
			parts = append(parts, fmt.Sprintf("%d of %d steps done", done, total))
		}
	}
	return strings.Join(parts, " · ")
}

// failure is why step id failed, as the event that failed it says it: its
// name, in plain words when a lens's rule rewrote it, and its time. It is the
// first failing line since the step last started, which is usually the
// cause; the lines after it are often the fallout (a rollback, a retry).
// Only an event a lens classified carries a name worth reading; a desk run's
// STEP-FAIL says nothing the flow does not.
func failure(events []Event, id string) (why string, at time.Time) {
	found := false
	for _, e := range events {
		if e.Step != id {
			continue
		}
		a, _ := e.field(LensActionField)
		switch Action(a) {
		case ActionStart:
			why, at, found = "", time.Time{}, false
		case ActionFail:
			if !found {
				why, at, found = e.Name, e.Time, true
			}
		}
	}
	return why, at
}
