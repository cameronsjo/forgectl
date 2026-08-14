package pr

import (
	"bytes"
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
	// constructors cannot.
	long := strings.Repeat("a", maxSessionKeyFieldBytes+1)
	b := []byte(sessionKeyMagic)
	b = append(b, sessionKeyVersion, byte(kindRemote))
	b = appendLenString(b, long)
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
