package tmux

import "context"

// sesh owns smart session naming (sessions + zoxide dirs + configured paths).
// forgectl delegates create/connect to it rather than reimplementing the
// naming logic — sesh stays the source of truth.

// SeshList returns the candidate names sesh offers, one per line. The TUI
// populates its picker from this; Pick connects to a chosen name.
func (c *Client) SeshList(ctx context.Context) ([]string, error) {
	if err := c.checkSeshAvailable(); err != nil {
		return nil, err
	}
	out, err := c.run.Run(ctx, c.seshBin, "list")
	if err != nil {
		return nil, err
	}
	return splitLines(out), nil
}

// Pick connects to (or smart-creates) the named target via sesh. sesh handles
// the inside/outside-tmux switch itself, so this is a straight interactive
// hand-off — sesh connect takes over the tty.
func (c *Client) Pick(ctx context.Context, name string) error {
	if err := c.checkSeshAvailable(); err != nil {
		return err
	}
	return c.run.RunInteractive(ctx, c.seshBin, "connect", name)
}
