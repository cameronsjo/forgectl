package bless

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fixedTime is a deterministic clock for envelope timestamps in tests.
var fixedTime = time.Date(2026, 7, 12, 0, 0, 0, 0, time.UTC)

func genKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate P-256 key: %v", err)
	}
	return k
}

func mustPubDER(t *testing.T, pub *ecdsa.PublicKey) []byte {
	t.Helper()
	der, err := EncodePublicKey(pub)
	if err != nil {
		t.Fatalf("encode public key: %v", err)
	}
	return der
}

func writeFile(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// signB64 signs data under domain d with priv and returns the base64-std of the
// ASN.1/DER signature — the exact form an Envelope carries.
func signB64(t *testing.T, priv *ecdsa.PrivateKey, d Domain, data []byte) string {
	t.Helper()
	dg := TaggedDigest(d, data)
	sig, err := ecdsa.SignASN1(rand.Reader, priv, dg[:])
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return base64.StdEncoding.EncodeToString(sig)
}

func mustEncodeEnvelope(t *testing.T, e Envelope) []byte {
	t.Helper()
	b, err := EncodeEnvelope(e)
	if err != nil {
		t.Fatalf("encode envelope: %v", err)
	}
	return b
}

func mustEncodeStore(t *testing.T, s Store) []byte {
	t.Helper()
	b, err := EncodeStore(s)
	if err != nil {
		t.Fatalf("encode store: %v", err)
	}
	return b
}

// testEnv is a fully-wired, on-disk trust environment: an anchor file, a trust
// store with one enrolled signer key, and the store's anchor-signed sidecar.
// Tests bless a workflow with signerKey (the enrolled key) and verify through
// verifier(), whose ownership check is stubbed to pass so the ownership branch
// is exercised separately.
type testEnv struct {
	dir        string
	anchorPath string
	storePath  string
	anchorKey  *ecdsa.PrivateKey
	signerKey  *ecdsa.PrivateKey
	signerID   string
	anchorFP   string
	storeBytes []byte
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	dir := t.TempDir()

	anchorKey := genKey(t)
	signerKey := genKey(t)

	anchorDER := mustPubDER(t, &anchorKey.PublicKey)
	anchorFP := Fingerprint(anchorDER)
	anchorPath := filepath.Join(dir, "trust-anchor.pub")
	writeFile(t, anchorPath, []byte(base64.StdEncoding.EncodeToString(anchorDER)+"\n"))

	signerDER := mustPubDER(t, &signerKey.PublicKey)
	signerFP := Fingerprint(signerDER)

	store := Store{
		Schema:      StoreSchema,
		AnchorKeyID: anchorFP,
		Keys: []TrustedKey{{
			KeyID:   signerFP,
			Machine: "test",
			Pubkey:  base64.StdEncoding.EncodeToString(signerDER),
			AddedAt: fixedTime.Format(time.RFC3339),
		}},
	}
	storeBytes := mustEncodeStore(t, store)
	storePath := filepath.Join(dir, "trust.toml")
	writeFile(t, storePath, storeBytes)

	storeEnv := Envelope{
		Schema:    EnvelopeSchema,
		Algo:      AlgoECDSAP256SHA256,
		KeyID:     anchorFP,
		Signature: signB64(t, anchorKey, DomainTrust, storeBytes),
		SignedAt:  fixedTime.Format(time.RFC3339),
	}
	writeFile(t, SidecarPath(storePath), mustEncodeEnvelope(t, storeEnv))

	return &testEnv{
		dir:        dir,
		anchorPath: anchorPath,
		storePath:  storePath,
		anchorKey:  anchorKey,
		signerKey:  signerKey,
		signerID:   signerFP,
		anchorFP:   anchorFP,
		storeBytes: storeBytes,
	}
}

// verifier returns a Verifier wired to this env with a passing ownership check.
func (e *testEnv) verifier() *Verifier {
	return &Verifier{
		anchorPath:     e.anchorPath,
		anchorCheck:    func(string) error { return nil },
		trustStorePath: func() (string, error) { return e.storePath, nil },
	}
}

// writeWorkflow writes a workflow file and returns its path.
func (e *testEnv) writeWorkflow(t *testing.T, data []byte) string {
	t.Helper()
	p := filepath.Join(e.dir, "x.workflow.toml")
	writeFile(t, p, data)
	return p
}

// bless writes a valid workflow sidecar next to wfPath, signed by the enrolled
// signer key over the workflow domain of signedData.
func (e *testEnv) bless(t *testing.T, wfPath string, signedData []byte) {
	t.Helper()
	env := Envelope{
		Schema:    EnvelopeSchema,
		Algo:      AlgoECDSAP256SHA256,
		KeyID:     e.signerID,
		Signature: signB64(t, e.signerKey, DomainWorkflow, signedData),
		SignedAt:  fixedTime.Format(time.RFC3339),
	}
	writeFile(t, SidecarPath(wfPath), mustEncodeEnvelope(t, env))
}

// signASN1 signs a raw digest with priv and returns the ASN.1/DER signature.
func signASN1(priv *ecdsa.PrivateKey, digest []byte) ([]byte, error) {
	return ecdsa.SignASN1(rand.Reader, priv, digest)
}

// verifyASN1 reports whether sig is a valid ASN.1/DER signature over digest.
func verifyASN1(pub *ecdsa.PublicKey, digest, sig []byte) bool {
	return ecdsa.VerifyASN1(pub, digest, sig)
}

// equalStrings reports slice equality — kept local so argv assertions read plainly.
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// exitErr is a test error carrying an ExitCode, mimicking os/exec.ExitError so
// mapHelperError's errors.As walk can find it.
type exitErr struct{ code int }

func (e exitErr) Error() string { return fmt.Sprintf("exit status %d", e.code) }
func (e exitErr) ExitCode() int { return e.code }
