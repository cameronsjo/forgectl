package surface

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"time"
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
// Everything is length-delimited, hard-capped, and time-bounded. The framing is
// deliberately boring — a four-byte big-endian length, then that many bytes of
// JSON — because the interesting failures here are not exotic encodings. They
// are a peer that claims a frame is 4 GiB and waits for us to allocate it, and
// a peer that claims a frame and then simply never sends it. The size caps
// answer the first; the exchange's deadline answers the second, and neither
// substitutes for the other.
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

	// maxFrameBytes caps the invocation frame — the largest thing that crosses,
	// and the only one this process writes. 1 MiB is far above any real
	// argv-plus-environment while staying small enough that a peer cannot make
	// us allocate meaningfully.
	maxFrameBytes = 1 << 20

	// maxHelloBytes caps the frames an *unauthenticated* peer sends.
	//
	// The 1 MiB budget above exists for the invocation, which travels in the
	// other direction and is written by us. Everything a peer sends before it
	// has proven anything — the hello, and the result frame after it — has a
	// known small maximum, so granting it the invocation's budget would let an
	// unauthenticated connection reserve a megabyte for nothing. A hello is a
	// kind, a version, and 64 hex characters; 4 KiB is generous.
	maxHelloBytes = 4 << 10

	// maxArgs and maxEnv bound the invocation's collections independently of
	// the byte cap, so a frame under 1 MiB cannot still carry an absurd number
	// of tiny entries.
	//
	// They are defence in depth rather than the binding constraint on the read
	// path: at these sizes the frame cap is reached first for any realistic
	// entry width, and only a frame of thousands of near-empty strings gets
	// caught here instead. What they guarantee is that the bound does not
	// depend on the frame cap staying where it is, and that a future caller
	// handing an invocationFrame straight to validate() — without a frame
	// around it — is still bounded.
	maxArgs = 4096
	maxEnv  = 4096

	// maxEntryBytes bounds a single argument or environment entry.
	maxEntryBytes = 128 << 10

	// maxDecodeErrorBytes truncates a wrapped decoder error.
	//
	// encoding/json quotes what it rejected — an unknown field name, an
	// unparseable number — and the peer chooses those bytes. A 900 KB field
	// name produces a 900 KB error string that is then logged, wrapped, and
	// carried up the stack. The first part of the message is the diagnostic
	// value; the rest is the peer choosing how much of our memory and our log
	// its refusal occupies.
	maxDecodeErrorBytes = 256

	// HandshakeTimeout bounds the *entire* three-frame exchange.
	//
	// One deadline for the whole conversation rather than one per frame: three
	// per-frame deadlines let a peer stall three times the budget by sending
	// each frame just before its own timer expires. The bound is on the
	// conversation because the conversation is what must not hang.
	//
	// It is generous relative to the work — both ends are local processes on
	// one machine exchanging a few kilobytes — because the cost of it being too
	// short is a failed launch on a loaded machine, and the cost of it being
	// too long is a bounded wait.
	HandshakeTimeout = 30 * time.Second
)

var (
	// ErrProtocol reports a peer that did not speak this protocol. Its message
	// is category-only: the frames carry a nonce and an invocation, and an
	// error quoting what it rejected would print them.
	//
	// Every protocol-level refusal below wraps this, so a consumer's general
	// predicate — errors.Is(err, ErrProtocol) — is a true superset and the
	// specific sentinels still discriminate for operator messaging. A flat set
	// of siblings would make the obvious predicate fail *open* on exactly the
	// refusals that matter most.
	ErrProtocol = errors.New("surface: bootstrap protocol violation")

	// ErrProtocolVersion reports a peer speaking a different wire version.
	ErrProtocolVersion = fmt.Errorf("%w: unsupported version", ErrProtocol)

	// ErrFrameTooLarge reports a frame over its cap. It is distinguished from a
	// plain violation because it is the one refusal that is plausibly innocent
	// — an invocation with a genuinely enormous environment — and an operator
	// deserves to be told which.
	ErrFrameTooLarge = fmt.Errorf("%w: frame exceeds its limit", ErrProtocol)

	// ErrTransport reports a connection that failed underneath the protocol —
	// a broken pipe, a reset, an expired deadline.
	//
	// It deliberately does *not* wrap ErrProtocol. A peer that violates the
	// framing rules and a socket that died are different events with different
	// responses: the first is a refusal to act on, the second may be the peer
	// having simply gone away. Folding both under one sentinel would leave a
	// caller unable to tell a misbehaving peer from a failed transport, which
	// is precisely the distinction the launch state machine turns on.
	ErrTransport = errors.New("surface: bootstrap transport failure")
)

// frameKind is the closed set of messages. It is a string on the wire because
// the frames are JSON and a mistyped integer is harder to read in a capture
// than a mistyped word, but every validator pins its exact expected kind, which
// is a stronger check than membership in the set.
type frameKind string

const (
	kindHello       frameKind = "hello"
	kindInvocation  frameKind = "invocation"
	kindExecStarted frameKind = "exec_started"
	kindExecFailed  frameKind = "exec_failed"
)

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

// ExecFailure is the closed set of reasons the trampoline could not start the
// harness.
//
// It is a closed type rather than a string because the type is the only thing
// that can enforce what the wire format promises. The trampoline holds the
// operating system's real error — which quotes the harness path, and can quote
// the argv — and the whole point of this field is that it does not forward it.
// A `string` with a length cap does not prevent that: a 128-byte cap is
// comfortably enough for `fork/exec <resolved path>: permission denied`, and
// truncating an error message is the most natural way to fill a capped string
// field. Making it an enum means the leak cannot be written by accident.
type ExecFailure string

const (
	// FailChdir reports that the child could not enter the target directory.
	FailChdir ExecFailure = "chdir"
	// FailExec reports that the harness binary could not be executed.
	FailExec ExecFailure = "exec"
	// FailInvocation reports an invocation frame the trampoline refused.
	FailInvocation ExecFailure = "invocation"
)

func (r ExecFailure) valid() bool {
	switch r {
	case FailChdir, FailExec, FailInvocation:
		return true
	default:
		return false
	}
}

// resultFrame is the trampoline's answer. Started is the commit signal and is
// sent only after the harness child has crossed the fork/exec boundary;
// Reason is a closed category on failure and empty on success.
type resultFrame struct {
	Kind    frameKind   `json:"kind"`
	Version int         `json:"version"`
	Reason  ExecFailure `json:"reason,omitempty"`
}

// exchange is one bootstrap conversation, and it exists so the deadline cannot
// be forgotten.
//
// readFrame and writeFrame take an io.Reader and an io.Writer, which makes them
// testable but leaves them with no way to bound their own blocking — a peer
// that sends a length header and then nothing blocks io.ReadFull forever, and
// no cap prevents that. Routing every real exchange through this type puts the
// deadline at construction, so the bound is a property of having a conversation
// at all rather than of the caller remembering to set one.
type exchange struct {
	conn net.Conn
}

// newExchange bounds the whole conversation on conn and returns it.
func newExchange(conn net.Conn, timeout time.Duration) (*exchange, error) {
	if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		return nil, fmt.Errorf("%w: set handshake deadline: %w", ErrTransport, err)
	}
	return &exchange{conn: conn}, nil
}

// write sends one frame. The parameter is the frame interface rather than a
// type parameter because methods cannot take one; the constraint is the same.
func (e *exchange) write(v frame) error { return writeFrame(e.conn, v) }

// read reads one frame, refusing anything over limit. The limit is per-call
// because the two directions carry different maxima: we write the invocation
// under maxFrameBytes and read a peer's frames under maxHelloBytes.
func (e *exchange) read(limit uint32) ([]byte, error) { return readFrame(e.conn, limit) }

// frame is the closed set of messages that may cross this socket.
//
// It exists to constrain writeFrame and decodeFrame, which previously took
// `any` — so writeFrame(w, "hello") compiled and put a bare JSON string on the
// wire. The constraint makes the wire format a property of the type system
// rather than of every call site being written carefully.
type frame interface{ validate() error }

// writeFrame validates, then encodes one frame with its length prefix.
//
// Validating on the way *out* is the corollary of the trampoline validating on
// the way in. That receiver-side check exists because it cannot trust the
// sender; this one exists so a sender cannot emit a message it already knows is
// invalid, which would surface at the far end as a protocol violation and be
// attributed to the peer. A bug on our side must not read as an attack from
// theirs.
func writeFrame[T frame](w io.Writer, v T) error {
	if err := v.validate(); err != nil {
		return err
	}
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
		return fmt.Errorf("%w: write frame length: %w", ErrTransport, err)
	}
	if _, err := w.Write(payload); err != nil {
		return fmt.Errorf("%w: write frame: %w", ErrTransport, err)
	}
	return nil
}

// readFrame reads one length-delimited frame, refusing anything over limit.
//
// The length is checked *before* any buffer is sized. That ordering is the
// whole reason this is not a json.Decoder on the raw connection: a decoder
// streaming an unbounded document will happily consume gigabytes before
// anything notices, and the peer here is a process a hostile manager started.
//
// The body is then copied incrementally rather than pre-allocated, so a peer
// that claims the maximum and delivers one byte costs one byte rather than the
// whole claim. Pre-sizing would make the *claim* the cost, which hands an
// unauthenticated peer a memory-reservation primitive that the cap bounds only
// per connection.
//
// It does not bound its own blocking: that is the exchange's deadline, set once
// for the conversation.
func readFrame(r io.Reader, limit uint32) ([]byte, error) {
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return nil, fmt.Errorf("%w: read frame length: %w", ErrTransport, err)
	}
	size := binary.BigEndian.Uint32(header[:])
	if size == 0 {
		return nil, fmt.Errorf("%w: empty frame", ErrProtocol)
	}
	if size > limit {
		return nil, fmt.Errorf("%w: %d bytes, limit %d", ErrFrameTooLarge, size, limit)
	}

	var buf bytes.Buffer
	if _, err := io.CopyN(&buf, r, int64(size)); err != nil {
		return nil, fmt.Errorf("%w: read frame: %w", ErrTransport, err)
	}
	return buf.Bytes(), nil
}

// decodeFrame parses a payload into v, refusing unknown fields and trailing
// content.
//
// Unknown fields fail rather than being ignored, for the same reason the
// reference decoder refuses them: a field this build does not recognise may be
// one a newer build uses to mean something, and silently dropping it is how a
// message gets acted on with half its meaning missing.
//
// Trailing content is rejected by decoding a second value and requiring io.EOF,
// not by Decoder.More. More only peeks for a closing delimiter, so it reports
// *false* — meaning "nothing follows" — when the trailing byte is `]` or `}`.
// A frame with a stray closing brace after it would have been accepted.
// It validates before returning, so a decoded frame is a checked frame. Leaving
// that to the caller means three frames and three chances to forget.
func decodeFrame[T frame](payload []byte, v T) error {
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("%w: decode frame: %s", ErrProtocol, truncate(err.Error(), maxDecodeErrorBytes))
	}
	var trailing json.RawMessage
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: trailing content after a frame", ErrProtocol)
	}
	return v.validate()
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
// a genuine outer process from something else that reached the socket first, so
// it validates what it was handed rather than trusting the sender it
// authenticated.
//
// What "absolute" buys here is bounded: it refuses a relative path, not a
// traversing one — `/a/../../bin/sh` is absolute and passes. That is deliberate
// rather than an oversight. A manager that could point the trampoline at a
// substitute socket could equally type any command it liked, so this layer is
// shape validation and defense in depth, not an authorization boundary. The
// authorization boundary is the peer check and the 0700 directory.
func (f invocationFrame) validate() error {
	if f.Kind != kindInvocation {
		return fmt.Errorf("%w: expected an invocation frame", ErrProtocol)
	}
	if f.Version != ProtocolVersion {
		return fmt.Errorf("%w: peer speaks version %d, this build speaks %d",
			ErrProtocolVersion, f.Version, ProtocolVersion)
	}
	if !isAbsPath(f.Path) {
		return fmt.Errorf("%w: harness path must be absolute", ErrProtocol)
	}
	if !isAbsPath(f.CWD) {
		return fmt.Errorf("%w: working directory must be absolute", ErrProtocol)
	}
	// NUL is rejected here rather than left to exec. The syscall refuses it too,
	// but its error quotes the offending path — and a bounded, category-only
	// refusal from this package is the whole reason the trampoline does not
	// forward the harness's own error.
	if containsNUL(f.Path) || containsNUL(f.CWD) {
		return fmt.Errorf("%w: path contains a NUL byte", ErrProtocol)
	}
	// Terminal-unsafe runes are refused in the two fields that get rendered.
	// Every other file in this package already does this — RunDir checks a
	// socket path it produces itself — and this was the one place an untrusted
	// path arrived without it. Our own errors here are category-only, so the
	// exposure is downstream: the service and launch's consumers print these
	// values, and an escape sequence in a path would be interpreted there.
	//
	// Scoped to Path and CWD deliberately. Args and Env legitimately carry tabs
	// and newlines — a prompt is an argument — so extending this to them would
	// refuse valid launches.
	if err := refuseUnsafeRunes("harness path", f.Path); err != nil {
		return err
	}
	if err := refuseUnsafeRunes("working directory", f.CWD); err != nil {
		return err
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
		if containsNUL(arg) {
			return fmt.Errorf("%w: argument %d contains a NUL byte", ErrProtocol, i)
		}
	}
	for i, entry := range f.Env {
		if len(entry) > maxEntryBytes {
			return fmt.Errorf("%w: environment entry %d exceeds %d bytes", ErrProtocol, i, maxEntryBytes)
		}
		// An entry that is not NAME=VALUE is not an environment variable, and
		// exec carries it into the child regardless — producing a child whose
		// environment differs from what the caller believes it built. A bare
		// "=" test is not enough: "=VALUE" contains one and names nothing.
		if !isEnvAssignment(entry) {
			return fmt.Errorf("%w: environment entry %d is not a NAME=VALUE assignment", ErrProtocol, i)
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
	if f.Kind == kindExecFailed && !f.Reason.valid() {
		// This covers the empty reason and the free-text one with a single
		// check. An unrecognised category is refused rather than passed through
		// precisely because passing it through is how an error message becomes
		// a "category".
		return fmt.Errorf("%w: a failed result carries no recognised reason", ErrProtocol)
	}
	return nil
}
