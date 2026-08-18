package surface

import (
	"errors"
	"fmt"
	"net"
	"slices"

	"github.com/cameronsjo/forgectl/internal/launch"
)

// The two ends of the bootstrap exchange.
//
// The frames themselves stay unexported: they are the wire format, and a second
// package able to construct one is a second definition of the protocol. What
// crosses the package boundary is the *conversation* — Accept and Serve for the
// outer service, Dial and its result frames for the trampoline — because the
// trampoline does not live here. It is the hidden `surface _exec` entry in the
// CLI package, deliberately kept out of this package so that starting and
// reaping a process does not become the job of the package that owns policy and
// privacy.
//
// Each type owns one connection and one deadline, and the deadline is set at
// construction rather than by the caller, so a conversation that hangs is not
// reachable through this API.

// ErrNonceMismatch reports a peer that did not present the expected nonce.
//
// It does not distinguish a malformed nonce from a wrong one, and the two paths
// are deliberately the same cost: a caller that could tell them apart could use
// this package as an oracle for guessing the nonce one property at a time.
var ErrNonceMismatch = errors.New("surface: bootstrap peer presented the wrong rendezvous nonce")

// Invocation is what the trampoline received and is about to execute.
//
// It is a surface-owned type rather than launch.Invocation because the
// trampoline needs strictly less: which binary, which argv, which environment,
// which directory. The harness name and the binary's provenance are the outer
// process's business — they inform policy decisions that were already made
// before anything reached this socket, and sending them would put facts on the
// wire that the receiving end has no use for.
type Invocation struct {
	Path string
	Args []string
	Env  []string
	CWD  string
}

// invocationFrameFrom maps an outbound invocation onto the wire.
//
// It exists so the mapping has one home. Written at the call site, a field
// added to launch.Invocation later is silently dropped — a child running with
// the wrong environment and nothing to indicate why. The companion test
// reflects over launch.Invocation and fails when a field is neither carried
// here nor named as intentionally dropped.
func invocationFrameFrom(inv launch.Invocation) invocationFrame {
	return invocationFrame{
		Kind:    kindInvocation,
		Version: ProtocolVersion,
		Path:    inv.Binary.Path,
		Args:    slices.Clone(inv.Args),
		Env:     slices.Clone(inv.Env),
		CWD:     inv.CWD,
	}
}

// Handoff is the service's end of one bootstrap conversation.
type Handoff struct {
	ex *exchange
}

// Accept authenticates a freshly accepted connection.
//
// Three checks, in this order and for this reason: the peer credential first,
// because it is the only one the peer cannot influence and the cheapest to
// fail; then the frame's shape; then the nonce. Nothing is sent to the peer
// until all three pass — the invocation is the thing this socket exists to
// protect, and every check before it is a precondition of sending it.
//
// It reads under the small unauthenticated budget, not the invocation's.
func Accept(conn net.Conn, expected Nonce) (*Handoff, error) {
	if err := VerifyPeer(conn); err != nil {
		return nil, err
	}
	if !expected.Valid() {
		// A service that generated no nonce must not authenticate anyone. This
		// is the same fail-open Nonce.Equal guards against, caught one layer up
		// where the mistake is likelier: a zero-value field on a struct.
		return nil, fmt.Errorf("%w: no nonce was generated for this launch", ErrNonceMismatch)
	}

	ex, err := newExchange(conn, HandshakeTimeout)
	if err != nil {
		return nil, err
	}

	payload, err := ex.read(maxHelloBytes)
	if err != nil {
		return nil, err
	}
	var hello helloFrame
	if err := decodeFrame(payload, &hello); err != nil {
		return nil, err
	}

	presented, err := ParseNonce(hello.Nonce)
	if err != nil || !expected.Equal(presented) {
		return nil, ErrNonceMismatch
	}
	return &Handoff{ex: ex}, nil
}

// Send delivers the invocation to the authenticated trampoline.
func (h *Handoff) Send(inv launch.Invocation) error {
	return h.ex.write(invocationFrameFrom(inv))
}

// AwaitStart blocks until the trampoline reports the outcome of its exec.
//
// A nil return is the commit signal, and it is the *only* commit signal: it
// means a complete, authenticated exec_started frame arrived. Receipt of the
// invocation is not commit, and neither is a connection that simply stayed
// open — both are states a trampoline that died mid-start would produce.
func (h *Handoff) AwaitStart() error {
	payload, err := h.ex.read(maxHelloBytes)
	if err != nil {
		return err
	}
	var result resultFrame
	if err := decodeFrame(payload, &result); err != nil {
		return err
	}
	if result.Kind == kindExecFailed {
		return fmt.Errorf("surface: the trampoline could not start the harness: %s", result.Reason)
	}
	return nil
}

// Close releases the conversation's connection.
func (h *Handoff) Close() error { return h.ex.conn.Close() }

// Bootstrap is the trampoline's end of one bootstrap conversation.
type Bootstrap struct {
	ex *exchange
}

// Dial presents the nonce and receives the invocation.
//
// The returned Invocation has already been validated. That validation is of a
// message the *outer* process sent, which sounds like checking our own work and
// is deliberate: the trampoline cannot tell a genuine outer process from
// something that reached the socket first, so it checks what it holds rather
// than trusting the sender it authenticated.
func Dial(conn net.Conn, n Nonce) (*Bootstrap, Invocation, error) {
	if err := VerifyPeer(conn); err != nil {
		return nil, Invocation{}, err
	}
	if !n.Valid() {
		return nil, Invocation{}, ErrInvalidNonce
	}

	ex, err := newExchange(conn, HandshakeTimeout)
	if err != nil {
		return nil, Invocation{}, err
	}

	if err := ex.write(helloFrame{
		Kind:    kindHello,
		Version: ProtocolVersion,
		Nonce:   n.String(),
	}); err != nil {
		return nil, Invocation{}, err
	}

	// The invocation is the one frame that gets the full budget, and it is the
	// only one this end reads.
	payload, err := ex.read(maxFrameBytes)
	if err != nil {
		return nil, Invocation{}, err
	}
	var frame invocationFrame
	if err := decodeFrame(payload, &frame); err != nil {
		return nil, Invocation{}, err
	}

	return &Bootstrap{ex: ex}, Invocation{
		Path: frame.Path,
		Args: slices.Clone(frame.Args),
		Env:  slices.Clone(frame.Env),
		CWD:  frame.CWD,
	}, nil
}

// Started reports that the harness child has crossed the fork/exec boundary.
//
// It must be called only after a successful Start — on Darwin and Linux that
// call returns only once the child's directory change and exec have both
// succeeded, which is what makes this an exact acknowledgement rather than an
// optimistic one.
func (b *Bootstrap) Started() error {
	return b.ex.write(resultFrame{Kind: kindExecStarted, Version: ProtocolVersion})
}

// Failed reports a bounded category. The trampoline holds the real error and
// does not send it: that error quotes the harness path and can quote the argv.
func (b *Bootstrap) Failed(reason ExecFailure) error {
	return b.ex.write(resultFrame{
		Kind:    kindExecFailed,
		Version: ProtocolVersion,
		Reason:  reason,
	})
}

// Close releases the conversation's connection.
func (b *Bootstrap) Close() error { return b.ex.conn.Close() }
