package tmux

import (
	"context"
	"errors"
)

// AttachOrSwitch is the single path every session/window jump goes through.
//
// Inside tmux we switch the current client (attaching would nest tmux in
// tmux). Outside, we attach — which hands the controlling tty to tmux, so it
// must go through the interactive Runner path. This inside/outside split is
// the bit the old bash `s` script got subtly wrong.
func (c *Client) AttachOrSwitch(ctx context.Context, target string) error {
	if c.InsideTmux() {
		_, err := c.run.Run(ctx, c.tmuxBin, "switch-client", "-t", target)
		return err
	}
	return c.run.RunInteractive(ctx, c.tmuxBin, "attach-session", "-t", target)
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

// mostRecentSession returns the session with the greatest session_last_attached
// timestamp (empty string if no server / no sessions).
func (c *Client) mostRecentSession(ctx context.Context) (string, error) {
	const format = "#{session_last_attached}" + fieldSep + "#{session_name}"
	out, err := c.run.Run(ctx, c.tmuxBin, "list-sessions", "-F", format)
	if err != nil {
		if isNoServer(err) {
			return "", nil
		}
		return "", err
	}
	best, bestTS := "", -1
	for _, line := range splitLines(out) {
		f := splitFields(line)
		if len(f) < 2 {
			continue
		}
		if ts := atoi(f[0]); ts > bestTS {
			bestTS, best = ts, f[1]
		}
	}
	return best, nil
}
