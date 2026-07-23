package bless

import (
	"context"
	"encoding/base64"
	"errors"
	"path/filepath"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
)

// stubSelf points the executable-resolution seam at path for the test's
// lifetime. It is how a test controls where the helper's SIBLING lookup lands —
// there is no environment override, by design (see
// TestNewHelperBlesser_EnvVarCannotRedirectHelper).
func stubSelf(t *testing.T, path string) {
	t.Helper()
	prev := resolveSelf
	resolveSelf = func() (string, error) { return path, nil }
	t.Cleanup(func() { resolveSelf = prev })
}

// siblingHelper writes a dummy helper into a fresh temp dir, aims the seam at a
// fake forgectl binary in that same dir, and returns the helper's path.
func siblingHelper(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, helperName)
	writeFile(t, p, []byte("#!/bin/sh\n"))
	stubSelf(t, filepath.Join(dir, "forgectl"))
	return p
}

// fakeHelper puts a real (dummy) helper next to the (stubbed) running executable
// so NewHelperBlesser's existence check passes, and binds a FakeRunner so no
// real process runs.
func fakeHelper(t *testing.T, run exec.Runner) *HelperBlesser {
	t.Helper()
	p := siblingHelper(t)
	hb, err := NewHelperBlesser(context.Background(), run)
	if err != nil {
		t.Fatalf("NewHelperBlesser: %v", err)
	}
	if hb.path != p {
		t.Fatalf("helper path = %q, want %q", hb.path, p)
	}
	return hb
}

func TestNewHelperBlesser_MissingHelper(t *testing.T) {
	// A temp dir with NO helper beside the executable.
	stubSelf(t, filepath.Join(t.TempDir(), "forgectl"))
	if _, err := NewHelperBlesser(context.Background(), &exec.FakeRunner{}); !errors.Is(err, ErrNoBlesser) {
		t.Fatalf("NewHelperBlesser = %v, want ErrNoBlesser", err)
	}
}

// TestNewHelperBlesser_EnvVarCannotRedirectHelper is the regression pin for the
// removed FORGECTL_BLESS_HELPER override — a root-of-trust vulnerability, not a
// dev convenience.
//
// The helper serves Enroll and PublicKey, not only Sign. `trust init` calls
// Enroll when NO trust store exists yet, so the key the helper returns BECOMES
// the anchor. An env var is fully within a same-UID agent's reach (one appended
// line in a shell rc file), so an agent-settable helper path let the agent
// supply the key that the human's own sudo then installed as the root of trust —
// no Touch ID prompt required, since a fake helper just prints a key.
//
// Discovery must therefore be the executable's sibling and nothing else. If this
// test ever fails, the override is back and blessing is defeated.
func TestNewHelperBlesser_EnvVarCannotRedirectHelper(t *testing.T) {
	// The attacker's helper, named by the env var that used to win.
	decoy := filepath.Join(t.TempDir(), helperName)
	writeFile(t, decoy, []byte("#!/bin/sh\necho '{\"pubkey\":\"attacker\"}'\n"))
	t.Setenv("FORGECTL_BLESS_HELPER", decoy)

	// The real helper, beside the running binary.
	want := siblingHelper(t)

	hb, err := NewHelperBlesser(context.Background(), &exec.FakeRunner{})
	if err != nil {
		t.Fatalf("NewHelperBlesser: %v", err)
	}
	if hb.path == decoy {
		t.Fatal("FORGECTL_BLESS_HELPER redirected helper discovery — the trust bootstrap is compromised")
	}
	if hb.path != want {
		t.Fatalf("helper path = %q, want the executable's sibling %q", hb.path, want)
	}
}

// setExpectedTeamID pins the Developer-ID trust gate to id for the test's
// lifetime and restores the prior value after. An empty id keeps the gate dormant
// — the dev/source-build default.
func setExpectedTeamID(t *testing.T, id string) {
	t.Helper()
	prev := ExpectedTeamID
	ExpectedTeamID = id
	t.Cleanup(func() { ExpectedTeamID = prev })
}

// TestNewHelperBlesser_GateDormantWhenTeamIDEmpty pins the no-op contract: with
// ExpectedTeamID empty (dev/source builds), constructing a HelperBlesser must NOT
// shell out to codesign at all. The gate is a genuine absence, not a verify that
// happens to pass.
func TestNewHelperBlesser_GateDormantWhenTeamIDEmpty(t *testing.T) {
	siblingHelper(t)
	fake := &exec.FakeRunner{}
	if _, err := NewHelperBlesser(context.Background(), fake); err != nil {
		t.Fatalf("NewHelperBlesser: %v", err)
	}
	if len(fake.Calls) != 0 {
		t.Fatalf("gate ran %d command(s) with ExpectedTeamID empty, want 0: %+v", len(fake.Calls), fake.Calls)
	}
}

// TestNewHelperBlesser_GateVerifiesWhenArmed pins the fail-closed contract: with a
// Team ID set, construction issues `codesign --verify --strict` against a
// requirement pinned to that Team ID, and a codesign failure surfaces as
// ErrHelperUntrusted — the helper exists but is not the release-signed binary.
func TestNewHelperBlesser_GateVerifiesWhenArmed(t *testing.T) {
	setExpectedTeamID(t, "TEAMTEST")
	p := siblingHelper(t)
	fake := &exec.FakeRunner{
		RunFunc: func(_ string, _ []string) (string, error) { return "", exitErr{1} },
	}

	_, err := NewHelperBlesser(context.Background(), fake)
	if !errors.Is(err, ErrHelperUntrusted) {
		t.Fatalf("NewHelperBlesser = %v, want ErrHelperUntrusted", err)
	}

	call := fake.Last()
	if call.Name != "/usr/bin/codesign" {
		t.Fatalf("gate ran %q, want /usr/bin/codesign", call.Name)
	}
	wantReq := `anchor apple generic and certificate leaf[subject.OU] = "TEAMTEST"`
	wantArgs := []string{"--verify", "--strict", "-R", wantReq, p}
	if !equalStrings(call.Args, wantArgs) {
		t.Fatalf("gate argv = %v, want %v", call.Args, wantArgs)
	}
}

// TestNewHelperBlesser_GateAcceptsSignedHelper pins the pass path: an armed gate
// whose codesign check succeeds returns a usable blesser and no error.
func TestNewHelperBlesser_GateAcceptsSignedHelper(t *testing.T) {
	setExpectedTeamID(t, "TEAMTEST")
	p := siblingHelper(t)
	fake := &exec.FakeRunner{
		RunFunc: func(_ string, _ []string) (string, error) { return "", nil },
	}

	hb, err := NewHelperBlesser(context.Background(), fake)
	if err != nil {
		t.Fatalf("NewHelperBlesser: %v", err)
	}
	if hb.path != p {
		t.Fatalf("helper path = %q, want %q", hb.path, p)
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
		{6, ErrKeyNotPresenceGated},
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
