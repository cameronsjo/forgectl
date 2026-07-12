package bless

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
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

// runStep / stripStep / launchStep / worktreeStep build StepChecks shaped like
// the ones the CLI maps out of the real registry — the guarded-field sets are
// the registry's (run: Cmd+Args, strip: Globs, launch: Skill/Mode/Posture,
// worktree: none), so this table stays honest about which fields are scanned.
func runStep(cmd string, args ...string) StepCheck {
	return StepCheck{Uses: "run", Guarded: map[string][]string{"Cmd": {cmd}, "Args": args}}
}

func stripStep(globs ...string) StepCheck {
	return StepCheck{Uses: "strip", Guarded: map[string][]string{"Globs": globs}}
}

func launchStep(skill, mode, posture string) StepCheck {
	return StepCheck{
		Uses:    "launch",
		Exports: []string{"review"},
		Guarded: map[string][]string{"Skill": {skill}, "Mode": {mode}, "Posture": {posture}},
	}
}

// worktreeStep has NO guarded fields — repo/ref merely name data, so a ${param}
// there is the intended parameterization, not an injection.
func worktreeStep() StepCheck {
	return StepCheck{Uses: "worktree", Exports: []string{"workspace"}}
}

func TestCheckGuardedParamRefs(t *testing.T) {
	tests := []struct {
		name    string
		steps   []StepCheck
		params  []string
		wantErr bool
	}{
		{
			name:    "run step with no refs is fine",
			steps:   []StepCheck{runStep("make", "build")},
			wantErr: false,
		},
		{
			name: "param colliding with an export name is refused",
			steps: []StepCheck{
				worktreeStep(),
				runStep("make", "-C", "${workspace}"),
			},
			params:  []string{"workspace"},
			wantErr: true,
		},
		{
			name: "param colliding with a LATER step's export is still refused",
			steps: []StepCheck{
				runStep("make"),
				launchStep("code-review", "sync", ""),
			},
			params:  []string{"review"},
			wantErr: true,
		},
		{
			name: "non-colliding params are fine",
			steps: []StepCheck{
				worktreeStep(),
				runStep("make", "-C", "${workspace}"),
			},
			params:  []string{"repo", "branch"},
			wantErr: false,
		},
		{
			name:    "run cmd references a param",
			steps:   []StepCheck{runStep("${evil}")},
			wantErr: true,
		},
		{
			name:    "run arg references a param",
			steps:   []StepCheck{runStep("make", "-C", "${evil}")},
			wantErr: true,
		},
		{
			name: "run step may reference an earlier step's export",
			steps: []StepCheck{
				worktreeStep(),
				runStep("make", "-C", "${workspace}"),
			},
			wantErr: false,
		},
		{
			name: "run step may not reference a later step's export",
			steps: []StepCheck{
				runStep("make", "-C", "${workspace}"),
				worktreeStep(),
			},
			wantErr: true,
		},
		{
			name: "run step may not reference its own export",
			steps: []StepCheck{
				{Uses: "run", Exports: []string{"self"}, Guarded: map[string][]string{"Cmd": {"echo"}, "Args": {"${self}"}}},
			},
			wantErr: true,
		},
		{
			name:    "unterminated ${ is refused",
			steps:   []StepCheck{runStep("echo ${oops")},
			wantErr: true,
		},
		{
			name: "multiple earlier exports all allowed",
			steps: []StepCheck{
				worktreeStep(),
				launchStep("code-review", "sync", ""),
				runStep("process", "${workspace}", "${review}"),
			},
			wantErr: false,
		},

		// strip.globs — the clean-room redaction list IS a security control, so a
		// param in it would let an agent narrow the strip-list at run time.
		{
			name:    "strip globs reference a param",
			steps:   []StepCheck{worktreeStep(), stripStep("CLAUDE.md", "${sneaky}")},
			params:  []string{"sneaky"},
			wantErr: true,
		},
		{
			name:    "strip globs may reference an earlier step's export",
			steps:   []StepCheck{worktreeStep(), stripStep("${workspace}/CLAUDE.md")},
			wantErr: false,
		},
		{
			name:    "strip globs with no refs are fine",
			steps:   []StepCheck{worktreeStep(), stripStep("CLAUDE.md", ".claude/")},
			wantErr: false,
		},
		{
			name:    "unterminated ${ in strip globs is refused",
			steps:   []StepCheck{stripStep("${oops")},
			wantErr: true,
		},

		// launch.skill / mode / posture steer what the launched agent DOES.
		{
			name:    "launch skill references a param",
			steps:   []StepCheck{launchStep("${skill}", "sync", "")},
			params:  []string{"skill"},
			wantErr: true,
		},
		{
			name:    "launch mode references a param",
			steps:   []StepCheck{launchStep("code-review", "${mode}", "")},
			params:  []string{"mode"},
			wantErr: true,
		},
		{
			name:    "launch posture references a param",
			steps:   []StepCheck{launchStep("code-review", "sync", "${posture}")},
			params:  []string{"posture"},
			wantErr: true,
		},
		{
			name:    "launch fields may reference an earlier step's export",
			steps:   []StepCheck{worktreeStep(), launchStep("${workspace}", "sync", "")},
			wantErr: false,
		},
		{
			name:    "launch fields with no refs are fine",
			steps:   []StepCheck{launchStep("code-review", "sync", "opus")},
			wantErr: false,
		},

		// The counterweight: params MUST keep flowing into non-guarded data
		// fields. `workflow run clean-room-review --param repo=owner/x` is the
		// feature — do not "harden" this away.
		{
			name:    "param in a worktree repo/ref is the intended parameterization",
			steps:   []StepCheck{worktreeStep(), stripStep("CLAUDE.md"), runStep("make", "-C", "${workspace}")},
			params:  []string{"repo", "branch"},
			wantErr: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckGuardedParamRefs(tc.steps, tc.params)
			if tc.wantErr && err == nil {
				t.Fatal("expected an error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

// TestCheckGuardedParamRefs_ErrorNamesFieldAndRef pins the error text: an author
// staring at a refusal needs the step index, the FIELD, and the offending ref.
func TestCheckGuardedParamRefs_ErrorNamesFieldAndRef(t *testing.T) {
	err := CheckGuardedParamRefs([]StepCheck{
		worktreeStep(),
		stripStep("CLAUDE.md", "${sneaky}"),
	}, []string{"sneaky"})
	if err == nil {
		t.Fatal("expected a refusal")
	}
	for _, want := range []string{"step 1", "strip", "Globs", "sneaky"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %q", err, want)
		}
	}
}
