package surface

import (
	"bytes"
	"encoding/binary"
	"errors"
	"strings"
	"testing"
)

// The frame types and their validators are unexported, so these tests live
// in-package. What they hold is the wire contract: a peer on the other end of
// this socket is a process a hostile terminal manager could have started, so
// every bound here is load-bearing rather than defensive tidiness.

func validNonceHex() string { return strings.Repeat("ab", 32) }

// TestReadFrame_RoundTripsAFrame is the baseline. Without it every refusal
// test below could pass against a reader that rejects everything.
func TestReadFrame_RoundTripsAFrame(t *testing.T) {
	var buf bytes.Buffer
	sent := helloFrame{Kind: kindHello, Version: ProtocolVersion, Nonce: validNonceHex()}
	if err := writeFrame(&buf, sent); err != nil {
		t.Fatalf("writeFrame: %v", err)
	}

	payload, err := readFrame(&buf)
	if err != nil {
		t.Fatalf("readFrame: %v", err)
	}
	var got helloFrame
	if err := decodeFrame(payload, &got); err != nil {
		t.Fatalf("decodeFrame: %v", err)
	}
	if got != sent {
		t.Errorf("round trip changed the frame:\n got %+v\nwant %+v", got, sent)
	}
	if err := got.validate(); err != nil {
		t.Errorf("a round-tripped frame does not validate: %v", err)
	}
}

// TestReadFrame_ChecksTheLengthBeforeAllocating is the denial-of-service
// property, and it is why this is not a json.Decoder on the raw connection.
//
// The header claims a size; nothing follows it. A reader that allocated first
// would reserve the claimed amount before discovering the body is absent — so
// the assertion is that a 4 GiB claim is refused *cheaply*, with the frame-size
// error rather than a read error.
func TestReadFrame_ChecksTheLengthBeforeAllocating(t *testing.T) {
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], ^uint32(0)) // 4 GiB - 1

	_, err := readFrame(bytes.NewReader(header[:]))
	if !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("readFrame on a 4GiB claim = %v, want ErrFrameTooLarge", err)
	}

	// Exactly at the cap is accepted; one over is not. The boundary is where a
	// cap is usually wrong.
	atCap := make([]byte, 4+maxFrameBytes)
	binary.BigEndian.PutUint32(atCap[:4], maxFrameBytes)
	if _, err := readFrame(bytes.NewReader(atCap)); err != nil {
		t.Errorf("readFrame at exactly the cap = %v, want success", err)
	}

	overCap := make([]byte, 4)
	binary.BigEndian.PutUint32(overCap, maxFrameBytes+1)
	if _, err := readFrame(bytes.NewReader(overCap)); !errors.Is(err, ErrFrameTooLarge) {
		t.Errorf("readFrame one byte over the cap = %v, want ErrFrameTooLarge", err)
	}
}

// TestReadFrame_RefusesTruncationAndEmptiness covers the shapes a peer that
// disconnects mid-frame produces, which must not read as a valid empty message.
func TestReadFrame_RefusesTruncationAndEmptiness(t *testing.T) {
	tests := map[string][]byte{
		"nothing at all":      {},
		"partial header":      {0, 0, 1},
		"zero-length frame":   {0, 0, 0, 0},
		"header without body": {0, 0, 0, 8},
		"body shorter than claimed": append(
			[]byte{0, 0, 0, 8}, []byte("abc")...),
	}

	for name, in := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := readFrame(bytes.NewReader(in)); !errors.Is(err, ErrProtocol) {
				t.Errorf("readFrame(%s) = %v, want ErrProtocol", name, err)
			}
		})
	}
}

// TestDecodeFrame_RefusesUnknownFieldsAndTrailingContent keeps a message from
// being acted on with half its meaning missing. A field this build does not
// know may be one a newer build uses to mean something.
func TestDecodeFrame_RefusesUnknownFieldsAndTrailingContent(t *testing.T) {
	tests := map[string]string{
		"unknown field":    `{"kind":"hello","version":1,"nonce":"` + validNonceHex() + `","focus":true}`,
		"trailing object":  `{"kind":"hello","version":1,"nonce":"` + validNonceHex() + `"}{"kind":"hello"}`,
		"trailing garbage": `{"kind":"hello","version":1,"nonce":"` + validNonceHex() + `"} nonsense`,
		"not an object":    `"hello"`,
		"empty":            ``,
	}

	for name, in := range tests {
		t.Run(name, func(t *testing.T) {
			var f helloFrame
			if err := decodeFrame([]byte(in), &f); !errors.Is(err, ErrProtocol) {
				t.Errorf("decodeFrame(%s) = %v, want ErrProtocol", name, err)
			}
		})
	}

	// The control: the same document without the extra field decodes.
	var f helloFrame
	good := `{"kind":"hello","version":1,"nonce":"` + validNonceHex() + `"}`
	if err := decodeFrame([]byte(good), &f); err != nil {
		t.Errorf("decodeFrame refused a well-formed frame: %v", err)
	}
}

// TestHelloFrame_Validation pins the shape checks that precede the nonce
// comparison. The nonce's value is checked in constant time elsewhere; this
// only establishes there is something of the right shape to compare.
func TestHelloFrame_Validation(t *testing.T) {
	good := helloFrame{Kind: kindHello, Version: ProtocolVersion, Nonce: validNonceHex()}
	if err := good.validate(); err != nil {
		t.Fatalf("the valid fixture was refused: %v", err)
	}

	tests := map[string]helloFrame{
		"wrong kind":      {Kind: kindInvocation, Version: ProtocolVersion, Nonce: validNonceHex()},
		"unset kind":      {Version: ProtocolVersion, Nonce: validNonceHex()},
		"no nonce":        {Kind: kindHello, Version: ProtocolVersion},
		"short nonce":     {Kind: kindHello, Version: ProtocolVersion, Nonce: "abc"},
		"uppercase nonce": {Kind: kindHello, Version: ProtocolVersion, Nonce: strings.ToUpper(validNonceHex())},
		"non-hex nonce":   {Kind: kindHello, Version: ProtocolVersion, Nonce: strings.Repeat("z", 64)},
	}
	for name, f := range tests {
		t.Run(name, func(t *testing.T) {
			if err := f.validate(); !errors.Is(err, ErrProtocol) {
				t.Errorf("validate(%s) = %v, want ErrProtocol", name, err)
			}
		})
	}

	// A version mismatch reports itself as one rather than as a generic
	// violation, because it is the one case with an actionable cause.
	older := helloFrame{Kind: kindHello, Version: ProtocolVersion + 1, Nonce: validNonceHex()}
	if err := older.validate(); !errors.Is(err, ErrProtocolVersion) {
		t.Errorf("a version mismatch = %v, want ErrProtocolVersion", err)
	}
}

// TestInvocationFrame_Validation is the check the trampoline runs on what it
// was *sent*. Validating our own message sounds redundant and is not: the
// trampoline cannot tell a genuine outer process from something that reached
// the socket first, so it checks what it holds rather than trusting a sender.
func TestInvocationFrame_Validation(t *testing.T) {
	good := invocationFrame{
		Kind: kindInvocation, Version: ProtocolVersion,
		Path: "/opt/homebrew/bin/claude",
		Args: []string{"--model", "opus"},
		Env:  []string{"PATH=/usr/bin", "HOME=/Users/x"},
		CWD:  "/Users/x/Projects/thing",
	}
	if err := good.validate(); err != nil {
		t.Fatalf("the valid fixture was refused: %v", err)
	}

	oversized := strings.Repeat("v", maxEntryBytes+1)

	tests := map[string]invocationFrame{
		"wrong kind":    {Kind: kindHello, Version: ProtocolVersion, Path: good.Path, CWD: good.CWD},
		"relative path": {Kind: kindInvocation, Version: ProtocolVersion, Path: "claude", CWD: good.CWD},
		"no path":       {Kind: kindInvocation, Version: ProtocolVersion, CWD: good.CWD},
		"relative cwd":  {Kind: kindInvocation, Version: ProtocolVersion, Path: good.Path, CWD: "thing"},
		"no cwd":        {Kind: kindInvocation, Version: ProtocolVersion, Path: good.Path},
		"env with no equals": {
			Kind: kindInvocation, Version: ProtocolVersion, Path: good.Path, CWD: good.CWD,
			Env: []string{"NOT_AN_ASSIGNMENT"},
		},
		"oversized argument": {
			Kind: kindInvocation, Version: ProtocolVersion, Path: good.Path, CWD: good.CWD,
			Args: []string{oversized},
		},
		"oversized env entry": {
			Kind: kindInvocation, Version: ProtocolVersion, Path: good.Path, CWD: good.CWD,
			Env: []string{"K=" + oversized},
		},
		"too many arguments": {
			Kind: kindInvocation, Version: ProtocolVersion, Path: good.Path, CWD: good.CWD,
			Args: make([]string, maxArgs+1),
		},
		"too many env entries": {
			Kind: kindInvocation, Version: ProtocolVersion, Path: good.Path, CWD: good.CWD,
			Env: make([]string, maxEnv+1),
		},
	}

	for name, f := range tests {
		t.Run(name, func(t *testing.T) {
			if err := f.validate(); !errors.Is(err, ErrProtocol) {
				t.Errorf("validate(%s) = %v, want ErrProtocol", name, err)
			}
		})
	}
}

// TestResultFrame_StartedAndFailedAreExclusive keeps the commit signal
// unambiguous. A started result carrying a failure reason, or a failed one
// carrying none, is a message the service cannot act on.
func TestResultFrame_StartedAndFailedAreExclusive(t *testing.T) {
	started := resultFrame{Kind: kindExecStarted, Version: ProtocolVersion}
	if err := started.validate(); err != nil {
		t.Fatalf("a plain started result was refused: %v", err)
	}
	failed := resultFrame{Kind: kindExecFailed, Version: ProtocolVersion, Reason: "chdir"}
	if err := failed.validate(); err != nil {
		t.Fatalf("a plain failed result was refused: %v", err)
	}

	tests := map[string]resultFrame{
		"started with a reason": {Kind: kindExecStarted, Version: ProtocolVersion, Reason: "why"},
		"failed with no reason": {Kind: kindExecFailed, Version: ProtocolVersion},
		"oversized reason": {
			Kind: kindExecFailed, Version: ProtocolVersion,
			Reason: strings.Repeat("r", maxReasonBytes+1),
		},
		"wrong kind": {Kind: kindHello, Version: ProtocolVersion},
	}
	for name, f := range tests {
		t.Run(name, func(t *testing.T) {
			if err := f.validate(); !errors.Is(err, ErrProtocol) {
				t.Errorf("validate(%s) = %v, want ErrProtocol", name, err)
			}
		})
	}
}

// TestFrameKind_IsClosed keeps a decoded document from naming a message this
// build does not implement.
func TestFrameKind_IsClosed(t *testing.T) {
	for _, k := range []frameKind{kindHello, kindInvocation, kindExecStarted, kindExecFailed} {
		if !k.valid() {
			t.Errorf("%q is not accepted by its own validity check", k)
		}
	}
	for _, k := range []frameKind{"", "Hello", "HELLO", "exec", "invocation ", "shutdown"} {
		if k.valid() {
			t.Errorf("%q was accepted as a frame kind", k)
		}
	}
}

// TestWriteFrame_RefusesAnOversizedPayload closes the producing side of the
// cap. Without it a bug on our end could emit a frame our own reader refuses.
func TestWriteFrame_RefusesAnOversizedPayload(t *testing.T) {
	huge := invocationFrame{
		Kind: kindInvocation, Version: ProtocolVersion,
		Path: "/bin/true", CWD: "/",
		Args: []string{strings.Repeat("a", maxFrameBytes)},
	}
	var buf bytes.Buffer
	if err := writeFrame(&buf, huge); !errors.Is(err, ErrFrameTooLarge) {
		t.Errorf("writeFrame on an oversized frame = %v, want ErrFrameTooLarge", err)
	}
	if buf.Len() != 0 {
		t.Errorf("a refused writeFrame still wrote %d bytes", buf.Len())
	}
}
