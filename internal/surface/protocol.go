package surface

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// The bootstrap wire protocol.
//
// Two processes, one socket, three frames, then done. The outer service is the
// only invocation *server*; the trampoline that a terminal manager started is
// the client. The direction matters and is not negotiable: the outer never
// accepts an invocation supplied by the child, because the child is the process
// a hostile manager could have started.
//
//	trampoline -> service : hello       (protocol version + rendezvous nonce)
//	service    -> trampoline: invocation (harness path, argv, env, cwd)
//	trampoline -> service : exec_started | exec_failed
//
// Everything is length-delimited and hard-capped. The framing is deliberately
// boring — a four-byte big-endian length, then that many bytes of JSON — because
// the interesting failure here is not an exotic encoding, it is a peer that
// claims a frame is 4 GiB and waits for us to allocate it.
//
// Frames carry no secrets in the clear except the invocation itself, which is
// the entire point of the socket: it is what must not travel through argv a
// terminal manager can see.

const (
	// ProtocolVersion is the only wire version this build speaks.
	//
	// The outer process and the trampoline are the same binary in the intended
	// flow, so a mismatch means either a hand-built invocation or an upgrade
	// that replaced the binary between a manager being asked to type the
	// command and it running. Both refuse rather than negotiate: there is no
	// downgrade path, because a downgrade is indistinguishable from an attack.
	ProtocolVersion = 1

	// maxFrameBytes caps a single frame. The invocation is the largest thing
	// that crosses, and 1 MiB is far above any real argv-plus-environment while
	// staying small enough that a peer cannot make us allocate meaningfully.
	maxFrameBytes = 1 << 20

	// maxArgs and maxEnv bound the invocation's collections independently of
	// the byte cap. A frame can be under 1 MiB and still carry a hundred
	// thousand empty strings, which is a different denial than a large frame.
	maxArgs = 4096
	maxEnv  = 4096

	// maxEntryBytes bounds a single argument or environment entry.
	maxEntryBytes = 128 << 10
)

var (
	// ErrProtocol reports a peer that did not speak this protocol. Its message
	// is category-only: the frames carry a nonce and an invocation, and an
	// error quoting what it rejected would print them.
	ErrProtocol = errors.New("surface: bootstrap protocol violation")

	// ErrProtocolVersion reports a peer speaking a different wire version.
	ErrProtocolVersion = errors.New("surface: unsupported bootstrap protocol version")

	// ErrFrameTooLarge reports a frame over the cap. It is distinct from
	// ErrProtocol because it is the one violation that is plausibly innocent —
	// an invocation with a genuinely enormous environment — and an operator
	// deserves to be told which.
	ErrFrameTooLarge = errors.New("surface: bootstrap frame exceeds its limit")
)

// frameKind is the closed set of messages. It is a string on the wire because
// the frames are JSON and a mistyped integer is harder to read in a capture
// than a mistyped word, but it is parsed against an exact set either way.
type frameKind string

const (
	kindHello       frameKind = "hello"
	kindInvocation  frameKind = "invocation"
	kindExecStarted frameKind = "exec_started"
	kindExecFailed  frameKind = "exec_failed"
)

func (k frameKind) valid() bool {
	switch k {
	case kindHello, kindInvocation, kindExecStarted, kindExecFailed:
		return true
	default:
		return false
	}
}

// helloFrame is the trampoline's opening message.
//
// The nonce travels here rather than being derived from anything, because the
// trampoline's only proof that it is the process forgectl asked a manager to
// start is that it knows a value forgectl generated. That proof is weak on
// purpose — see the threat model — and the peer-credential check is what
// carries same-user exclusion.
type helloFrame struct {
	Kind    frameKind `json:"kind"`
	Version int       `json:"version"`
	Nonce   string    `json:"nonce"`
}

// invocationFrame is what the socket exists to carry: the resolved harness
// invocation, delivered only after the peer is authenticated.
type invocationFrame struct {
	Kind    frameKind `json:"kind"`
	Version int       `json:"version"`
	Path    string    `json:"path"`
	Args    []string  `json:"args"`
	Env     []string  `json:"env"`
	CWD     string    `json:"cwd"`
}

// resultFrame is the trampoline's answer. Started is the commit signal and is
// sent only after the harness child has crossed the fork/exec boundary;
// Reason is a bounded category on failure and empty on success.
type resultFrame struct {
	Kind    frameKind `json:"kind"`
	Version int       `json:"version"`
	Reason  string    `json:"reason,omitempty"`
}

// writeFrame encodes one frame with its length prefix.
func writeFrame(w io.Writer, v any) error {
	payload, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("%w: encode frame: %w", ErrProtocol, err)
	}
	if len(payload) > maxFrameBytes {
		return fmt.Errorf("%w: %d bytes, limit %d", ErrFrameTooLarge, len(payload), maxFrameBytes)
	}

	var header [4]byte
	// The cap check above bounds len(payload) at 1 MiB, so the conversion
	// cannot overflow — and it is above rather than merged into this line
	// precisely so the bound is a refusal with a message rather than a silent
	// truncation into the length prefix.
	//nolint:gosec // G115: bounded by the maxFrameBytes check above
	binary.BigEndian.PutUint32(header[:], uint32(len(payload)))
	if _, err := w.Write(header[:]); err != nil {
		return fmt.Errorf("%w: write frame length: %w", ErrProtocol, err)
	}
	if _, err := w.Write(payload); err != nil {
		return fmt.Errorf("%w: write frame: %w", ErrProtocol, err)
	}
	return nil
}

// readFrame reads one length-delimited frame.
//
// The length is checked *before* the buffer is allocated. That ordering is the
// whole reason this is not a json.Decoder on the raw connection: a decoder
// streaming an unbounded document will happily consume gigabytes before
// anything notices, and the peer here is a process a hostile manager started.
func readFrame(r io.Reader) ([]byte, error) {
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return nil, fmt.Errorf("%w: read frame length: %w", ErrProtocol, err)
	}
	size := binary.BigEndian.Uint32(header[:])
	if size == 0 {
		return nil, fmt.Errorf("%w: empty frame", ErrProtocol)
	}
	if size > maxFrameBytes {
		return nil, fmt.Errorf("%w: %d bytes, limit %d", ErrFrameTooLarge, size, maxFrameBytes)
	}

	payload := make([]byte, size)
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, fmt.Errorf("%w: read frame: %w", ErrProtocol, err)
	}
	return payload, nil
}

// decodeFrame parses a payload into v, refusing unknown fields and trailing
// content.
//
// Unknown fields fail rather than being ignored, for the same reason the
// reference decoder refuses them: a field this build does not recognise may be
// one a newer build uses to mean something, and silently dropping it is how a
// message gets acted on with half its meaning missing.
func decodeFrame(payload []byte, v any) error {
	dec := json.NewDecoder(newByteReader(payload))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("%w: decode frame: %w", ErrProtocol, err)
	}
	if dec.More() {
		return fmt.Errorf("%w: trailing content after a frame", ErrProtocol)
	}
	return nil
}

// validate checks a hello frame's shape. The nonce's *value* is checked
// elsewhere, in constant time; this only establishes that there is one of the
// right shape to compare.
func (f helloFrame) validate() error {
	if f.Kind != kindHello {
		return fmt.Errorf("%w: expected a hello frame", ErrProtocol)
	}
	if f.Version != ProtocolVersion {
		return fmt.Errorf("%w: peer speaks version %d, this build speaks %d",
			ErrProtocolVersion, f.Version, ProtocolVersion)
	}
	if len(f.Nonce) != nonceHexLen || !lowerHexString(f.Nonce) {
		return fmt.Errorf("%w: malformed nonce", ErrProtocol)
	}
	return nil
}

// validate checks an invocation frame before anything is executed from it.
//
// The trampoline runs this on a frame the *outer* process sent, which sounds
// like checking our own work — and is, deliberately. The trampoline cannot tell
// a genuine outer process from something else that reached the socket first,
// so it validates what it was handed rather than trusting the sender it
// authenticated.
func (f invocationFrame) validate() error {
	if f.Kind != kindInvocation {
		return fmt.Errorf("%w: expected an invocation frame", ErrProtocol)
	}
	if f.Version != ProtocolVersion {
		return fmt.Errorf("%w: peer speaks version %d, this build speaks %d",
			ErrProtocolVersion, f.Version, ProtocolVersion)
	}
	if f.Path == "" || !isAbsPath(f.Path) {
		return fmt.Errorf("%w: harness path must be absolute", ErrProtocol)
	}
	if f.CWD == "" || !isAbsPath(f.CWD) {
		return fmt.Errorf("%w: working directory must be absolute", ErrProtocol)
	}
	if len(f.Args) > maxArgs {
		return fmt.Errorf("%w: %d arguments, limit %d", ErrProtocol, len(f.Args), maxArgs)
	}
	if len(f.Env) > maxEnv {
		return fmt.Errorf("%w: %d environment entries, limit %d", ErrProtocol, len(f.Env), maxEnv)
	}
	for i, arg := range f.Args {
		if len(arg) > maxEntryBytes {
			return fmt.Errorf("%w: argument %d exceeds %d bytes", ErrProtocol, i, maxEntryBytes)
		}
	}
	for i, entry := range f.Env {
		if len(entry) > maxEntryBytes {
			return fmt.Errorf("%w: environment entry %d exceeds %d bytes", ErrProtocol, i, maxEntryBytes)
		}
		// An entry with no "=" is not an environment variable, and passing it
		// to exec would produce a child whose environment differs from what
		// the caller believes it built.
		if !hasEqualsSign(entry) {
			return fmt.Errorf("%w: environment entry %d has no '='", ErrProtocol, i)
		}
	}
	return nil
}

// validate checks a result frame.
func (f resultFrame) validate() error {
	if f.Kind != kindExecStarted && f.Kind != kindExecFailed {
		return fmt.Errorf("%w: expected a result frame", ErrProtocol)
	}
	if f.Version != ProtocolVersion {
		return fmt.Errorf("%w: peer speaks version %d, this build speaks %d",
			ErrProtocolVersion, f.Version, ProtocolVersion)
	}
	if f.Kind == kindExecStarted && f.Reason != "" {
		return fmt.Errorf("%w: a started result carries a failure reason", ErrProtocol)
	}
	if f.Kind == kindExecFailed && f.Reason == "" {
		return fmt.Errorf("%w: a failed result carries no reason", ErrProtocol)
	}
	if len(f.Reason) > maxReasonBytes {
		return fmt.Errorf("%w: reason exceeds %d bytes", ErrProtocol, maxReasonBytes)
	}
	return nil
}

// maxReasonBytes bounds the trampoline's failure category. It is small because
// the reason is a category, not a message: the trampoline has the harness's
// real error and deliberately does not forward it, since that error can quote
// the path, the argv, and the environment.
const maxReasonBytes = 128

// nonceHexLen is the encoded length of a 256-bit rendezvous nonce.
const nonceHexLen = 64
