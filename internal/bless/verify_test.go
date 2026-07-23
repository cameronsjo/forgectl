package bless

import (
	"bytes"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

var workflowData = []byte("dsl_version = 1\nname = \"x\"\nversion = \"1.0.0\"\n")

func TestVerify_RoundTrip(t *testing.T) {
	env := newTestEnv(t)
	wf := env.writeWorkflow(t, workflowData)
	env.bless(t, wf, workflowData)
	if err := env.verifier().Verify(wf, workflowData); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

func TestVerify_TamperedData(t *testing.T) {
	env := newTestEnv(t)
	wf := env.writeWorkflow(t, workflowData)
	env.bless(t, wf, workflowData)
	// The blessing covers workflowData; verify against a one-byte edit.
	tampered := append([]byte{}, workflowData...)
	tampered[0] ^= 0x01
	err := env.verifier().Verify(wf, tampered)
	if !errors.Is(err, ErrTampered) {
		t.Fatalf("Verify tampered = %v, want ErrTampered", err)
	}
}

func TestVerify_MissingSidecar(t *testing.T) {
	env := newTestEnv(t)
	wf := env.writeWorkflow(t, workflowData)
	// No env.bless — no sidecar exists.
	err := env.verifier().Verify(wf, workflowData)
	if !errors.Is(err, ErrUnblessed) {
		t.Fatalf("Verify unblessed = %v, want ErrUnblessed", err)
	}
}

func TestVerify_UnknownKey(t *testing.T) {
	env := newTestEnv(t)
	wf := env.writeWorkflow(t, workflowData)
	// Sign with a key that is NOT in the store.
	stray := genKey(t)
	sidecar := Envelope{
		Schema:    EnvelopeSchema,
		Algo:      AlgoECDSAP256SHA256,
		KeyID:     Fingerprint(mustPubDER(t, &stray.PublicKey)),
		Signature: signB64(t, stray, DomainWorkflow, workflowData),
		SignedAt:  fixedTime.Format(time.RFC3339),
	}
	writeFile(t, SidecarPath(wf), mustEncodeEnvelope(t, sidecar))
	err := env.verifier().Verify(wf, workflowData)
	if !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("Verify unknown-key = %v, want ErrUnknownKey", err)
	}
}

func TestVerify_MalformedSidecarIsTampered(t *testing.T) {
	env := newTestEnv(t)
	wf := env.writeWorkflow(t, workflowData)
	writeFile(t, SidecarPath(wf), []byte("this is not valid envelope toml {"))
	err := env.verifier().Verify(wf, workflowData)
	if !errors.Is(err, ErrTampered) {
		t.Fatalf("Verify malformed sidecar = %v, want ErrTampered", err)
	}
}

func TestVerify_StoreBytesTampered(t *testing.T) {
	env := newTestEnv(t)
	wf := env.writeWorkflow(t, workflowData)
	env.bless(t, wf, workflowData)
	// Rewrite the store bytes after it was signed: the anchor signature no
	// longer matches.
	writeFile(t, env.storePath, append(env.storeBytes, []byte("\n# tampered\n")...))
	err := env.verifier().Verify(wf, workflowData)
	if !errors.Is(err, ErrTrustStoreInvalid) {
		t.Fatalf("Verify tampered-store = %v, want ErrTrustStoreInvalid", err)
	}
}

func TestVerify_StoreSidecarKeyIDMismatch(t *testing.T) {
	env := newTestEnv(t)
	wf := env.writeWorkflow(t, workflowData)
	env.bless(t, wf, workflowData)
	// Re-sign the store with a DIFFERENT key and advertise that key's id — the
	// store sidecar's key_id no longer equals the anchor fingerprint.
	other := genKey(t)
	badStoreEnv := Envelope{
		Schema:    EnvelopeSchema,
		Algo:      AlgoECDSAP256SHA256,
		KeyID:     Fingerprint(mustPubDER(t, &other.PublicKey)),
		Signature: signB64(t, other, DomainTrust, env.storeBytes),
		SignedAt:  fixedTime.Format(time.RFC3339),
	}
	writeFile(t, SidecarPath(env.storePath), mustEncodeEnvelope(t, badStoreEnv))
	err := env.verifier().Verify(wf, workflowData)
	if !errors.Is(err, ErrTrustStoreInvalid) {
		t.Fatalf("Verify store-keyid-mismatch = %v, want ErrTrustStoreInvalid", err)
	}
}

func TestVerify_AnchorFailures(t *testing.T) {
	env := newTestEnv(t)
	wf := env.writeWorkflow(t, workflowData)
	env.bless(t, wf, workflowData)

	t.Run("ownership check fails", func(t *testing.T) {
		v := env.verifier()
		v.anchorCheck = func(string) error { return errors.New("not root-owned") }
		if err := v.Verify(wf, workflowData); !errors.Is(err, ErrNoAnchor) {
			t.Fatalf("Verify = %v, want ErrNoAnchor", err)
		}
	})
	t.Run("anchor file missing", func(t *testing.T) {
		v := env.verifier()
		v.anchorPath = filepath.Join(env.dir, "does-not-exist")
		if err := v.Verify(wf, workflowData); !errors.Is(err, ErrNoAnchor) {
			t.Fatalf("Verify = %v, want ErrNoAnchor", err)
		}
	})
	t.Run("anchor file unparseable", func(t *testing.T) {
		bad := filepath.Join(env.dir, "bad-anchor")
		writeFile(t, bad, []byte("not base64 !!!"))
		v := env.verifier()
		v.anchorPath = bad
		if err := v.Verify(wf, workflowData); !errors.Is(err, ErrNoAnchor) {
			t.Fatalf("Verify = %v, want ErrNoAnchor", err)
		}
	})
}

func TestVerify_TrustStoreMissing(t *testing.T) {
	env := newTestEnv(t)
	wf := env.writeWorkflow(t, workflowData)
	env.bless(t, wf, workflowData)
	if err := os.Remove(env.storePath); err != nil {
		t.Fatalf("remove store: %v", err)
	}
	if err := env.verifier().Verify(wf, workflowData); !errors.Is(err, ErrTrustStoreInvalid) {
		t.Fatalf("Verify missing-store = %v, want ErrTrustStoreInvalid", err)
	}
}

// TestTrustedStore_MissingVsInvalid proves TrustedStore distinguishes a
// genuinely-absent store (ErrTrustStoreMissing) from a present-but-unreadable
// one (ErrTrustStoreInvalid only) — the distinction trust rebuild's peer-drop
// guard relies on so a transient read error can't be mistaken for "no store,
// safe to overwrite". Both cases still satisfy errors.Is(_, ErrTrustStoreInvalid)
// for backward compatibility.
func TestTrustedStore_MissingVsInvalid(t *testing.T) {
	env := newTestEnv(t)
	wf := env.writeWorkflow(t, workflowData)
	env.bless(t, wf, workflowData)

	// (a) genuinely absent → ErrTrustStoreMissing, and still ErrTrustStoreInvalid.
	if err := os.Remove(env.storePath); err != nil {
		t.Fatalf("remove store: %v", err)
	}
	_, err := env.verifier().TrustedStore()
	if !errors.Is(err, ErrTrustStoreMissing) {
		t.Errorf("absent store: TrustedStore = %v, want ErrTrustStoreMissing", err)
	}
	if !errors.Is(err, ErrTrustStoreInvalid) {
		t.Errorf("absent store: ErrTrustStoreMissing must still wrap ErrTrustStoreInvalid, got %v", err)
	}

	// (b) present but corrupt → ErrTrustStoreInvalid, NOT Missing (it exists).
	if err := os.WriteFile(env.storePath, []byte("not a valid trust store"), 0o644); err != nil {
		t.Fatalf("write corrupt store: %v", err)
	}
	_, err = env.verifier().TrustedStore()
	if !errors.Is(err, ErrTrustStoreInvalid) {
		t.Errorf("corrupt store: TrustedStore = %v, want ErrTrustStoreInvalid", err)
	}
	if errors.Is(err, ErrTrustStoreMissing) {
		t.Errorf("corrupt store: must NOT be ErrTrustStoreMissing (the file exists), got %v", err)
	}
}

// TestVerify_DomainSeparation proves a signature made under one domain cannot
// authenticate the other: a workflow blessing signed under the TRUST domain
// fails, and a trust-store signed under the WORKFLOW domain fails.
func TestVerify_DomainSeparation(t *testing.T) {
	t.Run("workflow sig under trust domain is rejected", func(t *testing.T) {
		env := newTestEnv(t)
		wf := env.writeWorkflow(t, workflowData)
		// Sign the workflow with the enrolled key but under the WRONG (trust) domain.
		sidecar := Envelope{
			Schema:    EnvelopeSchema,
			Algo:      AlgoECDSAP256SHA256,
			KeyID:     env.signerID,
			Signature: signB64(t, env.signerKey, DomainTrust, workflowData),
			SignedAt:  fixedTime.Format(time.RFC3339),
		}
		writeFile(t, SidecarPath(wf), mustEncodeEnvelope(t, sidecar))
		if err := env.verifier().Verify(wf, workflowData); !errors.Is(err, ErrTampered) {
			t.Fatalf("Verify = %v, want ErrTampered (wrong-domain workflow sig)", err)
		}
	})

	t.Run("trust-store sig under workflow domain is rejected", func(t *testing.T) {
		env := newTestEnv(t)
		wf := env.writeWorkflow(t, workflowData)
		env.bless(t, wf, workflowData)
		// Re-sign the store under the WRONG (workflow) domain with the anchor key.
		badStoreEnv := Envelope{
			Schema:    EnvelopeSchema,
			Algo:      AlgoECDSAP256SHA256,
			KeyID:     env.anchorFP,
			Signature: signB64(t, env.anchorKey, DomainWorkflow, env.storeBytes),
			SignedAt:  fixedTime.Format(time.RFC3339),
		}
		writeFile(t, SidecarPath(env.storePath), mustEncodeEnvelope(t, badStoreEnv))
		if err := env.verifier().Verify(wf, workflowData); !errors.Is(err, ErrTrustStoreInvalid) {
			t.Fatalf("Verify = %v, want ErrTrustStoreInvalid (wrong-domain store sig)", err)
		}
	})
}

// TestVerify_StoreEntryFingerprintMismatch proves the defense-in-depth check
// on store identity claims: an anchor-signed store whose entry records a key_id
// that does not fingerprint its own pubkey is refused — verification never
// proceeds on a key looked up by a false id, even though only broken enrollment
// tooling (not an agent) could produce such a store.
func TestVerify_StoreEntryFingerprintMismatch(t *testing.T) {
	env := newTestEnv(t)
	wf := env.writeWorkflow(t, workflowData)
	env.bless(t, wf, workflowData)

	// Swap the enrolled entry's pubkey for a different key's, keeping the
	// original key_id, and re-sign the store with the real anchor key.
	store, err := DecodeStore(env.storeBytes)
	if err != nil {
		t.Fatalf("decode store: %v", err)
	}
	imposter := genKey(t)
	for i := range store.Keys {
		if store.Keys[i].KeyID == env.signerID {
			store.Keys[i].Pubkey = base64.StdEncoding.EncodeToString(mustPubDER(t, &imposter.PublicKey))
		}
	}
	badBytes, err := EncodeStore(store)
	if err != nil {
		t.Fatalf("encode store: %v", err)
	}
	badStoreEnv := Envelope{
		Schema:    EnvelopeSchema,
		Algo:      AlgoECDSAP256SHA256,
		KeyID:     env.anchorFP,
		Signature: signB64(t, env.anchorKey, DomainTrust, badBytes),
		SignedAt:  fixedTime.Format(time.RFC3339),
	}
	writeFile(t, env.storePath, badBytes)
	writeFile(t, SidecarPath(env.storePath), mustEncodeEnvelope(t, badStoreEnv))

	err = env.verifier().Verify(wf, workflowData)
	if !errors.Is(err, ErrTrustStoreInvalid) {
		t.Fatalf("Verify fingerprint-mismatch = %v, want ErrTrustStoreInvalid", err)
	}
}

func TestVerify_UsesOnlyProvidedData(t *testing.T) {
	// Verify must never re-read the workflow file — TOCTOU is closed by the
	// caller. Bless a set of bytes, delete the file, and confirm Verify still
	// succeeds against the in-memory bytes it was handed.
	env := newTestEnv(t)
	wf := env.writeWorkflow(t, workflowData)
	env.bless(t, wf, workflowData)
	if err := os.Remove(wf); err != nil {
		t.Fatalf("remove workflow file: %v", err)
	}
	if err := env.verifier().Verify(wf, workflowData); err != nil {
		t.Fatalf("Verify after removing the workflow file: %v", err)
	}
}

// TestAnchor_ReturnsAuthenticatedAnchor proves the factored-out steps 1–2 return
// the installed anchor key and its fingerprint, and that the fingerprint is
// exactly the one TrustedStore roots the store on — so factoring Anchor() out of
// TrustedStore changed nothing observable.
func TestAnchor_ReturnsAuthenticatedAnchor(t *testing.T) {
	env := newTestEnv(t)
	pub, fp, err := env.verifier().Anchor()
	if err != nil {
		t.Fatalf("Anchor: %v", err)
	}
	if fp != env.anchorFP {
		t.Errorf("Anchor fingerprint = %q, want %q", fp, env.anchorFP)
	}
	if !bytes.Equal(mustPubDER(t, pub), mustPubDER(t, &env.anchorKey.PublicKey)) {
		t.Error("Anchor returned a key that is not the installed anchor")
	}
	store, err := env.verifier().TrustedStore()
	if err != nil {
		t.Fatalf("TrustedStore: %v", err)
	}
	if store.AnchorKeyID != fp {
		t.Errorf("TrustedStore anchor_key_id %q != Anchor fingerprint %q", store.AnchorKeyID, fp)
	}
}

func TestAnchor_OwnershipCheckFails(t *testing.T) {
	env := newTestEnv(t)
	v := env.verifier()
	v.anchorCheck = func(string) error { return errors.New("not root-owned") }
	if _, _, err := v.Anchor(); !errors.Is(err, ErrNoAnchor) {
		t.Fatalf("Anchor = %v, want ErrNoAnchor", err)
	}
}

func TestAnchor_MissingFile(t *testing.T) {
	env := newTestEnv(t)
	v := env.verifier()
	v.anchorPath = filepath.Join(env.dir, "does-not-exist")
	if _, _, err := v.Anchor(); !errors.Is(err, ErrNoAnchor) {
		t.Fatalf("Anchor = %v, want ErrNoAnchor", err)
	}
}

// TestAnchor_UncachedReReadsDisk proves Anchor re-reads the anchor from disk on
// every call: after corrupting the file a second call must fail, where a cached
// read would still succeed. TrustedStore depends on this uncached read.
func TestAnchor_UncachedReReadsDisk(t *testing.T) {
	env := newTestEnv(t)
	v := env.verifier()
	if _, _, err := v.Anchor(); err != nil {
		t.Fatalf("first Anchor: %v", err)
	}
	writeFile(t, env.anchorPath, []byte("not base64 !!!"))
	if _, _, err := v.Anchor(); !errors.Is(err, ErrNoAnchor) {
		t.Fatalf("second Anchor after corruption = %v, want ErrNoAnchor", err)
	}
}

// TestTrustedStore_SingleMachineAnchorIsSigner proves a rebuild-SHAPED store — a
// single key that is BOTH the anchor and the sole enrolled blesser, signed under
// the trust domain by that key — verifies through the real TrustedStore path.
// `trust rebuild` produces exactly this shape (anchor == the only enrolled key),
// so this is the ground-truth that a rebuilt store re-verifies.
func TestTrustedStore_SingleMachineAnchorIsSigner(t *testing.T) {
	dir := t.TempDir()
	key := genKey(t)
	der := mustPubDER(t, &key.PublicKey)
	fp := Fingerprint(der)

	anchorPath := filepath.Join(dir, "trust-anchor.pub")
	writeFile(t, anchorPath, []byte(base64.StdEncoding.EncodeToString(der)+"\n"))

	store := Store{
		Schema:      StoreSchema,
		AnchorKeyID: fp,
		Keys: []TrustedKey{{
			KeyID:   fp,
			Machine: "test",
			Pubkey:  base64.StdEncoding.EncodeToString(der),
			AddedAt: fixedTime.Format(time.RFC3339),
		}},
	}
	storeBytes := mustEncodeStore(t, store)
	storePath := filepath.Join(dir, "trust.toml")
	writeFile(t, storePath, storeBytes)
	storeEnv := Envelope{
		Schema:    EnvelopeSchema,
		Algo:      AlgoECDSAP256SHA256,
		KeyID:     fp,
		Signature: signB64(t, key, DomainTrust, storeBytes),
		SignedAt:  fixedTime.Format(time.RFC3339),
	}
	writeFile(t, SidecarPath(storePath), mustEncodeEnvelope(t, storeEnv))

	v := &Verifier{
		anchorPath:     anchorPath,
		anchorCheck:    func(string) error { return nil },
		trustStorePath: func() (string, error) { return storePath, nil },
	}
	got, err := v.TrustedStore()
	if err != nil {
		t.Fatalf("rebuild-shaped store must verify via TrustedStore: %v", err)
	}
	if got.AnchorKeyID != fp || len(got.Keys) != 1 || got.Keys[0].KeyID != fp {
		t.Errorf("TrustedStore = %+v, want single enrolled key %s == anchor", got, fp)
	}
}

func TestNewVerifier_AnchorPathCompiledIn(t *testing.T) {
	const want = "/etc/forgectl/trust-anchor.pub"
	if AnchorPath != want {
		t.Errorf("AnchorPath = %q, want %q", AnchorPath, want)
	}
	if got := NewVerifier().anchorPath; got != want {
		t.Errorf("NewVerifier().anchorPath = %q, want %q", got, want)
	}
}
