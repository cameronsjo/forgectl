package tmux

import "context"

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
