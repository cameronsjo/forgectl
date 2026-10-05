// Package ready decides whether a coordinator worker's harness is at its
// input prompt, from the pane's visible text and herdr's agent status.
//
// herdr's status is a hint, never proof: the 2026-09-28 trial saw it report
// `blocked` at an idle prompt and `idle` while a model was still loading. So a
// harness is ready only when three things agree: no known blocking screen is
// showing, herdr names the expected agent as idle or done, and the harness's
// own input prompt is visible. The decision is a pure function of its inputs;
// reading the pane is the caller's job.
//
// This guards against accidents, not a hostile worker. All three signals can
// be set from inside the worker's pane: the screen is whatever the pane draws,
// and any pane can report its own herdr agent status. A caller that must not
// be steered by the worker (a first brief, an approval) needs a channel the
// worker cannot draw on.
package ready

import (
	"fmt"
	"strings"
)

// Screen is one observation of a worker's pane.
type Screen struct {
	// Text is the pane's visible text. It is untrusted: it is whatever the
	// pane displays, so it is matched against, never rendered or logged raw.
	Text string
	// Agent is the agent herdr detected in the pane, or "" for none.
	Agent string
	// Status is herdr's agent_status for the pane: idle, done, working,
	// blocked, or unknown.
	Status string
}

// State is the outcome class of one evaluation.
type State string

const (
	// StateReady: the input prompt is visible and nothing disagrees.
	StateReady State = "ready"
	// StateBlocked: a known blocking screen is showing. Blocking names it.
	StateBlocked State = "blocked"
	// StateNotReady: no blocking screen, but some signal disagrees: the
	// prompt is not visible, or herdr reports another agent or state.
	StateNotReady State = "not-ready"
)

// Verdict is the result of [Table.Evaluate].
type Verdict struct {
	State State `json:"state"`
	// Blocking is the name of the blocking screen seen, when State is
	// StateBlocked.
	Blocking string `json:"blocking,omitempty"`
	// Reason says which signal disagreed, naming what was expected and what
	// was seen. Empty when ready.
	Reason string `json:"reason,omitempty"`
	// Input is the text already in the harness's input box when the prompt
	// matched, or "". A brief must not be typed over it.
	Input string `json:"input,omitempty"`
}

// Ready reports whether the verdict allows typing into the pane.
func (v Verdict) Ready() bool { return v.State == StateReady }

// statusReady is herdr's agent_status values that mean "at its input". Both
// mean ready for input; `done` is "finished and not yet looked at", and it
// flips to idle when anyone views the pane.
var statusReady = map[string]bool{"idle": true, "done": true}

// Has reports whether the table has predicates for harness. A harness it
// does not know can never become ready, so a caller checks this before
// waiting rather than polling a fixed failure.
func (t *Table) Has(harness string) bool {
	_, ok := t.harnesses[harness]
	return ok
}

// Evaluate decides readiness for harness from one screen.
//
// For a prompt_first harness, an input prompt anchored at the bottom of the
// screen wins: that harness draws every dialog in place of its input box, so
// text above the box is transcript and blocking patterns there would match a
// past message. Otherwise blocking screens are checked first, over the whole
// screen. Either way herdr's agent and status must agree before a visible
// prompt counts, and a disagreement is reported rather than hidden.
func (t *Table) Evaluate(harness string, s Screen) Verdict {
	h, ok := t.harnesses[harness]
	if !ok {
		return Verdict{State: StateNotReady, Reason: fmt.Sprintf("no readiness predicates for harness %q", harness)}
	}
	p, prompted := h.matchPrompt(s.Text)
	var b string
	var blocked bool
	switch {
	case !h.promptFirst || !prompted:
		b, blocked = t.blockingFor(h, s.Text, false)
	default:
		// The box shows. Rows under it are footer, where no dialog row should
		// ever match; an overlay that keeps the box visible needs with_prompt.
		if b, blocked = t.blockingFor(h, p.footer, false); !blocked {
			b, blocked = t.blockingFor(h, s.Text, true)
		}
	}
	if blocked {
		return Verdict{State: StateBlocked, Blocking: b, Reason: "showing the " + b}
	}
	if !prompted {
		return Verdict{State: StateNotReady, Reason: "the " + harness + " input prompt is not visible"}
	}
	if s.Agent != h.agent {
		return Verdict{State: StateNotReady, Reason: fmt.Sprintf("the %s input prompt is visible, but herdr detects agent %q in the pane, want %q", harness, s.Agent, h.agent)}
	}
	if !statusReady[s.Status] {
		return Verdict{State: StateNotReady, Reason: fmt.Sprintf("the %s input prompt is visible, but herdr reports agent status %q, want idle or done", harness, s.Status)}
	}
	return Verdict{State: StateReady, Input: p.input}
}

// blockingFor returns the first blocking screen that matches text, the
// harness's own before the shared ones. With onlyWithPrompt it considers only
// rows marked with_prompt.
func (t *Table) blockingFor(h harness, text string, onlyWithPrompt bool) (string, bool) {
	for _, rows := range [][]screen{h.blocking, t.blocking} {
		for _, b := range rows {
			if onlyWithPrompt && !b.withPrompt {
				continue
			}
			if b.matches(text) {
				return b.name, true
			}
		}
	}
	return "", false
}

// promptMatch is one match of a harness's prompt pattern.
type promptMatch struct {
	input  string
	footer string
}

// matchPrompt finds the input prompt, what is typed in it, and the footer
// rows under it. The patterns anchor at the end of the screen, so there is at
// most one match. Wrapped input rows are joined with single spaces, and a
// placeholder the harness shows in an empty box reads as no input.
func (h harness) matchPrompt(text string) (promptMatch, bool) {
	m := h.prompt.FindStringSubmatch(text)
	if m == nil {
		return promptMatch{}, false
	}
	var p promptMatch
	if i := h.prompt.SubexpIndex("input"); i >= 0 {
		p.input = strings.Join(strings.Fields(m[i]), " ")
	}
	if i := h.prompt.SubexpIndex("footer"); i >= 0 {
		p.footer = m[i]
	}
	if h.placeholder != nil && h.placeholder.MatchString(p.input) {
		p.input = ""
	}
	return p, true
}
