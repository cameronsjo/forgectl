package tmux

import (
	"context"
	"errors"
	"fmt"
)

// sesh owns smart session naming (sessions + zoxide dirs + configured paths).
// forgectl delegates create/connect to it rather than reimplementing the
// naming logic — sesh stays the source of truth.

// ErrSeshUnavailableWhenPinned reports a sesh delegation attempted by a
// socket-pinned client.
//
// sesh is a separate binary with its own tmux invocations and no `-S` for
// forgectl to thread through, so it acts on the ENVIRONMENTAL server no matter
// which socket this client is pinned to. Letting it run would silently break
// the pin's whole guarantee: the operator asks for an action on the pinned
// server and gets one on the default one — creating or attaching a session that
// no identity from this client can ever revalidate. Refusing is the only honest
// outcome, and it is a hard error rather than a fallback because a fallback here
// IS the wrong-server bug.
var ErrSeshUnavailableWhenPinned = errors.New("sesh cannot be used by a socket-pinned tmux client")

// refuseSeshWhenPinned is the shared guard. It runs before checkSeshAvailable
// so the refusal does not depend on whether sesh happens to be installed —
// otherwise the same pinned client would report two different errors on two
// machines for one unconditional refusal.
func (c *Client) refuseSeshWhenPinned() error {
	if c.socket == "" {
		return nil
	}
	return fmt.Errorf("%w (socket %s)", ErrSeshUnavailableWhenPinned, c.socket)
}

// SeshList returns the candidate names sesh offers, one per line. The TUI
// populates its picker from this; Pick connects to a chosen name.
func (c *Client) SeshList(ctx context.Context) ([]string, error) {
	if err := c.refuseSeshWhenPinned(); err != nil {
		return nil, err
	}
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
//
// The `--` terminates sesh's flags. name is not forgectl-controlled: any
// same-uid process can create a tmux session called `--help` or `-t`, SeshList
// surfaces it, and the TUI picker hands it straight back here — so without the
// terminator it reaches sesh's own flag parser as argv[2].
//
// Note this is the one session path forgectl does NOT match exactly: past this
// point resolution is sesh's smart naming, not forgectl's string equality.
func (c *Client) Pick(ctx context.Context, name string) error {
	if err := c.refuseSeshWhenPinned(); err != nil {
		return err
	}
	if err := c.checkSeshAvailable(); err != nil {
		return err
	}
	return c.run.RunInteractive(ctx, c.seshBin, "connect", "--", name)
}
