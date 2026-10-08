package drain

import (
	"fmt"

	"github.com/cameronsjo/forgectl/internal/surface/worker"
)

// The claude-slots gate: before each launch the drain runs `claude-slots
// check N`, the machine's cap on live Claude sessions, where N counts this
// launch and every one the tick already made: a session launched moments ago
// may not be registered yet, so the Nth launch of a tick asks for N slots.
// Exit 0 launches, exit 1 holds the row, and anything else (another exit code, a timeout, a run
// that fails to start) launches as if the tool were missing, so a broken
// tool never wedges the queue.

// SlotsCheck is the result of one `claude-slots check N` run.
type SlotsCheck struct {
	// Need is the N the check asked for.
	Need int
	// Stopped reports that the drain is stopping: the check was cut short
	// by the drain's own context, so nothing may launch.
	Stopped bool
	// Exit is the exit code, or -1 when the run ended without one.
	Exit int
	// TimedOut reports that the run hit its time cap.
	TimedOut bool
	// Err is why the run did not start or finish, or empty.
	Err string
	// Reason is the tool's one-line output, already made safe to print.
	Reason string
}

// SlotsDecision is what one check means for the launch it gates.
type SlotsDecision struct {
	// Hold keeps the row from launching: it goes back to queued and the
	// tick claims nothing more.
	Hold bool
	// Reason is the hold's reason, for the row's last_error.
	Reason string
	// Cond names the condition the check leaves the gate in: "" when free,
	// "held", or the kind of failure ("timeout", "exit 2", "run failed").
	// The drain keeps it between checks so an event is recorded once per
	// entry into a condition, not once per tick.
	Cond string
	// Event is recorded when Emit is set.
	Emit  bool
	Event Event
}

// NoSlotsNote is the note the drain records once at start when claude-slots
// is not on PATH.
const NoSlotsNote = "claude-slots not found; launching without the session cap"

// DecideSlots reads one check against the condition the previous check left
// (prev): exit 0 launches; exit 1 holds; anything else launches without the
// cap. An event is due only when the check enters a condition other than
// free that differs from prev.
//
// A stopped check holds the row and leaves the condition as it was, with no
// event: the drain is shutting down, not judging the cap.
func DecideSlots(c SlotsCheck, prev string) SlotsDecision {
	if c.Stopped {
		return SlotsDecision{Hold: true, Cond: prev, Reason: "not launched: the drain is stopping"}
	}
	reason := c.Reason
	if reason == "" {
		reason = "no reason given"
	}
	call := fmt.Sprintf("claude-slots check %d", max(c.Need, 1))
	var d SlotsDecision
	switch {
	case c.TimedOut:
		d.Cond = "timeout"
		d.Event = Event{Kind: EventError, Error: call + " did not finish in time; launching without the session cap"}
	case c.Err != "":
		d.Cond = "run failed"
		d.Event = Event{Kind: EventError, Error: call + " could not run: " + c.Err + "; launching without the session cap"}
	case c.Exit == 0:
		return SlotsDecision{}
	case c.Exit == 1:
		d.Hold, d.Cond = true, "held"
		d.Reason = "waiting for a claude session slot: " + reason
		d.Event = Event{Kind: EventSlotsHeld, State: string(worker.QueueQueued), Error: call + " exited 1: " + reason}
	default:
		d.Cond = fmt.Sprintf("exit %d", c.Exit)
		d.Event = Event{Kind: EventError, Error: fmt.Sprintf("%s exited %d, expected 0 or 1: %s; launching without the session cap", call, c.Exit, reason)}
	}
	d.Emit = d.Cond != prev
	return d
}

// Unclaim is the change a held row takes: back to queued with no attempt
// counted and no launch id, naming why in last_error.
func Unclaim(q worker.QueueRow, why string) Change {
	c := change(q).to(worker.QueueQueued, why)
	c.ClearLaunch = true
	return c
}
