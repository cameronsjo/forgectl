package tmux

import (
	"context"
	"errors"
	"log/slog"
)

// AttachOrSwitch is the single path every session/window jump goes through.
//
// Inside tmux we switch the current client (attaching would nest tmux in
// tmux). Outside, we attach — which hands the controlling tty to tmux, so it
// must go through the interactive Runner path. This inside/outside split is
// the bit the old bash `s` script got subtly wrong.
func (c *Client) AttachOrSwitch(ctx context.Context, target string) error {
	inside := c.InsideTmux()
	slog.Debug("Preparing to attach.", "target", target, "inside_tmux", inside)
	var err error
	if inside {
		_, err = c.run.Run(ctx, c.tmuxBin, "switch-client", "-t", target)
	} else {
		err = c.run.RunInteractive(ctx, c.tmuxBin, "attach-session", "-t", target)
	}
	if err != nil {
		slog.Error("Failed to attach.", "target", target, "error", err)
	} else {
		slog.Debug("Successfully attached.", "target", target)
	}
	return err
}

// LastSession jumps to the last-used session. Inside tmux, tmux already tracks
// this — switch-client -l. Outside, there's no "last" client state, so we
// resolve the most-recently-attached session ourselves and attach to it.
func (c *Client) LastSession(ctx context.Context) error {
	if c.InsideTmux() {
		_, err := c.run.Run(ctx, c.tmuxBin, "switch-client", "-l")
		return err
	}
	name, err := c.mostRecentSession(ctx)
	if err != nil {
		return err
	}
	if name == "" {
		return errors.New("no session to attach to")
	}
	return c.run.RunInteractive(ctx, c.tmuxBin, "attach-session", "-t", name)
}

// lastAttachedFieldCount is how many fields mostRecentSession's format emits.
const lastAttachedFieldCount = 2

// mostRecentSession returns the session with the greatest session_last_attached
// timestamp (empty string if no server / no sessions).
//
// The field check is EXACT for the same reason parseSessions' is: a session
// name may carry FieldSep, and under the old `len(f) < 2` check a name of
// `real<sep>decoy` yielded f[1] == "real" — a truncated attach target, and the
// one this function hands straight to `attach-session -t`.
func (c *Client) mostRecentSession(ctx context.Context) (string, error) {
	const format = "#{session_last_attached}" + FieldSep + "#{session_name}"
	args := []string{"list-sessions", "-F", format}
	out, err := c.run.Run(ctx, c.tmuxBin, args...)
	if err != nil {
		if c.absentDefaultServer(ctx, args, err) {
			return "", nil
		}
		return "", err
	}
	// -1 (not 0) so a session that has never been attached (last_attached=0)
	// still beats the sentinel and gets picked when it's the only candidate.
	lines := splitLines(out)
	candidates := make([]string, 0, len(lines))
	best, bestTS := "", -1
	for _, line := range lines {
		f := splitFields(line)
		if len(f) != lastAttachedFieldCount {
			continue
		}
		candidates = append(candidates, f[1])
		if ts := atoi(f[0]); ts > bestTS {
			bestTS, best = ts, f[1]
		}
	}
	// Non-empty output that yielded no candidate at all means the separator did
	// not survive — refuse rather than report "no session to attach to", which
	// reads as an empty server.
	if _, err := parsedRows(candidates, lines, "list-sessions", lastAttachedFieldCount); err != nil {
		return "", err
	}
	return best, nil
}
