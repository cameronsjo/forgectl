package tmux

import (
	"context"
	"errors"
	"fmt"

	"github.com/cameronsjo/forgectl/internal/termsafe"
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

// ErrAttachUnavailableWhenPinned reports a client attach or switch attempted by
// a socket-pinned client.
//
// A pin exists so forgectl can drive a PRIVATE tmux server that some terminal
// manager displays; forgectl itself is never the client of that server. Both
// branches of attachOrSwitch are wrong under a pin, for opposite reasons:
//
//   - switch-client targets the operator's currently attached client, which is
//     connected to a DIFFERENT server, so it is meaningless here. InsideTmux
//     already reports false under a pin, so this branch is unreachable.
//   - attach-session would seize the invoking terminal for the private server —
//     and tmux refuses it outright when $TMUX is set ("sessions should be
//     nested with care"), which under a pin is exactly the case that reaches it.
//
// So the reachable branch is broken precisely when a pin is in play, and the
// unreachable one would be wrong if it ran. Refusing is the honest outcome.
var ErrAttachUnavailableWhenPinned = errors.New("a socket-pinned tmux client cannot attach or switch clients")

// refuseWhenPinned is the one place a capability the pin cannot reach is
// refused. Both callers get the same shape and, more importantly, the same
// quoting decision: written as two sibling functions, one quoted the socket and
// one did not, and nothing said which was the rule. The path is safe today only
// because NewPinned screens control runes — quoting here is what keeps that
// true if a Client's socket ever arrives by another route.
func (c *Client) refuseWhenPinned(sentinel error) error {
	if c.socket == "" {
		return nil
	}
	return fmt.Errorf("%w (socket %s)", sentinel, termsafe.QuotePath(c.socket))
}

// refuseAttachWhenPinned guards the attach/switch capability. AttachSession,
// AttachWindow, and LastSession each call it up front — before revalidating —
// so a caller asking "can this client ever attach?" gets that answer rather
// than a transient-looking ErrObjectGone from a stale identity. attachOrSwitch
// keeps the check too, as the seam backstop for any future entry point.
func (c *Client) refuseAttachWhenPinned() error {
	return c.refuseWhenPinned(ErrAttachUnavailableWhenPinned)
}

// refuseSeshWhenPinned guards the sesh delegation. It runs before
// checkSeshAvailable so the refusal does not depend on whether sesh happens to
// be installed — otherwise the same pinned client would report two different
// errors on two machines for one unconditional refusal.
func (c *Client) refuseSeshWhenPinned() error {
	return c.refuseWhenPinned(ErrSeshUnavailableWhenPinned)
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
