package cli

// Trust-store field echoes (#778 item 3). The store is anchor-signed, but
// DecodeStore never validates its key_id, machine or added_at, so each is
// capped and escaped wherever `trust rebuild` or `trust list` prints it.
//
//   [x] rebuild's peer refusal caps and escapes the peer's key_id and machine
//   [x] rebuild's unverifiable-store note escapes and caps the store error
//   [x] trust list caps and escapes key_id, machine and added_at

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/bless"
	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/module"
)

// plantedField is a store value built to drive a terminal and flood a line.
var plantedField = "\x1b]0;pwned\x07" + strings.Repeat("F", 300)

func assertInertAndCapped(t *testing.T, what, text string) {
	t.Helper()
	if strings.ContainsAny(text, "\x1b\x07") {
		t.Errorf("%s carries a raw control: %q", what, text)
	}
	if strings.Contains(text, strings.Repeat("F", 100)) {
		t.Errorf("%s echoes a store field uncapped: %q", what, text)
	}
}

func TestWorkflowTrustRebuild_PeerRefusalCapsStoreFields(t *testing.T) {
	cliRedirectConfigDir(t)
	key := cliGenKey(t)
	pubDER := cliPubDER(t, key)
	keyID := bless.Fingerprint(pubDER)
	store := bless.Store{
		Schema:      bless.StoreSchema,
		AnchorKeyID: keyID,
		Keys: []bless.TrustedKey{
			{KeyID: keyID, Machine: "self", Pubkey: base64.StdEncoding.EncodeToString(pubDER)},
			{KeyID: plantedField, Machine: plantedField},
		},
	}
	swapTrustReader(t, fakeTrustReader{anchorFP: keyID, store: store})
	installBlesser(t, &rebuildSpyBlesser{t: t, key: key, pubDER: pubDER,
		signErr: fmt.Errorf("sign must not be reached when a peer would be dropped")})
	spyAnchorInstall(t, nil)

	cmd := newWorkflowTrustRebuildCmd(module.Deps{Runner: &exec.FakeRunner{}})
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs(nil)
	err := cmd.ExecuteContext(context.Background())
	if err == nil || !strings.Contains(err.Error(), "also enrolls") {
		t.Fatalf("rebuild = %v, want the peer refusal", err)
	}
	assertInertAndCapped(t, "the peer refusal", err.Error())
}

func TestWorkflowTrustRebuild_UnverifiableNoteEscapesTheStoreError(t *testing.T) {
	cliRedirectConfigDir(t)
	key := cliGenKey(t)
	pubDER := cliPubDER(t, key)
	keyID := bless.Fingerprint(pubDER)
	storeErr := fmt.Errorf("%w: %s", bless.ErrTrustStoreInvalid, plantedField)
	swapTrustReader(t, fakeTrustReader{anchorFP: keyID, storeErr: storeErr})
	installBlesser(t, &rebuildSpyBlesser{t: t, key: key, pubDER: pubDER})
	spyAnchorInstall(t, nil)

	cmd := newWorkflowTrustRebuildCmd(module.Deps{Runner: &exec.FakeRunner{}})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs(nil)
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("rebuild over an unverifiable store must still recover: %v", err)
	}
	if !strings.Contains(out.String(), "could not be verified") {
		t.Fatalf("want the unverifiable-store note, got %q", out.String())
	}
	// 300 is the cap: the note must hold fewer F's than the planted field.
	if strings.Contains(out.String(), strings.Repeat("F", 300)) {
		t.Errorf("the note echoes the store error uncapped: %q", out.String())
	}
	if strings.ContainsAny(out.String(), "\x1b\x07") {
		t.Errorf("the note carries a raw control: %q", out.String())
	}
}

func TestWorkflowTrustList_CapsStoreFields(t *testing.T) {
	store := bless.Store{
		Schema:      bless.StoreSchema,
		AnchorKeyID: "sha256:anchor",
		Keys:        []bless.TrustedKey{{KeyID: plantedField, Machine: plantedField, AddedAt: plantedField}},
	}
	swapTrustReader(t, fakeTrustReader{store: store})
	cmd := newWorkflowTrustListCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs(nil)
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("trust list: %v", err)
	}
	assertInertAndCapped(t, "trust list", out.String())
	if got := strings.Count(out.String(), "[truncated]"); got != 3 {
		t.Errorf("want all three fields marked truncated, got %d in %q", got, out.String())
	}
}
