package bless

import (
	"context"
	"encoding/base64"
	"errors"
	"path/filepath"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
)

// fakeHelper points FORGECTL_BLESS_HELPER at a real (dummy) on-disk file so
// NewHelperBlesser's existence check passes, and binds a FakeRunner so no real
// process runs.
func fakeHelper(t *testing.T, run exec.Runner) *HelperBlesser {
	t.Helper()
	p := filepath.Join(t.TempDir(), "forgectl-bless-helper")
	writeFile(t, p, []byte("#!/bin/sh\n"))
	t.Setenv("FORGECTL_BLESS_HELPER", p)
	hb, err := NewHelperBlesser(run)
	if err != nil {
		t.Fatalf("NewHelperBlesser: %v", err)
	}
	if hb.path != p {
		t.Fatalf("helper path = %q, want %q", hb.path, p)
	}
	return hb
}

func TestNewHelperBlesser_MissingHelper(t *testing.T) {
	t.Setenv("FORGECTL_BLESS_HELPER", filepath.Join(t.TempDir(), "absent"))
	if _, err := NewHelperBlesser(&exec.FakeRunner{}); !errors.Is(err, ErrNoBlesser) {
		t.Fatalf("NewHelperBlesser = %v, want ErrNoBlesser", err)
	}
}

func TestHelperBlesser_SignContract(t *testing.T) {
	signer := genKey(t)
	var digest [32]byte
	for i := range digest {
		digest[i] = byte(i)
	}

	fake := &exec.FakeRunner{
		RunFunc: func(_ string, _ []string) (string, error) {
			sig, err := signASN1(signer, digest[:])
			if err != nil {
				return "", err
			}
			return `{"signature":"` + base64.StdEncoding.EncodeToString(sig) + `"}`, nil
		},
	}
	hb := fakeHelper(t, fake)

	der, err := hb.Sign(context.Background(), "mylabel", digest)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}

	call := fake.Last()
	wantArgs := []string{"sign", "--label", "mylabel"}
	if !equalStrings(call.Args, wantArgs) {
		t.Errorf("Sign argv = %v, want %v", call.Args, wantArgs)
	}
	// stdin must be the base64 of exactly the 32 digest bytes plus a newline.
	wantInput := base64.StdEncoding.EncodeToString(digest[:]) + "\n"
	if call.Input != wantInput {
		t.Errorf("Sign stdin = %q, want %q", call.Input, wantInput)
	}
	// The returned DER must be a real signature over the digest.
	if !verifyASN1(&signer.PublicKey, digest[:], der) {
		t.Error("returned signature does not verify over the digest")
	}
}

func TestHelperBlesser_PubkeyContract(t *testing.T) {
	key := genKey(t)
	der := mustPubDER(t, &key.PublicKey)
	fake := &exec.FakeRunner{
		RunFunc: func(_ string, _ []string) (string, error) {
			return `{"pubkey":"` + base64.StdEncoding.EncodeToString(der) + `"}`, nil
		},
	}
	hb := fakeHelper(t, fake)

	for _, verb := range []string{"enroll", "pubkey"} {
		var (
			got []byte
			err error
		)
		if verb == "enroll" {
			got, err = hb.Enroll(context.Background(), "l")
		} else {
			got, err = hb.PublicKey(context.Background(), "l")
		}
		if err != nil {
			t.Fatalf("%s: %v", verb, err)
		}
		if string(got) != string(der) {
			t.Errorf("%s returned DER mismatch", verb)
		}
		call := fake.Last()
		if !equalStrings(call.Args, []string{verb, "--label", "l"}) {
			t.Errorf("%s argv = %v", verb, call.Args)
		}
	}
}

func TestHelperBlesser_RejectsBadReplies(t *testing.T) {
	tests := []struct {
		name  string
		reply string
	}{
		{"empty pubkey", `{"pubkey":""}`},
		{"missing field", `{}`},
		{"unknown field", `{"pubkey":"AAAA","extra":1}`},
		{"pubkey not base64", `{"pubkey":"!!!"}`},
		{"not json", `garbage`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fake := &exec.FakeRunner{RunFunc: func(_ string, _ []string) (string, error) { return tc.reply, nil }}
			hb := fakeHelper(t, fake)
			if _, err := hb.Enroll(context.Background(), "l"); err == nil {
				t.Fatalf("expected an error for reply %q", tc.reply)
			}
		})
	}
}

func TestHelperBlesser_ExitCodeMapping(t *testing.T) {
	tests := []struct {
		code int
		want error
	}{
		{2, ErrCancelled},
		{3, ErrLabelExists},
		{4, ErrKeyNotFound},
		{5, ErrBadDigest},
	}
	for _, tc := range tests {
		fake := &exec.FakeRunner{RunFunc: func(_ string, _ []string) (string, error) {
			return "", exitErr{tc.code}
		}}
		hb := fakeHelper(t, fake)
		if _, err := hb.Enroll(context.Background(), "l"); !errors.Is(err, tc.want) {
			t.Errorf("exit %d mapped to %v, want %v", tc.code, err, tc.want)
		}
	}

	// A usage/unknown exit (1) and a non-exit error are not mapped to a ceremony
	// sentinel.
	fake := &exec.FakeRunner{RunFunc: func(_ string, _ []string) (string, error) { return "", exitErr{1} }}
	hb := fakeHelper(t, fake)
	_, err := hb.Enroll(context.Background(), "l")
	if err == nil {
		t.Fatal("expected an error for exit 1")
	}
	for _, s := range []error{ErrCancelled, ErrLabelExists, ErrKeyNotFound, ErrBadDigest} {
		if errors.Is(err, s) {
			t.Errorf("exit 1 must not map to %v", s)
		}
	}
}
