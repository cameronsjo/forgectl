package bless

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestEncodeDecodeStore_RoundTrip(t *testing.T) {
	key := genKey(t)
	anchor := genKey(t)
	s := Store{
		Schema:      StoreSchema,
		AnchorKeyID: Fingerprint(mustPubDER(t, &anchor.PublicKey)),
		Keys: []TrustedKey{{
			KeyID:   Fingerprint(mustPubDER(t, &key.PublicKey)),
			Machine: "sjomba",
			Pubkey:  "AAAA",
			AddedAt: fixedTime.Format(time.RFC3339),
		}},
	}
	encoded := mustEncodeStore(t, s)
	got, err := DecodeStore(encoded)
	if err != nil {
		t.Fatalf("DecodeStore: %v", err)
	}
	if got.Schema != s.Schema || got.AnchorKeyID != s.AnchorKeyID || len(got.Keys) != 1 {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	if got.Keys[0] != s.Keys[0] {
		t.Errorf("key round-trip mismatch:\n got %+v\nwant %+v", got.Keys[0], s.Keys[0])
	}
}

func TestDecodeStore_Rejections(t *testing.T) {
	tests := []struct {
		name string
		toml string
	}{
		{"unknown key", "schema = 1\nanchor_key_id = \"x\"\nbogus = 1\n"},
		{"wrong schema", "schema = 2\nanchor_key_id = \"x\"\n"},
		{"unknown key in [[key]]", "schema = 1\nanchor_key_id = \"x\"\n\n[[key]]\nkey_id = \"a\"\nnope = true\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := DecodeStore([]byte(tc.toml)); err == nil {
				t.Fatalf("expected an error for %s", tc.name)
			}
		})
	}
}

func TestStore_Lookup(t *testing.T) {
	s := Store{Keys: []TrustedKey{{KeyID: "sha256:aaa"}, {KeyID: "sha256:bbb"}}}
	if k, ok := s.Lookup("sha256:bbb"); !ok || k.KeyID != "sha256:bbb" {
		t.Errorf("Lookup existing = %+v, %v", k, ok)
	}
	if _, ok := s.Lookup("sha256:zzz"); ok {
		t.Error("Lookup should miss an absent key")
	}
}

func TestCheckAnchorOwnership(t *testing.T) {
	t.Run("missing file", func(t *testing.T) {
		if err := checkAnchorOwnership(filepath.Join(t.TempDir(), "nope")); err == nil {
			t.Fatal("expected an error for a missing anchor")
		}
	})
	t.Run("directory is not a regular file", func(t *testing.T) {
		if err := checkAnchorOwnership(t.TempDir()); err == nil {
			t.Fatal("expected an error for a directory")
		}
	})
	t.Run("group/world-writable", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "anchor")
		writeFile(t, p, []byte("x"))
		if err := os.Chmod(p, 0o666); err != nil {
			t.Fatalf("chmod: %v", err)
		}
		if err := checkAnchorOwnership(p); err == nil {
			t.Fatal("expected an error for a world-writable anchor")
		}
	})
	t.Run("non-root owner", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("running as root; the non-root-owner path is unreachable")
		}
		p := filepath.Join(t.TempDir(), "anchor")
		writeFile(t, p, []byte("x"))
		if err := os.Chmod(p, 0o644); err != nil {
			t.Fatalf("chmod: %v", err)
		}
		// Owned by the (non-root) test user, mode 0644 — passes regular + writable
		// checks, fails the uid-0 requirement.
		if err := checkAnchorOwnership(p); err == nil {
			t.Fatal("expected an error for an anchor not owned by root")
		}
	})
}
