package pr

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

// The #218 defect, stated as a test: a synthetic local session and a real PR
// under a forge owner literally named "local" could land on the same tmux
// window name. Typed keys make the two unrepresentable as one value.
func TestSessionKeyLocalAndRemoteCollisionPairDiffer(t *testing.T) {
	local, err := localSessionKey("0123456")
	if err != nil {
		t.Fatalf("localSessionKey: %v", err)
	}
	// The historical collision: owner "local", repo = the 7-char short oid,
	// number = the value derived from the oid's first 6 hex digits.
	remote, err := remoteSessionKey("local", "0123456", 0x012345)
	if err != nil {
		t.Fatalf("remoteSessionKey: %v", err)
	}
	if local.equal(remote) {
		t.Fatal("local and remote keys compared equal")
	}
	if bytes.Equal(local.canonical(), remote.canonical()) {
		t.Fatal("local and remote keys encoded to identical canonical bytes")
	}
}

func TestSessionKeyRemoteIsCaseInsensitive(t *testing.T) {
	lower, err := remoteSessionKey("cameronsjo", "forgectl", 218)
	if err != nil {
		t.Fatalf("lower: %v", err)
	}
	upper, err := remoteSessionKey("CameronSjo", "ForgeCtl", 218)
	if err != nil {
		t.Fatalf("upper: %v", err)
	}
	if !lower.equal(upper) {
		t.Fatal("mixed-case owner/repo did not compare equal")
	}
	if !bytes.Equal(lower.canonical(), upper.canonical()) {
		t.Fatal("mixed-case owner/repo encoded differently")
	}
}

func TestSessionKeyLocalIsCaseInsensitive(t *testing.T) {
	lower, err := localSessionKey("0abcdef")
	if err != nil {
		t.Fatalf("lower: %v", err)
	}
	upper, err := localSessionKey("0ABCDEF")
	if err != nil {
		t.Fatalf("upper: %v", err)
	}
	if !lower.equal(upper) {
		t.Fatal("mixed-case oid did not compare equal")
	}
}

func TestSessionKeyRoundTripIsByteIdentical(t *testing.T) {
	keys := []struct {
		name string
		make func() (prSessionKey, error)
	}{
		{"local-short", func() (prSessionKey, error) { return localSessionKey("0123456") }},
		{"local-sha1", func() (prSessionKey, error) {
			return localSessionKey(strings.Repeat("ab", 20))
		}},
		{"local-sha256", func() (prSessionKey, error) {
			return localSessionKey(strings.Repeat("ab", 32))
		}},
		{"remote", func() (prSessionKey, error) { return remoteSessionKey("owner", "repo.name", 42) }},
		{"remote-max", func() (prSessionKey, error) {
			return remoteSessionKey("o", "r", maxPRNumber)
		}},
	}
	for _, tc := range keys {
		t.Run(tc.name, func(t *testing.T) {
			key, err := tc.make()
			if err != nil {
				t.Fatalf("construct: %v", err)
			}
			encoded := key.canonical()
			decoded, err := decodeSessionKey(encoded)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if !decoded.equal(key) {
				t.Fatalf("decoded key differs: %+v vs %+v", decoded, key)
			}
			if !bytes.Equal(decoded.canonical(), encoded) {
				t.Fatal("re-encoding a decoded key was not byte-identical")
			}
		})
	}
}

func TestSessionKeyConstructorsRejectInvalidInput(t *testing.T) {
	if _, err := localSessionKey(""); err == nil {
		t.Error("empty oid accepted")
	}
	if _, err := localSessionKey("012345"); err == nil {
		t.Error("6-char oid accepted (no declared algorithm has that width)")
	}
	if _, err := localSessionKey("012345g"); err == nil {
		t.Error("non-hex oid accepted")
	}
	if _, err := remoteSessionKey("", "repo", 1); err == nil {
		t.Error("empty owner accepted")
	}
	if _, err := remoteSessionKey("owner", "repo", 0); err == nil {
		t.Error("zero PR number accepted")
	}
	if _, err := remoteSessionKey("owner", "repo", -1); err == nil {
		t.Error("negative PR number accepted")
	}
	if _, err := remoteSessionKey("ow ner", "repo", 1); err == nil {
		t.Error("owner outside the charset accepted")
	}
	if _, err := remoteSessionKey("-owner", "repo", 1); err == nil {
		t.Error("option-like owner accepted")
	}
}

// The canonical encoding prefixes each string with a uint16 length, and
// ValidOwnerRepoPart bounds the charset but not the length. Without an explicit
// bound, an owner of 65536+ bytes wraps that prefix to a short one — and two
// different owners can then encode to identical bytes, which is a forged
// identity, not merely a malformed one.
func TestSessionKeyRejectsOverlongFields(t *testing.T) {
	long := strings.Repeat("a", maxSessionKeyFieldBytes+1)
	if _, err := remoteSessionKey(long, "repo", 1); err == nil {
		t.Error("an overlong owner was accepted")
	}
	if _, err := remoteSessionKey("owner", long, 1); err == nil {
		t.Error("an overlong repo was accepted")
	}
	// The pair that would collide if the length prefix wrapped: identical for
	// the first 65536 bytes, different after. Neither may encode at all.
	wrapA := strings.Repeat("a", 1<<16)
	wrapB := wrapA + "b"
	keyA, errA := remoteSessionKey(wrapA, "r", 1)
	keyB, errB := remoteSessionKey(wrapB, "r", 1)
	if errA == nil || errB == nil {
		t.Fatalf("owners at the uint16 boundary were accepted: %v / %v — encoded %q vs %q",
			errA, errB, keyA.canonical(), keyB.canonical())
	}
}

func TestDecodeSessionKeyRejectsOverlongFields(t *testing.T) {
	// Hand-build an encoding whose owner exceeds the bound. It is representable
	// on the wire (the uint16 prefix holds it), so only an explicit check in
	// decode refuses it — and it must, or decode would mint keys the
	// constructors cannot. The prefix is written by hand rather than through
	// appendLenString, which refuses to encode an out-of-bound field at all.
	long := strings.Repeat("a", maxSessionKeyFieldBytes+1)
	b := []byte(sessionKeyMagic)
	b = append(b, sessionKeyVersion, byte(kindRemote))
	b = binary.BigEndian.AppendUint16(b, uint16(len(long)))
	b = append(b, long...)
	b = appendLenString(b, "repo")
	b = append(b, 0, 0, 0, 0, 0, 0, 0, 1)
	if _, err := decodeSessionKey(b); err == nil {
		t.Fatal("decode accepted an overlong owner")
	}
}

func TestDecodeSessionKeyRejectsNoncanonicalBytes(t *testing.T) {
	valid, err := remoteSessionKey("owner", "repo", 7)
	if err != nil {
		t.Fatalf("remoteSessionKey: %v", err)
	}
	good := valid.canonical()

	corrupt := func(fn func(b []byte) []byte) []byte {
		b := make([]byte, len(good))
		copy(b, good)
		return fn(b)
	}

	cases := map[string][]byte{
		"empty":         {},
		"bad magic":     corrupt(func(b []byte) []byte { b[0] = 'X'; return b }),
		"bad version":   corrupt(func(b []byte) []byte { b[4] = 0x01; return b }),
		"unknown kind":  corrupt(func(b []byte) []byte { b[5] = 0x09; return b }),
		"truncated":     good[:len(good)-1],
		"trailing byte": append(append([]byte{}, good...), 0x00),
		"uppercase owner": func() []byte {
			return bytes.Replace(good, []byte("owner"), []byte("OWNER"), 1)
		}(),
	}
	for name, encoded := range cases {
		if _, err := decodeSessionKey(encoded); err == nil {
			t.Errorf("%s: decode accepted noncanonical bytes", name)
		}
	}
}

func TestDecodeSessionKeyRejectsZeroPRNumber(t *testing.T) {
	valid, err := remoteSessionKey("owner", "repo", 1)
	if err != nil {
		t.Fatalf("remoteSessionKey: %v", err)
	}
	b := valid.canonical()
	// The uint64 PR number is the final 8 bytes of a remote encoding.
	for i := len(b) - 8; i < len(b); i++ {
		b[i] = 0
	}
	if _, err := decodeSessionKey(b); err == nil {
		t.Fatal("decode accepted a zero PR number")
	}
}

func TestSessionKeyForRefMatchesKind(t *testing.T) {
	localRef := newLocalRef("0123456789abcdef0123456789abcdef01234567")
	key, err := sessionKeyForRef(localRef)
	if err != nil {
		t.Fatalf("sessionKeyForRef(local): %v", err)
	}
	if key.kind != kindLocal {
		t.Fatalf("local ref produced kind %#x", key.kind)
	}

	remoteRef, err := RefFromParts("local", localRef.Repo, "1193046")
	if err != nil {
		t.Fatalf("RefFromParts: %v", err)
	}
	remoteKey, err := sessionKeyForRef(remoteRef)
	if err != nil {
		t.Fatalf("sessionKeyForRef(remote): %v", err)
	}
	if remoteKey.kind != kindRemote {
		t.Fatalf("remote ref produced kind %#x", remoteKey.kind)
	}
	if key.equal(remoteKey) {
		t.Fatal("a local ref and a remote ref spelling the same parts share a key")
	}
}

// The uint16 length prefix is where a forged identity would be minted: two
// different owners encoding to identical canonical bytes, which the window-name
// digest is then taken over. The bound is enforced at the narrowing itself, so
// a future constructor that forgets checkFieldLength stops rather than wraps.
func TestAppendLenStringRefusesToWrapItsLengthPrefix(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("appendLenString encoded a field over the bound instead of panicking")
		}
	}()
	appendLenString(nil, strings.Repeat("a", maxSessionKeyFieldBytes+1))
}

// oidWidths is what the DECODER trusts and the width constants are what the
// CONSTRUCTOR switches on, so the two disagreeing is a real hazard: an
// algorithm present only in the map is decodable but unconstructible, and one
// present only in the constants names a tag the decoder refuses. Neither is a
// compile error, so it is asserted here.
func TestOidWidthsAgreeWithDeclaredConstants(t *testing.T) {
	want := map[string]int{
		oidAlgorithmShort:  oidWidthShort,
		oidAlgorithmSHA1:   oidWidthSHA1,
		oidAlgorithmSHA256: oidWidthSHA256,
	}
	if len(oidWidths) != len(want) {
		t.Fatalf("oidWidths has %d algorithms, want exactly %d", len(oidWidths), len(want))
	}
	for algorithm, width := range want {
		got, declared := oidWidths[algorithm]
		if !declared {
			t.Errorf("algorithm %q is missing from oidWidths", algorithm)
			continue
		}
		if got != width {
			t.Errorf("oidWidths[%q] is %d, want the declared constant %d", algorithm, got, width)
		}
	}
}

// The runtime half of the distinctness guarantee. The compile-time half — the
// constant switch in localSessionKey and the array-literal index assertion
// beside the constants — cannot see a duplicate introduced through oidWidths
// alone, and a duplicate width there means one algorithm's oids are silently
// filed under another's tag. The tag is inside the canonical bytes the window
// digest covers, so that is a wrong identity, not a wrong label.
func TestOidWidthsAreDistinct(t *testing.T) {
	seen := make(map[int]string, len(oidWidths))
	for algorithm, width := range oidWidths {
		if other, clash := seen[width]; clash {
			t.Errorf("algorithms %q and %q both declare width %d; a key of that width cannot name its own algorithm", other, algorithm, width)
			continue
		}
		seen[width] = algorithm
	}
}

// Distinctness stated as the behaviour it protects: every declared width must
// reach its OWN algorithm tag. A first-match switch over colliding widths passes
// every existing test — the key still builds and still names a window — and
// fails only here, where the tag itself is read back.
func TestLocalSessionKeyRoutesEachWidthToItsOwnAlgorithm(t *testing.T) {
	for algorithm, width := range oidWidths {
		t.Run(algorithm, func(t *testing.T) {
			key, err := localSessionKey(strings.Repeat("a", width))
			if err != nil {
				t.Fatalf("localSessionKey at width %d: %v", width, err)
			}
			if key.algorithm != algorithm {
				t.Fatalf("a %d-digit oid was filed under algorithm %q, want %q", width, key.algorithm, algorithm)
			}
			if key.oid != strings.Repeat("a", width) {
				t.Fatalf("oid round-tripped as %q", key.oid)
			}
		})
	}
}

// decodeSessionKey has no production caller yet (forgectl#299 item 1 brings
// one), so this is what exercises it: the property that makes it worth keeping.
// A decode that SUCCEEDS must re-encode byte-identically, because exactly one
// encoding per key is the premise the window-name digest rests on — accept a
// second spelling of one key and two canonical byte strings name one session,
// or one byte string names two.
//
// Failures are fine and expected; only a success that does not round-trip is a
// finding. Seeds are built programmatically rather than written as literals so
// no control or bidi byte is ever authored into this file.
func FuzzDecodeSessionKey(f *testing.F) {
	seed := func(k prSessionKey, err error) {
		if err != nil {
			f.Fatalf("seed construction: %v", err)
		}
		f.Add(k.canonical())
	}
	seed(localSessionKey(strings.Repeat("a", oidWidthShort)))
	seed(localSessionKey(strings.Repeat("b", oidWidthSHA1)))
	seed(localSessionKey(strings.Repeat("c", oidWidthSHA256)))
	seed(remoteSessionKey("owner", "repo.name", 42))
	seed(remoteSessionKey("o", "r", maxPRNumber))
	f.Add([]byte{})
	f.Add([]byte(sessionKeyMagic))
	f.Add([]byte(sessionKeyMagic + "\x02\x01"))

	// Near-misses that decode MUST refuse, seeded explicitly so the oracle has
	// teeth under a plain `go test` run rather than only under -fuzz. Each is a
	// second spelling of a key that already has one: normalize instead of
	// refusing any of them and two byte strings name one session.
	nearMissBase, err := remoteSessionKey("owner", "repo", 7)
	if err != nil {
		f.Fatalf("seed construction: %v", err)
	}
	good := nearMissBase.canonical()
	f.Add(bytes.Replace(good, []byte("owner"), []byte("OWNER"), 1))
	f.Add(append(append([]byte{}, good...), 0x00))

	f.Fuzz(func(t *testing.T, encoded []byte) {
		key, err := decodeSessionKey(encoded)
		if err != nil {
			return
		}
		reencoded := key.canonical()
		if !bytes.Equal(reencoded, encoded) {
			t.Fatalf("decode accepted a non-canonical encoding: %q re-encoded to %q", encoded, reencoded)
		}
		// A key that decodes must also survive its own re-encoding, or the
		// oracle above is only checking the first hop of a round trip.
		again, err := decodeSessionKey(reencoded)
		if err != nil {
			t.Fatalf("a key's own canonical bytes failed to decode: %v", err)
		}
		if !again.equal(key) {
			t.Fatalf("re-decoding a canonical encoding changed the identity")
		}
	})
}
