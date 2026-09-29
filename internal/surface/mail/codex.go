package mail

import (
	"context"
	"fmt"
	"regexp"
)

// Runner runs a command and returns its output. It matches the shape of
// forgectl's internal/exec runner; T4 adds the one-line shim if it differs.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

// CodexAdapter delivers with `codex queue`, which reaches a session on the
// local app-server daemon: an idle session starts a turn, a busy one takes the
// message at its next safe point. Every priority queues as a follow-up; v1
// does no mid-turn steering into Codex (turn/start is start-or-steer and not
// atomic).
type CodexAdapter struct {
	Runner Runner
	// Bin is the codex executable. Empty means "codex" on PATH.
	Bin string
}

var threadPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$`)

// Deliver queues text on the worker's thread. The thread id is learned from
// the worker's first turn-complete notify, so a message sent before that stays
// queued rather than failing.
func (a CodexAdapter) Deliver(ctx context.Context, w Worker, m Message, text string) (string, error) {
	if w.ThreadID == "" {
		return "", NotReady("codex thread for %s is not known yet; forgectl records it from the first turn-complete notify", w.Name)
	}
	if !threadPattern.MatchString(w.ThreadID) {
		return "", fmt.Errorf("codex thread id %s has unexpected characters", quoteTrunc(w.ThreadID))
	}
	if a.Runner == nil {
		return "", fmt.Errorf("codex adapter has no runner")
	}
	bin := a.Bin
	if bin == "" {
		bin = "codex"
	}
	// --flag=value keeps a value that starts with a dash from reading as a flag.
	out, err := a.Runner.Run(ctx, bin, "queue", "--thread="+w.ThreadID, "--message="+text)
	if err != nil {
		return "", NotReady("codex queue: %v: %s", err, oneLine(out, 200))
	}
	return "queued on codex thread " + w.ThreadID, nil
}

// State is the last state a notify event recorded; Codex has no registry
// forgectl reads in v1.
func (a CodexAdapter) State(ctx context.Context, w Worker) (WorkerState, error) {
	if w.State == "" {
		return StateUnknown, nil
	}
	return w.State, nil
}
