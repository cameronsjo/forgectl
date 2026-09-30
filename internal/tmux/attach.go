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
	// Ahead of revalidation, not behind it. A pinned client can NEVER attach, so
	// a stale identity must not turn that permanent answer into a transient-
	// looking ErrObjectGone — a caller distinguishing "route this elsewhere"
	// from "retry" would read the wrong one and retry forever.
	if err := c.refuseAttachWhenPinned(); err != nil {
		return err
	}
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
//
// The select-window runs inside generationGuarded (forgectl#785), so a server
// replaced after the revalidation does not select its own @N.
func (c *Client) AttachWindow(ctx context.Context, want WindowIdentity) error {
	// Up front, for AttachSession's reason — and here it also spares a
	// select-window against a session the client could never then attach to.
	if err := c.refuseAttachWhenPinned(); err != nil {
		return err
	}
	current, err := c.RevalidateWindow(ctx, want)
	if err != nil {
		return fmt.Errorf("attach window %q: %w", want.Name, err)
	}
	if err := ValidateWindowID(current.ID); err != nil {
		return fmt.Errorf("select window %q: %w", want.Name, err)
	}
	if err := c.runGuarded(ctx, "select window "+current.ID, current.Generation, current.ID,
		"select-window -t "+current.ID); err != nil {
		return err
	}
	return c.attachOrSwitch(ctx, current.SessionID, current.Name)
}

// SelectWindow makes a window current within its own session without attaching
// or switching clients — the "I am already looking at this session, just change
// the view" path.
//
// The select runs inside generationGuarded on the captured Run path, exactly
// as AttachWindow's does (forgectl#805): a server replaced after the
// revalidation answers with the mismatch marker instead of selecting its own
// @N, and that answer can only be read back from captured output. tmux
// redraws any attached client itself, so nothing here needs the tty.
func (c *Client) SelectWindow(ctx context.Context, want WindowIdentity) error {
	current, err := c.RevalidateWindow(ctx, want)
	if err != nil {
		return fmt.Errorf("select window %q: %w", want.Name, err)
	}
	if err := ValidateWindowID(current.ID); err != nil {
		return fmt.Errorf("select window %q: %w", want.Name, err)
	}
	return c.runGuarded(ctx, "select window "+current.ID, current.Generation, current.ID,
		"select-window -t "+current.ID)
}

// attachOrSwitch is the single inside/outside branch, taking an already
// revalidated native session id.
func (c *Client) attachOrSwitch(ctx context.Context, sessionID, label string) error {
	if err := c.refuseAttachWhenPinned(); err != nil {
		return err
	}
	inside := c.InsideTmux()
	slog.Debug("Preparing to attach.", "session_id", sessionID, "name", label, "inside_tmux", inside)
	var err error
	if inside {
		_, err = c.run.Run(ctx, c.tmuxBin, c.tmuxArgs("switch-client", "-t", sessionID)...)
	} else {
		err = c.run.RunInteractive(ctx, c.tmuxBin, c.tmuxArgs("attach-session", "-t", sessionID)...)
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
	// Checked up front rather than left to attachOrSwitch: both outcomes end in
	// an attach a pinned client refuses, so the listing below would be work
	// done only to be discarded.
	if err := c.refuseAttachWhenPinned(); err != nil {
		return err
	}
	if c.InsideTmux() {
		_, err := c.run.Run(ctx, c.tmuxBin, c.tmuxArgs("switch-client", "-l")...)
		return err
	}
	identity, unreadable, err := c.mostRecentSession(ctx)
	if err != nil {
		return err
	}
	if unreadable > 0 {
		// Not a refusal: the readable rows still name a real session, and the
		// operator asked to go somewhere. But the most recent one may be among
		// the rows that could not be read, so say so.
		slog.Warn("The last session may not be the most recent one.",
			"reason", UnreadableRows{Sessions: unreadable}.Note())
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
// session_last_attached timestamp (a zero identity if no server / no sessions),
// and how many rows it could not read (readableRows).
//
// The field check is EXACT for the same reason parseSessions' is: a session
// name may carry FieldSep, and under a `len(f) < N` check a name of
// `real<sep>decoy` shifted every later field one right — yielding a truncated
// attach target, from the one function that hands its result straight to
// attach.
func (c *Client) mostRecentSession(ctx context.Context) (SessionIdentity, int, error) {
	args := c.tmuxArgs("list-sessions", "-F", lastAttachedFormat)
	out, err := c.run.Run(ctx, c.tmuxBin, args...)
	if err != nil {
		if c.absentServer(ctx, args, err) {
			return SessionIdentity{}, 0, nil
		}
		// An exited server's leftover socket also means no session to jump
		// to (forgectl#786). The zero identity is only ever reported, never
		// acted on.
		stateErr := c.serverStateError(ctx, args, err)
		if errors.Is(stateErr, ErrServerExited) {
			return SessionIdentity{}, 0, nil
		}
		return SessionIdentity{}, 0, stateErr
	}
	lines := splitLines(out)
	// The counting parser every listing shares (forgectl#815): the rows it
	// drops are counted, so LastSession can say the session it picked may not
	// be the most recent one.
	rows, unreadable := readableRows(lines, lastAttachedFieldCount, func(f []string) bool {
		return ValidateSessionID(f[3]) == nil
	})
	selector := c.currentSelector()
	// -1 (not 0) so a session that has never been attached (last_attached=0)
	// still beats the sentinel and gets picked when it's the only candidate.
	best, bestTS := SessionIdentity{}, -1
	for _, f := range rows {
		if ts := atoi(f[0]); ts > bestTS {
			bestTS = ts
			best = SessionIdentity{
				Generation: ServerGeneration{Selector: selector, PID: f[1], StartTime: f[2]},
				ID:         f[3],
				Name:       f[4],
			}
		}
	}
	// Non-empty output that yielded no parsed row goes through parsedRows: if
	// no line proves the separator survived, that is the locale error, refused
	// rather than reported as "no session to attach to", which reads as an
	// empty server. If a line does prove it, every session is unreadable
	// (forgectl#826), and the empty result carries the unreadable count.
	if _, err := parsedRows(rows, lines, "list-sessions", lastAttachedFieldCount); err != nil {
		return SessionIdentity{}, 0, err
	}
	return best, unreadable, nil
}
