package bless

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestTaggedDigest_DomainSeparation(t *testing.T) {
	data := []byte("identical bytes")
	if TaggedDigest(DomainWorkflow, data) == TaggedDigest(DomainTrust, data) {
		t.Fatal("workflow and trust digests of identical bytes must differ (domain separation)")
	}
	// The tag is a prefix of the pre-image, not the raw data.
	if TaggedDigest(DomainWorkflow, data) == TaggedDigest(DomainWorkflow, append([]byte(DomainWorkflow), data...)) {
		t.Fatal("digest should hash tag‖data, not accept a pre-tagged payload")
	}
}

func TestSidecarPath(t *testing.T) {
	if got := SidecarPath("/a/b/x.workflow.toml"); got != "/a/b/x.workflow.toml.blessing" {
		t.Errorf("SidecarPath = %q", got)
	}
}

func TestEncodeDecodeEnvelope_RoundTrip(t *testing.T) {
	key := genKey(t)
	e := Envelope{
		Schema:    EnvelopeSchema,
		Algo:      AlgoECDSAP256SHA256,
		KeyID:     Fingerprint(mustPubDER(t, &key.PublicKey)),
		Signature: signB64(t, key, DomainWorkflow, []byte("data")),
		SignedAt:  fixedTime.Format(time.RFC3339),
	}
	encoded := mustEncodeEnvelope(t, e)
	got, err := DecodeEnvelope(encoded)
	if err != nil {
		t.Fatalf("DecodeEnvelope: %v", err)
	}
	if got != e {
		t.Errorf("round-trip mismatch:\n got %+v\nwant %+v", got, e)
	}
}

func TestDecodeEnvelope_Rejections(t *testing.T) {
	key := genKey(t)
	validKeyID := Fingerprint(mustPubDER(t, &key.PublicKey))
	validSig := signB64(t, key, DomainWorkflow, []byte("data"))

	tests := []struct {
		name string
		toml string
	}{
		{
			name: "unknown key",
			toml: "schema = 1\nalgo = \"ecdsa-p256-sha256\"\nkey_id = \"" + validKeyID + "\"\nsignature = \"" + validSig + "\"\nsigned_at = \"x\"\nbogus = true\n",
		},
		{
			name: "wrong schema",
			toml: "schema = 2\nalgo = \"ecdsa-p256-sha256\"\nkey_id = \"" + validKeyID + "\"\nsignature = \"" + validSig + "\"\nsigned_at = \"x\"\n",
		},
		{
			name: "wrong algo",
			toml: "schema = 1\nalgo = \"ed25519\"\nkey_id = \"" + validKeyID + "\"\nsignature = \"" + validSig + "\"\nsigned_at = \"x\"\n",
		},
		{
			name: "malformed key_id: too short",
			toml: "schema = 1\nalgo = \"ecdsa-p256-sha256\"\nkey_id = \"sha256:abc\"\nsignature = \"" + validSig + "\"\nsigned_at = \"x\"\n",
		},
		{
			name: "malformed key_id: uppercase hex",
			toml: "schema = 1\nalgo = \"ecdsa-p256-sha256\"\nkey_id = \"sha256:" + strings.Repeat("A", 64) + "\"\nsignature = \"" + validSig + "\"\nsigned_at = \"x\"\n",
		},
		{
			name: "malformed key_id: missing prefix",
			toml: "schema = 1\nalgo = \"ecdsa-p256-sha256\"\nkey_id = \"" + strings.Repeat("a", 64) + "\"\nsignature = \"" + validSig + "\"\nsigned_at = \"x\"\n",
		},
		{
			name: "signature not base64",
			toml: "schema = 1\nalgo = \"ecdsa-p256-sha256\"\nkey_id = \"" + validKeyID + "\"\nsignature = \"not!base64!\"\nsigned_at = \"x\"\n",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := DecodeEnvelope([]byte(tc.toml)); err == nil {
				t.Fatalf("expected an error for %s", tc.name)
			}
		})
	}
}

func TestEncodeEnvelope_ProducesExpectedKeys(t *testing.T) {
	key := genKey(t)
	e := Envelope{
		Schema:    EnvelopeSchema,
		Algo:      AlgoECDSAP256SHA256,
		KeyID:     Fingerprint(mustPubDER(t, &key.PublicKey)),
		Signature: signB64(t, key, DomainWorkflow, []byte("data")),
		SignedAt:  fixedTime.Format(time.RFC3339),
	}
	encoded := mustEncodeEnvelope(t, e)
	for _, want := range []string{"schema", "algo", "key_id", "signature", "signed_at"} {
		if !bytes.Contains(encoded, []byte(want)) {
			t.Errorf("encoded envelope missing key %q:\n%s", want, encoded)
		}
	}
}
