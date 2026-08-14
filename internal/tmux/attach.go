package tmux

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
)

// AttachSession attaches (or, inside tmux, switches) to the session the
// identity names, revalidating it immediately first.
//
// Inside tmux we switch the current client; attaching would nest tmux in tmux.
// Outside, we attach — which hands the controlling tty to tmux, so it must go
// through the interactive Runner path. That inside/outside split is the bit the
// old bash `s` script got subtly wrong.
//
// The `-t` operand is the native session id, never the name: a name goes
// through tmux's target grammar, where a missing session falls through to a
// prefix sibling (forgectl#237).
func (c *Client) AttachSession(ctx context.Context, want SessionIdentity) error {
	current, err := c.RevalidateSession(ctx, want)
	if err != nil {
		return fmt.Errorf("attach session %q: %w", want.Name, err)
	}
	return c.attachOrSwitch(ctx, current.ID, current.Name)
}

// AttachWindow brings the given window to the foreground: it selects the window
// within its session, then attaches (or switches) to that session.
//
// Both steps take native ids, and the order matters. select-window works
// against a detached session perfectly well, so doing it first means the client
// arrives already looking at the right window rather than flashing whatever was
// current.
func (c *Client) AttachWindow(ctx context.Context, want WindowIdentity) error {
	current, err := c.RevalidateWindow(ctx, want)
	if err != nil {
		return fmt.Errorf("attach window %q: %w", want.Name, err)
	}
	if _, err := c.run.Run(ctx, c.tmuxBin, "select-window", "-t", current.ID); err != nil {
		return fmt.Errorf("select window %s: %w", current.ID, err)
	}
	return c.attachOrSwitch(ctx, current.SessionID, current.Name)
}

// SelectWindow makes a window current within its own session without attaching
// or switching clients — the "I am already looking at this session, just change
// the view" path. It goes through the interactive runner because tmux redraws
// the attached client as a side effect.
func (c *Client) SelectWindow(ctx context.Context, want WindowIdentity) error {
	current, err := c.RevalidateWindow(ctx, want)
	if err != nil {
		return fmt.Errorf("select window %q: %w", want.Name, err)
	}
	return c.run.RunInteractive(ctx, c.tmuxBin, "select-window", "-t", current.ID)
}

// attachOrSwitch is the single inside/outside branch, taking an already
// revalidated native session id.
func (c *Client) attachOrSwitch(ctx context.Context, sessionID, label string) error {
	inside := c.InsideTmux()
	slog.Debug("Preparing to attach.", "session_id", sessionID, "name", label, "inside_tmux", inside)
	var err error
	if inside {
		_, err = c.run.Run(ctx, c.tmuxBin, "switch-client", "-t", sessionID)
	} else {
		err = c.run.RunInteractive(ctx, c.tmuxBin, "attach-session", "-t", sessionID)
	}
	if err != nil {
		slog.Error("Failed to attach.", "session_id", sessionID, "name", label, "error", err)
		return err
	}
	slog.Debug("Successfully attached.", "session_id", sessionID, "name", label)
	return nil
}

// LastSession jumps to the last-used session. Inside tmux, tmux already tracks
// this — switch-client -l. Outside, there's no "last" client state, so we
// resolve the most-recently-attached session ourselves and attach to it by id.
func (c *Client) LastSession(ctx context.Context) error {
	if c.InsideTmux() {
		_, err := c.run.Run(ctx, c.tmuxBin, "switch-client", "-l")
		return err
	}
	identity, err := c.mostRecentSession(ctx)
	if err != nil {
		return err
	}
	if identity.ID == "" {
		return errors.New("no session to attach to")
	}
	return c.AttachSession(ctx, identity)
}

// lastAttachedFormat carries the sort key plus a full identity, so the winner is
// attached by native id rather than by the name it happened to have when the
// list was taken.
const lastAttachedFormat = "#{session_last_attached}" + FieldSep +
	"#{pid}" + FieldSep +
	"#{start_time}" + FieldSep +
	"#{session_id}" + FieldSep +
	"#{session_name}"

// lastAttachedFieldCount is how many fields lastAttachedFormat emits.
const lastAttachedFieldCount = 5

// mostRecentSession returns the identity of the session with the greatest
// session_last_attached timestamp (a zero identity if no server / no sessions).
//
// The field check is EXACT for the same reason parseSessions' is: a session
// name may carry FieldSep, and under a `len(f) < N` check a name of
// `real<sep>decoy` shifted every later field one right — yielding a truncated
// attach target, from the one function that hands its result straight to
// attach.
func (c *Client) mostRecentSession(ctx context.Context) (SessionIdentity, error) {
	args := []string{"list-sessions", "-F", lastAttachedFormat}
	out, err := c.run.Run(ctx, c.tmuxBin, args...)
	if err != nil {
		if c.absentDefaultServer(ctx, args, err) {
			return SessionIdentity{}, nil
		}
		return SessionIdentity{}, c.serverStateError(ctx, args, err)
	}
	lines := splitLines(out)
	// parsed holds the rows that split cleanly. Only its emptiness is read
	// below; the winner is tracked separately because it is the best row, not
	// the last one.
	parsed := make([]struct{}, 0, len(lines))
	selector := c.currentSelector()
	// -1 (not 0) so a session that has never been attached (last_attached=0)
	// still beats the sentinel and gets picked when it's the only candidate.
	best, bestTS := SessionIdentity{}, -1
	for _, line := range lines {
		f := splitFields(line)
		if len(f) != lastAttachedFieldCount {
			continue
		}
		if err := ValidateSessionID(f[3]); err != nil {
			continue
		}
		parsed = append(parsed, struct{}{})
		if ts := atoi(f[0]); ts > bestTS {
			bestTS = ts
			best = SessionIdentity{
				Generation: ServerGeneration{Selector: selector, PID: f[1], StartTime: f[2]},
				ID:         f[3],
				Name:       f[4],
			}
		}
	}
	// Non-empty output that yielded no parsed row at all means the separator did
	// not survive — refuse rather than report "no session to attach to", which
	// reads as an empty server.
	if _, err := parsedRows(parsed, lines, "list-sessions", lastAttachedFieldCount); err != nil {
		return SessionIdentity{}, err
	}
	return best, nil
}
