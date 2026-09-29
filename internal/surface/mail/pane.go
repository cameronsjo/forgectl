package mail

import (
	"context"
	"errors"
)

// PaneDriver is what the pane fallback needs from the herdr worker verbs
// (#536): the readiness check `surface ready` runs, and a paste-and-submit.
type PaneDriver interface {
	Ready(ctx context.Context, w Worker) (bool, error)
	Submit(ctx context.Context, w Worker, text string) error
}

// PaneAdapter delivers to a harness with no way in but its terminal. It only
// pastes when the harness is at its input prompt, so it never types into a
// running turn; until then the message stays queued.
type PaneAdapter struct {
	Driver PaneDriver
}

// Deliver pastes text once the pane is ready.
func (a PaneAdapter) Deliver(ctx context.Context, w Worker, m Message, text string) (string, error) {
	if a.Driver == nil {
		return "", errors.New("no pane driver configured")
	}
	ready, err := a.Driver.Ready(ctx, w)
	if err != nil {
		return "", NotReady("pane readiness: %v", err)
	}
	if !ready {
		return "", NotReady("pane is not at its input prompt")
	}
	if err := a.Driver.Submit(ctx, w, text); err != nil {
		return "", NotReady("pane submit: %v", err)
	}
	return "pasted into pane " + w.PaneID, nil
}

// State is the pane's readiness: ready means idle.
func (a PaneAdapter) State(ctx context.Context, w Worker) (WorkerState, error) {
	if a.Driver == nil {
		return StateUnknown, nil
	}
	ready, err := a.Driver.Ready(ctx, w)
	if err != nil {
		return StateUnknown, err
	}
	if ready {
		return StateIdle, nil
	}
	return StateBusy, nil
}
