package bless

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"regexp"
	"strings"
	"testing"
)

func TestParsePublicKey_AcceptsP256(t *testing.T) {
	key := genKey(t)
	der := mustPubDER(t, &key.PublicKey)
	pub, err := ParsePublicKey(der)
	if err != nil {
		t.Fatalf("ParsePublicKey: %v", err)
	}
	if !pub.Equal(&key.PublicKey) {
		t.Error("parsed key does not equal the original")
	}
}

func TestParsePublicKey_RejectsNonECDSA(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate ed25519: %v", err)
	}
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatalf("marshal ed25519: %v", err)
	}
	if _, err := ParsePublicKey(der); err == nil {
		t.Fatal("expected ParsePublicKey to reject a non-ECDSA key")
	}
}

func TestParsePublicKey_RejectsWrongCurve(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatalf("generate P-384: %v", err)
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatalf("marshal P-384: %v", err)
	}
	if _, err := ParsePublicKey(der); err == nil {
		t.Fatal("expected ParsePublicKey to reject a non-P-256 curve")
	}
}

func TestFingerprint_Format(t *testing.T) {
	key := genKey(t)
	fp := Fingerprint(mustPubDER(t, &key.PublicKey))
	if !regexp.MustCompile(`^sha256:[0-9a-f]{64}$`).MatchString(fp) {
		t.Errorf("Fingerprint = %q, want sha256:<64 lowercase hex>", fp)
	}
	// Deterministic for the same DER.
	if fp != Fingerprint(mustPubDER(t, &key.PublicKey)) {
		t.Error("Fingerprint is not deterministic for identical DER")
	}
}

func TestParseAnchorFile(t *testing.T) {
	key := genKey(t)
	der := mustPubDER(t, &key.PublicKey)
	line := base64.StdEncoding.EncodeToString(der)

	t.Run("with trailing newline", func(t *testing.T) {
		pub, err := ParseAnchorFile([]byte(line + "\n"))
		if err != nil {
			t.Fatalf("ParseAnchorFile: %v", err)
		}
		if !pub.Equal(&key.PublicKey) {
			t.Error("parsed anchor key mismatch")
		}
	})
	t.Run("with surrounding whitespace", func(t *testing.T) {
		if _, err := ParseAnchorFile([]byte("  " + line + " \n\n")); err != nil {
			t.Fatalf("ParseAnchorFile with whitespace: %v", err)
		}
	})
	t.Run("empty", func(t *testing.T) {
		if _, err := ParseAnchorFile([]byte("   \n")); err == nil {
			t.Fatal("expected an error for an empty anchor file")
		}
	})
	t.Run("bad base64", func(t *testing.T) {
		if _, err := ParseAnchorFile([]byte("not base64 !!!")); err == nil {
			t.Fatal("expected an error for non-base64 anchor content")
		}
	})
	t.Run("valid base64 non-key", func(t *testing.T) {
		if _, err := ParseAnchorFile([]byte(base64.StdEncoding.EncodeToString([]byte("hello")))); err == nil {
			t.Fatal("expected an error for base64 that is not a PKIX key")
		}
	})
}

func TestEncodePublicKey_RoundTripsThroughParse(t *testing.T) {
	key := genKey(t)
	der, err := EncodePublicKey(&key.PublicKey)
	if err != nil {
		t.Fatalf("EncodePublicKey: %v", err)
	}
	pub, err := ParsePublicKey(der)
	if err != nil {
		t.Fatalf("ParsePublicKey: %v", err)
	}
	if !pub.Equal(&key.PublicKey) {
		t.Error("round-trip key mismatch")
	}
	// Fingerprint over canonical DER is stable.
	if !strings.HasPrefix(Fingerprint(der), "sha256:") {
		t.Error("fingerprint prefix")
	}
}
