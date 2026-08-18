package surface

import (
	"bytes"
	"encoding/binary"
	"errors"
	"net"
	"os"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
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

	payload, err := readFrame(&buf, maxFrameBytes)
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

	// The error alone does not prove the ordering this test is named for: a
	// reader that allocated the claim *first* and then refused it returns the
	// identical error. So the allocation is measured. Under a reader that sizes
	// the buffer before checking the cap, this reports ~4 GiB.
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	_, err := readFrame(bytes.NewReader(header[:]), maxFrameBytes)
	runtime.ReadMemStats(&after)

	if !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("readFrame on a 4GiB claim = %v, want ErrFrameTooLarge", err)
	}
	if grew := after.TotalAlloc - before.TotalAlloc; grew > maxFrameBytes {
		t.Errorf("refusing a 4 GiB claim allocated %d bytes; the cap must be checked "+
			"before the buffer is sized", grew)
	}

	// Exactly at the cap is accepted; one over is not. The boundary is where a
	// cap is usually wrong.
	atCap := make([]byte, 4+maxFrameBytes)
	binary.BigEndian.PutUint32(atCap[:4], maxFrameBytes)
	if _, err := readFrame(bytes.NewReader(atCap), maxFrameBytes); err != nil {
		t.Errorf("readFrame at exactly the cap = %v, want success", err)
	}

	overCap := make([]byte, 4)
	binary.BigEndian.PutUint32(overCap, maxFrameBytes+1)
	if _, err := readFrame(bytes.NewReader(overCap), maxFrameBytes); !errors.Is(err, ErrFrameTooLarge) {
		t.Errorf("readFrame one byte over the cap = %v, want ErrFrameTooLarge", err)
	}
}

// TestReadFrame_HonoursTheCallerCap covers the smaller budget an
// *unauthenticated* peer gets. A hello is a kind, a version, and 64 hex
// characters; granting it the invocation's megabyte would let anything that can
// reach the socket reserve one before proving a thing.
func TestReadFrame_HonoursTheCallerCap(t *testing.T) {
	claim := make([]byte, 4)
	binary.BigEndian.PutUint32(claim, maxHelloBytes+1)

	if _, err := readFrame(bytes.NewReader(claim), maxHelloBytes); !errors.Is(err, ErrFrameTooLarge) {
		t.Errorf("readFrame over the hello cap = %v, want ErrFrameTooLarge", err)
	}
	// The control: the same claim is fine under the invocation's cap, so the
	// refusal above is the caller's limit binding rather than a constant.
	withBody := make([]byte, 4+maxHelloBytes+1)
	binary.BigEndian.PutUint32(withBody[:4], maxHelloBytes+1)
	if _, err := readFrame(bytes.NewReader(withBody), maxFrameBytes); err != nil {
		t.Errorf("readFrame under the invocation cap = %v, want success", err)
	}
	if maxHelloBytes >= maxFrameBytes {
		t.Fatal("the hello cap is not smaller than the invocation cap; the test above proves nothing")
	}
}

// TestReadFrame_AllocatesWhatArrivesNotWhatIsClaimed is the other half of the
// size story. The cap bounds one frame; this bounds what an *undelivered* claim
// costs, so a peer cannot reserve the maximum by promising it.
func TestReadFrame_AllocatesWhatArrivesNotWhatIsClaimed(t *testing.T) {
	claim := make([]byte, 4, 5)
	binary.BigEndian.PutUint32(claim, maxFrameBytes)
	claim = append(claim, 'x') // one byte of a megabyte-sized promise

	payload, err := readFrame(bytes.NewReader(claim), maxFrameBytes)
	if err == nil {
		t.Fatalf("readFrame accepted a frame short of its claim, returning %d bytes", len(payload))
	}
	if !errors.Is(err, ErrTransport) {
		t.Errorf("an undelivered body = %v, want ErrTransport", err)
	}
}

// TestReadFrame_RefusesTruncationAndEmptiness covers the shapes a peer that
// disconnects mid-frame produces, which must not read as a valid empty message.
//
// The two sentinels are asserted separately on purpose. A peer that hung up
// mid-frame is a transport event; a peer that announced a zero-length frame is
// a protocol violation — it framed a message correctly and the message was
// nothing. Collapsing them would leave the launch state machine unable to tell
// "the socket died" from "the peer is misbehaving".
func TestReadFrame_RefusesTruncationAndEmptiness(t *testing.T) {
	transport := map[string][]byte{
		"nothing at all":      {},
		"partial header":      {0, 0, 1},
		"header without body": {0, 0, 0, 8},
		"body shorter than claimed": append(
			[]byte{0, 0, 0, 8}, []byte("abc")...),
	}
	for name, in := range transport {
		t.Run(name, func(t *testing.T) {
			_, err := readFrame(bytes.NewReader(in), maxFrameBytes)
			if !errors.Is(err, ErrTransport) {
				t.Errorf("readFrame(%s) = %v, want ErrTransport", name, err)
			}
			if errors.Is(err, ErrProtocol) {
				t.Errorf("readFrame(%s) reported a transport failure as a protocol violation", name)
			}
		})
	}

	_, err := readFrame(bytes.NewReader([]byte{0, 0, 0, 0}), maxFrameBytes)
	if !errors.Is(err, ErrProtocol) {
		t.Errorf("a zero-length frame = %v, want ErrProtocol", err)
	}
	if errors.Is(err, ErrTransport) {
		t.Error("a zero-length frame was reported as a transport failure")
	}
}

// TestErrorHierarchy pins the subset relation consumers will rely on.
//
// The obvious refusal predicate is errors.Is(err, ErrProtocol). If the specific
// sentinels are siblings rather than children, that predicate returns false for
// a version mismatch and an oversized frame — it fails *open* on exactly the
// refusals that matter, and every test in this file would still pass, because
// each asserts its own sentinel and never the relation between them.
func TestErrorHierarchy(t *testing.T) {
	if !errors.Is(ErrProtocolVersion, ErrProtocol) {
		t.Error("ErrProtocolVersion does not satisfy errors.Is(_, ErrProtocol)")
	}
	if !errors.Is(ErrFrameTooLarge, ErrProtocol) {
		t.Error("ErrFrameTooLarge does not satisfy errors.Is(_, ErrProtocol)")
	}
	if !errors.Is(ErrPeerUnsupported, ErrPeerIdentity) {
		t.Error("ErrPeerUnsupported does not satisfy errors.Is(_, ErrPeerIdentity)")
	}

	// The relation is one-way, or the specific sentinels would stop
	// discriminating and the hierarchy would buy nothing.
	if errors.Is(ErrProtocol, ErrProtocolVersion) {
		t.Error("a generic violation matches ErrProtocolVersion; the sentinels no longer discriminate")
	}
	if errors.Is(ErrProtocol, ErrFrameTooLarge) {
		t.Error("a generic violation matches ErrFrameTooLarge; the sentinels no longer discriminate")
	}

	// A transport failure is deliberately outside the protocol tree: a dead
	// socket is not a misbehaving peer.
	if errors.Is(ErrTransport, ErrProtocol) {
		t.Error("ErrTransport wraps ErrProtocol; a dead socket now reads as a protocol violation")
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
		// These two are the reason the trailing check is a second Decode
		// requiring io.EOF rather than Decoder.More. More only peeks for a
		// closing delimiter, so it answers "nothing follows" when what follows
		// *is* a closing delimiter — and both of these were accepted.
		"trailing close bracket": `{"kind":"hello","version":1,"nonce":"` + validNonceHex() + `"}]`,
		"trailing close brace":   `{"kind":"hello","version":1,"nonce":"` + validNonceHex() + `"}}`,
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

// TestDecodeFrame_ValidatesWhatItDecoded pins the second half of decodeFrame's
// job. Syntactically valid JSON that is a semantically invalid frame must not
// come back clean — leaving validation to the caller means three frames and
// three chances to forget, and the one that forgets is the one that matters.
func TestDecodeFrame_ValidatesWhatItDecoded(t *testing.T) {
	// Well-formed JSON, correct field names, no unknown fields — and a nonce of
	// the wrong width. Only validate() catches it.
	shortNonce := `{"kind":"hello","version":1,"nonce":"abcd"}`
	var f helloFrame
	if err := decodeFrame([]byte(shortNonce), &f); !errors.Is(err, ErrProtocol) {
		t.Errorf("decodeFrame on a decodable but invalid frame = %v, want ErrProtocol", err)
	}

	// And a result frame that decodes cleanly while carrying a contradiction.
	startedWithReason := `{"kind":"exec_started","version":1,"reason":"chdir"}`
	var r resultFrame
	if err := decodeFrame([]byte(startedWithReason), &r); !errors.Is(err, ErrProtocol) {
		t.Errorf("decodeFrame on a contradictory result = %v, want ErrProtocol", err)
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

// TestVersionMismatch_IsReportedByEveryValidator covers the other two. A
// mismatch is the same event wherever it is noticed, and a validator that
// reported it as a generic violation would send an operator hunting a malformed
// message instead of a version skew.
func TestVersionMismatch_IsReportedByEveryValidator(t *testing.T) {
	badInvocation := invocationFrame{
		Kind: kindInvocation, Version: ProtocolVersion + 1,
		Path: "/bin/true", CWD: "/",
	}
	if err := badInvocation.validate(); !errors.Is(err, ErrProtocolVersion) {
		t.Errorf("an invocation version mismatch = %v, want ErrProtocolVersion", err)
	}
	badResult := resultFrame{Kind: kindExecStarted, Version: ProtocolVersion + 1}
	if err := badResult.validate(); !errors.Is(err, ErrProtocolVersion) {
		t.Errorf("a result version mismatch = %v, want ErrProtocolVersion", err)
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
		// "=VALUE" carries a separator and names nothing. exec passes it to the
		// child as a variable with an empty name, so a bare Contains("=") test
		// admits exactly the shape this check exists to reject.
		"env with an empty name": {
			Kind: kindInvocation, Version: ProtocolVersion, Path: good.Path, CWD: good.CWD,
			Env: []string{"=orphaned"},
		},
		"env that is only a separator": {
			Kind: kindInvocation, Version: ProtocolVersion, Path: good.Path, CWD: good.CWD,
			Env: []string{"="},
		},
		// NUL is refused here rather than left to exec, whose own error quotes
		// the offending value — the one thing this protocol exists to keep out
		// of a log.
		"NUL in the path": {
			Kind: kindInvocation, Version: ProtocolVersion, Path: "/bin/tr\x00ue", CWD: good.CWD,
		},
		"NUL in the cwd": {
			Kind: kindInvocation, Version: ProtocolVersion, Path: good.Path, CWD: "/tmp/a\x00b",
		},
		"NUL in an argument": {
			Kind: kindInvocation, Version: ProtocolVersion, Path: good.Path, CWD: good.CWD,
			Args: []string{"--model\x00opus"},
		},
		// A path is rendered — by the service's logs, by launch's consumers —
		// so an escape sequence in one is interpreted rather than displayed.
		// Every other file in this package already refuses these; this was the
		// one place an untrusted path arrived without the check.
		"escape sequence in the path": {
			Kind: kindInvocation, Version: ProtocolVersion,
			Path: "/bin/\x1b[2Ktrue", CWD: good.CWD,
		},
		"escape sequence in the cwd": {
			Kind: kindInvocation, Version: ProtocolVersion,
			Path: good.Path, CWD: "/tmp/\x1b]0;pwned\x07x",
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
		// The entries are real assignments, not empty strings. A fixture of
		// `make([]string, maxEnv+1)` proves nothing about the count bound: every
		// one of those entries is an empty string, so the NAME=VALUE rule
		// refuses the first one and the count is never reached. Deleting the
		// count check entirely left that version of this case green.
		"too many env entries": {
			Kind: kindInvocation, Version: ProtocolVersion, Path: good.Path, CWD: good.CWD,
			Env: slices.Repeat([]string{"K=v"}, maxEnv+1),
		},
		"too many arguments, each valid": {
			Kind: kindInvocation, Version: ProtocolVersion, Path: good.Path, CWD: good.CWD,
			Args: slices.Repeat([]string{"-v"}, maxArgs+1),
		},
	}

	for name, f := range tests {
		t.Run(name, func(t *testing.T) {
			if err := f.validate(); !errors.Is(err, ErrProtocol) {
				t.Errorf("validate(%s) = %v, want ErrProtocol", name, err)
			}
		})
	}

	// The collection bounds accept their exact maximum. Without this, "one over
	// is refused" is consistent with a bound that is off by one in the other
	// direction and refuses a legitimate launch.
	atMax := good
	atMax.Args = slices.Repeat([]string{"-v"}, maxArgs)
	atMax.Env = slices.Repeat([]string{"K=v"}, maxEnv)
	if err := atMax.validate(); err != nil {
		t.Errorf("a frame at exactly the collection caps was refused: %v", err)
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
	failed := resultFrame{Kind: kindExecFailed, Version: ProtocolVersion, Reason: FailChdir}
	if err := failed.validate(); err != nil {
		t.Fatalf("a plain failed result was refused: %v", err)
	}

	tests := map[string]resultFrame{
		"started with a reason": {Kind: kindExecStarted, Version: ProtocolVersion, Reason: FailChdir},
		"failed with no reason": {Kind: kindExecFailed, Version: ProtocolVersion},
		// The reason is a closed category, so free text is refused outright.
		// A byte cap alone would admit this: the string below is well under any
		// plausible cap, and it is exactly the shape a truncated os error takes.
		"failed with a message instead of a category": {
			Kind: kindExecFailed, Version: ProtocolVersion,
			Reason: ExecFailure("fork/exec /opt/harness/bin/claude: permission denied"),
		},
		"failed with an unknown category": {
			Kind: kindExecFailed, Version: ProtocolVersion, Reason: ExecFailure("chdir "),
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

// TestExchange_BoundsAPeerThatNeverFinishes is the slowloris property, and it
// is the failure the size caps do not touch.
//
// A peer sends a length header and then simply stops. Nothing is oversized and
// nothing is malformed, so every cap in this file is satisfied — readFrame just
// blocks on io.ReadFull forever. A hostile terminal manager holds the socket
// path, so this is a connection it can open at will.
//
// The assertion is that the deadline is a property of *having* an exchange, not
// of the caller remembering to set one: newExchange is the only way to get one,
// and it sets the bound at construction.
func TestExchange_BoundsAPeerThatNeverFinishes(t *testing.T) {
	ours, theirs := net.Pipe()
	t.Cleanup(func() { _ = ours.Close() })
	t.Cleanup(func() { _ = theirs.Close() })

	ex, err := newExchange(ours, 150*time.Millisecond)
	if err != nil {
		t.Fatalf("newExchange: %v", err)
	}

	// A header claiming a body that never arrives.
	go func() {
		var header [4]byte
		binary.BigEndian.PutUint32(header[:], 64)
		_, _ = theirs.Write(header[:])
		// and then nothing, forever
	}()

	done := make(chan error, 1)
	go func() {
		_, readErr := ex.read(maxHelloBytes)
		done <- readErr
	}()

	select {
	case err := <-done:
		if !errors.Is(err, ErrTransport) {
			t.Errorf("a stalled peer = %v, want ErrTransport", err)
		}
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Errorf("a stalled peer did not surface a deadline: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("readFrame never returned against a peer that stopped mid-frame")
	}
}

// TestExchange_DeadlineCoversTheWholeConversation pins one deadline for three
// frames rather than one per frame. Per-frame timers let a peer stall three
// times the budget by sending each frame just before its own timer expires.
func TestExchange_DeadlineCoversTheWholeConversation(t *testing.T) {
	ours, theirs := net.Pipe()
	t.Cleanup(func() { _ = ours.Close() })
	t.Cleanup(func() { _ = theirs.Close() })

	const (
		budget    = 600 * time.Millisecond
		firstWait = 400 * time.Millisecond
	)

	start := time.Now()
	ex, err := newExchange(ours, budget)
	if err != nil {
		t.Fatalf("newExchange: %v", err)
	}

	// The peer spends most of the budget before answering the first frame, then
	// goes quiet. Under one conversation-wide deadline the whole exchange ends
	// at `budget`. Under a per-frame deadline the second read would restart the
	// clock and the exchange would run to firstWait+budget instead — which is
	// what this measures, rather than the second read's own duration, because
	// total elapsed is the property and is not sensitive to how the wait splits
	// across the two reads.
	go func() {
		time.Sleep(firstWait)
		var buf bytes.Buffer
		_ = writeFrame(&buf, helloFrame{
			Kind: kindHello, Version: ProtocolVersion, Nonce: validNonceHex(),
		})
		_, _ = theirs.Write(buf.Bytes())
	}()

	if _, err := ex.read(maxHelloBytes); err != nil {
		t.Fatalf("the first frame did not arrive: %v", err)
	}

	// The second read is run under a watchdog rather than inline. With no
	// deadline at all it blocks forever, and a test whose failure mode is an
	// unbounded hang reports "the suite stopped responding" instead of "the
	// deadline is missing".
	second := make(chan error, 1)
	go func() {
		_, readErr := ex.read(maxHelloBytes)
		second <- readErr
	}()
	select {
	case err := <-second:
		if !errors.Is(err, ErrTransport) {
			t.Errorf("the second read = %v, want ErrTransport", err)
		}
	case <-time.After(firstWait + budget + time.Second):
		t.Fatal("the second read never returned; the conversation has no deadline")
	}

	elapsed := time.Since(start)
	// The ceiling sits between the two outcomes — well above `budget` so a
	// loaded machine does not fail it, and well below firstWait+budget so a
	// per-frame reset cannot pass it.
	const ceiling = budget + (firstWait / 2)
	if elapsed > ceiling {
		t.Errorf("the conversation ran %v against a %v budget; the deadline was reset "+
			"per frame rather than covering the whole exchange", elapsed, budget)
	}
}

// TestWriteFrame_RefusesAnOversizedPayload closes the producing side of the
// cap. Without it a bug on our end could emit a frame our own reader refuses.
func TestWriteFrame_RefusesAnOversizedPayload(t *testing.T) {
	// Every entry is individually legal and the collections are at their caps —
	// so the frame passes validate() and the *size* cap is what refuses it. An
	// entry over maxEntryBytes would be refused by validation first and this
	// test would pass without the size check existing at all.
	huge := invocationFrame{
		Kind: kindInvocation, Version: ProtocolVersion,
		Path: "/bin/true", CWD: "/",
		Args: slices.Repeat([]string{strings.Repeat("a", 300)}, maxArgs),
	}
	if err := huge.validate(); err != nil {
		t.Fatalf("the oversized fixture is invalid for another reason: %v", err)
	}

	var buf bytes.Buffer
	if err := writeFrame(&buf, huge); !errors.Is(err, ErrFrameTooLarge) {
		t.Errorf("writeFrame on an oversized frame = %v, want ErrFrameTooLarge", err)
	}
	if buf.Len() != 0 {
		t.Errorf("a refused writeFrame still wrote %d bytes", buf.Len())
	}
}

// TestWriteFrame_RefusesToSendAnInvalidFrame closes the sending side.
//
// Without it, a bug on our end emits a message the far end refuses, and the
// failure surfaces there as a protocol violation — attributed to the peer. A
// fault of ours must not read as an attack from theirs.
func TestWriteFrame_RefusesToSendAnInvalidFrame(t *testing.T) {
	var buf bytes.Buffer
	invalid := invocationFrame{
		Kind: kindInvocation, Version: ProtocolVersion,
		Path: "claude", CWD: "/", // relative path — our own validator refuses it
	}
	if err := writeFrame(&buf, invalid); !errors.Is(err, ErrProtocol) {
		t.Errorf("writeFrame on an invalid frame = %v, want ErrProtocol", err)
	}
	if buf.Len() != 0 {
		t.Errorf("a refused writeFrame still wrote %d bytes", buf.Len())
	}

	// The control: the same frame with an absolute path is written.
	valid := invalid
	valid.Path = "/bin/true"
	if err := writeFrame(&buf, valid); err != nil {
		t.Errorf("writeFrame refused a valid frame: %v", err)
	}
}
