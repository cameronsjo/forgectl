package bless

import (
	"context"
	"encoding/base64"
	"errors"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
)

func TestEnsureKey_EnrollHappyPath(t *testing.T) {
	key := genKey(t)
	der := mustPubDER(t, &key.PublicKey)
	fake := &exec.FakeRunner{RunFunc: func(_ string, _ []string) (string, error) {
		return `{"pubkey":"` + base64.StdEncoding.EncodeToString(der) + `"}`, nil
	}}
	hb := fakeHelper(t, fake)

	got, err := EnsureKey(context.Background(), hb, "l")
	if err != nil {
		t.Fatalf("EnsureKey: %v", err)
	}
	if string(got) != string(der) {
		t.Error("EnsureKey returned unexpected DER")
	}
	if fake.Last().Args[0] != "enroll" {
		t.Errorf("expected enroll on the happy path, got %v", fake.Last().Args)
	}
}

func TestEnsureKey_LabelExistsFallsBackToPublicKey(t *testing.T) {
	key := genKey(t)
	der := mustPubDER(t, &key.PublicKey)
	fake := &exec.FakeRunner{RunFunc: func(_ string, args []string) (string, error) {
		switch args[0] {
		case "enroll":
			return "", exitErr{3} // label exists
		case "pubkey":
			return `{"pubkey":"` + base64.StdEncoding.EncodeToString(der) + `"}`, nil
		}
		return "", nil
	}}
	hb := fakeHelper(t, fake)

	got, err := EnsureKey(context.Background(), hb, "l")
	if err != nil {
		t.Fatalf("EnsureKey: %v", err)
	}
	if string(got) != string(der) {
		t.Error("EnsureKey fallback returned unexpected DER")
	}
	if fake.Last().Args[0] != "pubkey" {
		t.Errorf("expected a pubkey fallback call, got %v", fake.Last().Args)
	}
}

func TestEnsureKey_PropagatesOtherErrors(t *testing.T) {
	fake := &exec.FakeRunner{RunFunc: func(_ string, _ []string) (string, error) {
		return "", exitErr{2} // cancelled
	}}
	hb := fakeHelper(t, fake)
	if _, err := EnsureKey(context.Background(), hb, "l"); !errors.Is(err, ErrCancelled) {
		t.Fatalf("EnsureKey = %v, want ErrCancelled propagated", err)
	}
}

func TestSignEnvelope_AssemblesAndRoundTripsThroughVerify(t *testing.T) {
	env := newTestEnv(t)
	wf := env.writeWorkflow(t, workflowData)

	// The helper signs the workflow-domain digest of the data with the enrolled
	// key — a REAL signature, so the assembled envelope verifies end to end.
	fake := &exec.FakeRunner{RunFunc: func(_ string, _ []string) (string, error) {
		d := TaggedDigest(DomainWorkflow, workflowData)
		sig, err := signASN1(env.signerKey, d[:])
		if err != nil {
			return "", err
		}
		return `{"signature":"` + base64.StdEncoding.EncodeToString(sig) + `"}`, nil
	}}
	hb := fakeHelper(t, fake)

	envlp, err := SignEnvelope(context.Background(), hb, "l", env.signerID, DomainWorkflow, workflowData, fixedTime)
	if err != nil {
		t.Fatalf("SignEnvelope: %v", err)
	}
	if envlp.Schema != EnvelopeSchema || envlp.Algo != AlgoECDSAP256SHA256 || envlp.KeyID != env.signerID {
		t.Fatalf("envelope fields wrong: %+v", envlp)
	}
	if envlp.SignedAt != fixedTime.UTC().Format("2006-01-02T15:04:05Z07:00") {
		t.Errorf("signed_at = %q", envlp.SignedAt)
	}
	// stdin carried the base64 digest + newline.
	wantInput := base64.StdEncoding.EncodeToString(func() []byte { d := TaggedDigest(DomainWorkflow, workflowData); return d[:] }()) + "\n"
	if fake.Last().Input != wantInput {
		t.Errorf("Sign stdin = %q, want %q", fake.Last().Input, wantInput)
	}

	writeFile(t, SidecarPath(wf), mustEncodeEnvelope(t, envlp))
	if err := env.verifier().Verify(wf, workflowData); err != nil {
		t.Fatalf("Verify of SignEnvelope output: %v", err)
	}
}

func TestInstallAnchor_TwoSudoLegs(t *testing.T) {
	fake := &exec.FakeRunner{}
	key := genKey(t)
	pubDER := mustPubDER(t, &key.PublicKey)

	if err := InstallAnchor(context.Background(), fake, pubDER); err != nil {
		t.Fatalf("InstallAnchor: %v", err)
	}
	if len(fake.Calls) != 2 {
		t.Fatalf("expected 2 sudo legs, got %d: %+v", len(fake.Calls), fake.Calls)
	}
	for i, c := range fake.Calls {
		if !c.Interactive {
			t.Errorf("leg %d must be Interactive", i)
		}
		if c.Name != "sudo" {
			t.Errorf("leg %d name = %q, want sudo", i, c.Name)
		}
	}
	// Leg 1: install -d ... /etc/forgectl.
	mkdir := fake.Calls[0].Args
	wantMkdir := []string{"install", "-d", "-o", "root", "-g", "wheel", "-m", "0755", "/etc/forgectl"}
	if !equalStrings(mkdir, wantMkdir) {
		t.Errorf("mkdir leg argv = %v, want %v", mkdir, wantMkdir)
	}
	// Leg 2: install ... <tmp> /etc/forgectl/trust-anchor.pub.
	place := fake.Calls[1].Args
	if len(place) < 2 || place[len(place)-1] != AnchorPath {
		t.Errorf("place leg must end at %q, got %v", AnchorPath, place)
	}
	wantPrefix := []string{"install", "-o", "root", "-g", "wheel", "-m", "0644"}
	if !equalStrings(place[:len(wantPrefix)], wantPrefix) {
		t.Errorf("place leg prefix = %v, want %v", place[:len(wantPrefix)], wantPrefix)
	}
}

func TestCheckRunStepParamRefs(t *testing.T) {
	tests := []struct {
		name    string
		steps   []StepCheck
		params  []string
		wantErr bool
	}{
		{
			name:    "run step with no refs is fine",
			steps:   []StepCheck{{Uses: "run", Cmd: "make", Args: []string{"build"}}},
			wantErr: false,
		},
		{
			name: "param colliding with an export name is refused",
			steps: []StepCheck{
				{Uses: "worktree", Exports: []string{"workspace"}},
				{Uses: "run", Cmd: "make", Args: []string{"-C", "${workspace}"}},
			},
			params:  []string{"workspace"},
			wantErr: true,
		},
		{
			name: "param colliding with a LATER step's export is still refused",
			steps: []StepCheck{
				{Uses: "run", Cmd: "make"},
				{Uses: "launch", Exports: []string{"review"}},
			},
			params:  []string{"review"},
			wantErr: true,
		},
		{
			name: "non-colliding params are fine",
			steps: []StepCheck{
				{Uses: "worktree", Exports: []string{"workspace"}},
				{Uses: "run", Cmd: "make", Args: []string{"-C", "${workspace}"}},
			},
			params:  []string{"repo", "branch"},
			wantErr: false,
		},
		{
			name:    "run cmd references a param",
			steps:   []StepCheck{{Uses: "run", Cmd: "${evil}"}},
			wantErr: true,
		},
		{
			name:    "run arg references a param",
			steps:   []StepCheck{{Uses: "run", Cmd: "make", Args: []string{"-C", "${evil}"}}},
			wantErr: true,
		},
		{
			name: "run step may reference an earlier step's export",
			steps: []StepCheck{
				{Uses: "worktree", Exports: []string{"workspace"}},
				{Uses: "run", Cmd: "make", Args: []string{"-C", "${workspace}"}},
			},
			wantErr: false,
		},
		{
			name: "run step may not reference a later step's export",
			steps: []StepCheck{
				{Uses: "run", Cmd: "make", Args: []string{"-C", "${workspace}"}},
				{Uses: "worktree", Exports: []string{"workspace"}},
			},
			wantErr: true,
		},
		{
			name: "run step may not reference its own export",
			steps: []StepCheck{
				{Uses: "run", Cmd: "echo", Args: []string{"${self}"}, Exports: []string{"self"}},
			},
			wantErr: true,
		},
		{
			name:    "unterminated ${ is refused",
			steps:   []StepCheck{{Uses: "run", Cmd: "echo ${oops"}},
			wantErr: true,
		},
		{
			name: "non-run step's fields are not scanned",
			steps: []StepCheck{
				{Uses: "worktree", Cmd: "${repo}", Args: []string{"${branch}"}},
			},
			wantErr: false,
		},
		{
			name: "multiple earlier exports all allowed",
			steps: []StepCheck{
				{Uses: "worktree", Exports: []string{"workspace"}},
				{Uses: "launch", Exports: []string{"review"}},
				{Uses: "run", Cmd: "process", Args: []string{"${workspace}", "${review}"}},
			},
			wantErr: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckRunStepParamRefs(tc.steps, tc.params)
			if tc.wantErr && err == nil {
				t.Fatal("expected an error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}
