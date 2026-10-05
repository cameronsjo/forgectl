// Package ready decides whether a coordinator worker's harness is at its
// input prompt, from the pane's visible text and herdr's agent status.
//
// herdr's status is a hint, never proof: the 2026-09-28 trial saw it report
// `blocked` at an idle prompt and `idle` while a model was still loading. So a
// harness is ready only when three things agree: no known blocking screen is
// showing, herdr names the expected agent as idle or done, and the harness's
// own input prompt is visible. The decision is a pure function of its inputs;
// reading the pane is the caller's job.
package ready

import "fmt"

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

// Evaluate decides readiness for harness from one screen.
//
// Order matters. Blocking screens are checked first, so a dialog drawn over a
// prompt is never read as the prompt. herdr's status is checked before the
// prompt pattern, so a disagreement is reported as such rather than hidden
// behind a pattern match.
func (t *Table) Evaluate(harness string, s Screen) Verdict {
	h, ok := t.harnesses[harness]
	if !ok {
		return Verdict{State: StateNotReady, Reason: fmt.Sprintf("no readiness predicates for harness %q", harness)}
	}
	for _, b := range h.blocking {
		if b.matches(s.Text) {
			return Verdict{State: StateBlocked, Blocking: b.name, Reason: "showing the " + b.name}
		}
	}
	for _, b := range t.blocking {
		if b.matches(s.Text) {
			return Verdict{State: StateBlocked, Blocking: b.name, Reason: "showing the " + b.name}
		}
	}
	if s.Agent != h.agent {
		return Verdict{State: StateNotReady, Reason: fmt.Sprintf("herdr detects agent %q in the pane, want %q", s.Agent, h.agent)}
	}
	if !statusReady[s.Status] {
		return Verdict{State: StateNotReady, Reason: fmt.Sprintf("herdr reports agent status %q, want idle or done", s.Status)}
	}
	m := h.prompt.FindAllStringSubmatch(s.Text, -1)
	if len(m) == 0 {
		return Verdict{State: StateNotReady, Reason: "the " + harness + " input prompt is not visible"}
	}
	// The last match is the live input box; earlier ones are scrollback.
	last := m[len(m)-1]
	input := ""
	if len(last) > 1 {
		input = last[1]
	}
	return Verdict{State: StateReady, Input: input}
}
