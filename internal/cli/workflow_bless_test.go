package cli

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/bless"
	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/step"
)

// blessRunCheck is a `run` StepCheck carrying s in its guarded Cmd field —
// the shape the CLI maps out of the real registry (run guards Cmd + Args).
func blessRunCheck(s string) bless.StepCheck {
	return bless.StepCheck{Uses: "run", Guarded: map[string][]string{"Cmd": {s}}}
}

// enrolledStore builds a trust store enrolling one key as both anchor and
// blesser — the single-machine root of trust `trust init` produces.
func enrolledStore(keyID string, pubDER []byte) bless.Store {
	return bless.Store{
		Schema:      bless.StoreSchema,
		AnchorKeyID: keyID,
		Keys: []bless.TrustedKey{{
			KeyID:   keyID,
			Machine: "test",
			Pubkey:  base64.StdEncoding.EncodeToString(pubDER),
			AddedAt: "2026-07-12T00:00:00Z",
		}},
	}
}

func TestWorkflowBless_HappyPath(t *testing.T) {
	dir := cliRedirectConfigDir(t)
	data := []byte(cliValidWorkflow)
	wfPath := cliWriteUserWorkflow(t, dir, "demo", data)

	key := cliGenKey(t)
	pubDER := cliPubDER(t, key)
	keyID := bless.Fingerprint(pubDER)

	swapTrustStorer(t, fakeTrustStorer{store: enrolledStore(keyID, pubDER)})
	cliFakeBless(t, cliFakeBlesser{key: key})

	cmd := newWorkflowBlessCmd(module.Deps{Runner: &exec.FakeRunner{}})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"demo"})
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("bless: %v", err)
	}

	// The written sidecar must decode and its signature must verify over the
	// workflow bytes under the enrolled key — the exact crypto Verify step 4
	// runs. (The full Verifier can't run here: AnchorPath is a compiled-in
	// constant with no test seam by design.)
	raw, err := os.ReadFile(bless.SidecarPath(wfPath))
	if err != nil {
		t.Fatalf("read sidecar: %v", err)
	}
	env, err := bless.DecodeEnvelope(raw)
	if err != nil {
		t.Fatalf("decode sidecar envelope: %v", err)
	}
	if env.KeyID != keyID {
		t.Errorf("sidecar key_id = %q, want %q", env.KeyID, keyID)
	}
	sig, err := base64.StdEncoding.DecodeString(env.Signature)
	if err != nil {
		t.Fatalf("sidecar signature base64: %v", err)
	}
	dg := bless.TaggedDigest(bless.DomainWorkflow, data)
	if !ecdsa.VerifyASN1(&key.PublicKey, dg[:], sig) {
		t.Error("written blessing does not verify over the workflow bytes")
	}
}

func TestWorkflowBless_RefusesBuiltin(t *testing.T) {
	cliRedirectConfigDir(t)
	cmd := newWorkflowBlessCmd(module.Deps{Runner: &exec.FakeRunner{}})
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"clean-room-review"})
	err := cmd.ExecuteContext(context.Background())
	if err == nil || !strings.Contains(err.Error(), "built-in") {
		t.Fatalf("want a built-in refusal, got %v", err)
	}
}

func TestWorkflowBless_RefusesParamRefInRunStep(t *testing.T) {
	dir := cliRedirectConfigDir(t)
	wf := `dsl_version = 1
name = "demo"
version = "1.0.0"

[params]
target = { required = true, help = "x" }

[[step]]
uses = "run"
cmd = "echo"
args = ["${target}"]
`
	cliWriteUserWorkflow(t, dir, "demo", []byte(wf))
	// The param-ref check fires before the trust chain, so no helper/store wiring
	// is needed — this exercises the real registry mapping into StepChecks.
	cmd := newWorkflowBlessCmd(module.Deps{Runner: &exec.FakeRunner{}})
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"demo"})
	err := cmd.ExecuteContext(context.Background())
	if err == nil || !strings.Contains(err.Error(), "target") {
		t.Fatalf("want a ${param} refusal naming target, got %v", err)
	}
}

// TestWorkflowBless_RefusesParamRefInStripGlobs is the strip-list case: the
// clean-room redaction list is a SECURITY CONTROL, so a ${param} in it would let
// an agent weaken the strip-list at run time against already-blessed bytes —
// exactly the "neuter the strip-list so a repo's CLAUDE.md survives into the
// reviewer" threat. End-to-end through the real merged registry.
func TestWorkflowBless_RefusesParamRefInStripGlobs(t *testing.T) {
	dir := cliRedirectConfigDir(t)
	wf := `dsl_version = 1
name = "demo"
version = "1.0.0"

[params]
extra = { default = "CLAUDE.md", help = "x" }

[[step]]
uses = "worktree"
repo = "owner/x"

[[step]]
uses = "strip"
globs = ["${extra}"]
`
	cliWriteUserWorkflow(t, dir, "demo", []byte(wf))
	cmd := newWorkflowBlessCmd(module.Deps{Runner: &exec.FakeRunner{}})
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"demo"})
	err := cmd.ExecuteContext(context.Background())
	if err == nil {
		t.Fatal("a ${param} in a strip step's globs must be refused")
	}
	for _, want := range []string{"strip", "Globs", "extra"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q should mention %q", err, want)
		}
	}
}

// TestWorkflowBless_AllowsParamInWorktreeRepo is the counterweight to the two
// refusal tests: a param feeding a worktree step's repo is the INTENDED
// parameterization (`workflow run clean-room-review --param repo=owner/x`).
// Blessing protects the workflow DEFINITION; the sandbox and the strip-list are
// what protect against repo CONTENTS. If a future "hardening" pass guards repo,
// this test is the alarm.
func TestWorkflowBless_AllowsParamInWorktreeRepo(t *testing.T) {
	dir := cliRedirectConfigDir(t)
	data := []byte(`dsl_version = 1
name = "demo"
version = "1.0.0"

[params]
repo   = { required = true, help = "owner/repo or local path" }
branch = { default = "main", help = "ref to review" }

[[step]]
uses = "worktree"
repo = "${repo}"
ref  = "${branch}"

[[step]]
uses = "strip"
globs = ["CLAUDE.md", ".claude/"]

[[step]]
uses = "run"
cmd  = "make"
args = ["-C", "${workspace}", "check"]
`)
	wfPath := cliWriteUserWorkflow(t, dir, "demo", data)

	key := cliGenKey(t)
	pubDER := cliPubDER(t, key)
	keyID := bless.Fingerprint(pubDER)
	swapTrustStorer(t, fakeTrustStorer{store: enrolledStore(keyID, pubDER)})
	cliFakeBless(t, cliFakeBlesser{key: key})

	cmd := newWorkflowBlessCmd(module.Deps{Runner: &exec.FakeRunner{}})
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"demo"})
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("a param feeding worktree repo/ref must bless: %v", err)
	}
	if _, err := os.Stat(bless.SidecarPath(wfPath)); err != nil {
		t.Errorf("blessing sidecar not written: %v", err)
	}
}

func TestWorkflowBless_RefusesUnknownVerb(t *testing.T) {
	dir := cliRedirectConfigDir(t)
	wf := `dsl_version = 1
name = "demo"
version = "1.0.0"

[[step]]
uses = "teleport"
`
	cliWriteUserWorkflow(t, dir, "demo", []byte(wf))
	cmd := newWorkflowBlessCmd(module.Deps{Runner: &exec.FakeRunner{}})
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"demo"})
	err := cmd.ExecuteContext(context.Background())
	if err == nil || !strings.Contains(err.Error(), "teleport") {
		t.Fatalf("want an unknown-verb refusal, got %v", err)
	}
}

func TestWorkflowBless_NotEnrolledKeyRefused(t *testing.T) {
	dir := cliRedirectConfigDir(t)
	cliWriteUserWorkflow(t, dir, "demo", []byte(cliValidWorkflow))

	machineKey := cliGenKey(t)

	// The store enrolls a DIFFERENT (peer) key, not this machine's.
	peer := cliGenKey(t)
	peerPub := cliPubDER(t, peer)
	peerID := bless.Fingerprint(peerPub)
	swapTrustStorer(t, fakeTrustStorer{store: enrolledStore(peerID, peerPub)})
	// This machine's key is machineKey; a signErr guards that Sign is never
	// reached once the store lookup rejects the unenrolled key.
	cliFakeBless(t, cliFakeBlesser{key: machineKey, signErr: fmt.Errorf("sign must not be reached for an unenrolled key")})

	cmd := newWorkflowBlessCmd(module.Deps{Runner: &exec.FakeRunner{}})
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"demo"})
	err := cmd.ExecuteContext(context.Background())
	if err == nil || !strings.Contains(err.Error(), "not enrolled") {
		t.Fatalf("want a not-enrolled refusal, got %v", err)
	}
}

func TestWorkflowTrustInit_RefusesWhenAnchorExists(t *testing.T) {
	cliRedirectConfigDir(t)
	existing := filepath.Join(t.TempDir(), "trust-anchor.pub")
	if err := os.WriteFile(existing, []byte("x"), 0o644); err != nil {
		t.Fatalf("write anchor: %v", err)
	}
	old := anchorStatPath
	anchorStatPath = existing
	t.Cleanup(func() { anchorStatPath = old })

	cmd := newWorkflowTrustInitCmd(module.Deps{Runner: &exec.FakeRunner{}})
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs(nil)
	err := cmd.ExecuteContext(context.Background())
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("want an already-exists refusal, got %v", err)
	}
}

// spyAnchorInstall swaps the privileged anchor write for a recorder, returning
// the slice it appends each installed key to. A unit test cannot create a
// root-owned /etc file, and the real InstallAnchor now reads the anchor back to
// detect a raced write — so the CLI tests exercise the trust-init FLOW here and
// leave the argv + read-back assertions to internal/bless's own tests.
func spyAnchorInstall(t *testing.T, fail error) *[][]byte {
	t.Helper()
	var calls [][]byte
	prev := installAnchor
	installAnchor = func(_ context.Context, _ exec.Runner, pubDER []byte) error {
		calls = append(calls, pubDER)
		return fail
	}
	t.Cleanup(func() { installAnchor = prev })
	return &calls
}

func TestWorkflowTrustInit_HappyPath(t *testing.T) {
	cliRedirectConfigDir(t)
	key := cliGenKey(t)
	pubDER := cliPubDER(t, key)
	cliFakeBless(t, cliFakeBlesser{key: key})
	anchorCallsPtr := spyAnchorInstall(t, nil)
	// Point the refuse-check at a guaranteed-absent path so the test is
	// hermetic even on a machine that has really run trust init.
	absent := filepath.Join(t.TempDir(), "no-anchor.pub")
	old := anchorStatPath
	anchorStatPath = absent
	t.Cleanup(func() { anchorStatPath = old })

	cmd := newWorkflowTrustInitCmd(module.Deps{Runner: &exec.FakeRunner{}})
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetArgs(nil)
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("trust init: %v", err)
	}

	storePath, err := config.TrustStorePath()
	if err != nil {
		t.Fatalf("TrustStorePath: %v", err)
	}
	if _, err := os.Stat(storePath); err != nil {
		t.Errorf("trust store not written: %v", err)
	}
	if _, err := os.Stat(bless.SidecarPath(storePath)); err != nil {
		t.Errorf("trust store sidecar not written: %v", err)
	}

	// The anchor is installed LAST, after the key and store exist — so a
	// cancelled sudo leaves reusable material (see the resume test). The argv it
	// builds, and its post-install read-back, are pinned by internal/bless's own
	// tests; here we assert the flow reached it with the enrolled key.
	anchorCalls := *anchorCallsPtr
	if len(anchorCalls) != 1 {
		t.Fatalf("expected exactly one anchor install, got %d", len(anchorCalls))
	}
	if string(anchorCalls[0]) != string(pubDER) {
		t.Error("anchor install did not receive the enrolled public key")
	}
}

func TestWorkflowTrustInit_CancelledSudoResumes(t *testing.T) {
	cliRedirectConfigDir(t)
	absent := filepath.Join(t.TempDir(), "no-anchor.pub")
	old := anchorStatPath
	anchorStatPath = absent
	t.Cleanup(func() { anchorStatPath = old })

	key := cliGenKey(t)

	// The anchor write fails the first time (the human cancels the sudo prompt)
	// and succeeds on the resume — the point of the test is that run 1 leaves the
	// key and store REUSABLE, so run 2 completes the anchor leg alone.
	var anchorAttempts int
	prevInstall := installAnchor
	installAnchor = func(_ context.Context, _ exec.Runner, _ []byte) error {
		anchorAttempts++
		if anchorAttempts == 1 {
			return errors.New("sudo: cancelled by user")
		}
		return nil
	}
	t.Cleanup(func() { installAnchor = prevInstall })

	// Run 1: enroll mints the key; the anchor install is cancelled.
	cliFakeBless(t, cliFakeBlesser{key: key})
	cmd1 := newWorkflowTrustInitCmd(module.Deps{Runner: &exec.FakeRunner{}})
	cmd1.SetOut(new(bytes.Buffer))
	cmd1.SetErr(new(bytes.Buffer))
	cmd1.SetArgs(nil)
	if err := cmd1.ExecuteContext(context.Background()); err == nil {
		t.Fatal("run 1 should fail at the anchor install")
	}
	storePath, _ := config.TrustStorePath()
	if _, err := os.Stat(storePath); err != nil {
		t.Fatalf("store should survive a cancelled sudo: %v", err)
	}

	// Run 2: the key blob already exists, so enroll reports ErrLabelExists and
	// EnsureKey falls back to PublicKey rather than wedging; sudo succeeds, so the
	// resume completes the anchor leg alone.
	cliFakeBless(t, cliFakeBlesser{key: key, enrollErr: bless.ErrLabelExists})
	cmd2 := newWorkflowTrustInitCmd(module.Deps{Runner: &exec.FakeRunner{}})
	cmd2.SetOut(new(bytes.Buffer))
	cmd2.SetArgs(nil)
	if err := cmd2.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("resume run should complete: %v", err)
	}
	if anchorAttempts != 2 {
		t.Errorf("expected the anchor install to be attempted on both runs, got %d", anchorAttempts)
	}
}

// TestBlessRefExtractionMatchesInterpolate pins bless's ${} extraction to
// step.Context.Interpolate: the names CheckGuardedParamRefs treats as refs in a
// guarded field MUST equal the names Interpolate resolves. A drift in either
// scanner (boundary handling, nesting, termination) fails this table.
func TestBlessRefExtractionMatchesInterpolate(t *testing.T) {
	cases := []struct {
		input        string
		refs         []string // names both scanners extract
		unterminated bool
	}{
		{input: "${a}", refs: []string{"a"}},
		{input: "x${a}y${b}", refs: []string{"a", "b"}},
		{input: "${a${b}}", refs: []string{"a${b"}}, // nested — first '}' wins
		{input: "${a", unterminated: true},          // unterminated — both error
		{input: "$a", refs: nil},                    // no "${" — not a ref
		{input: "{a}", refs: nil},
		{input: "", refs: nil},
	}

	for _, tc := range cases {
		t.Run(fmt.Sprintf("%q", tc.input), func(t *testing.T) {
			// Interpolate side: set every expected ref so resolution is observable.
			ctx := step.NewContext(nil)
			for _, r := range tc.refs {
				ctx.Set(r, "X_"+r)
			}
			_, interpErr := ctx.Interpolate(tc.input)

			// bless side: the expected refs as an earlier step's exports, so an
			// in-vocabulary ref is ALLOWED and the only thing left to fail on is
			// an unterminated scan. The input rides a guarded field (run's Cmd).
			allowed := []bless.StepCheck{
				{Uses: "seed", Exports: tc.refs},
				blessRunCheck(tc.input),
			}
			blessErr := bless.CheckGuardedParamRefs(allowed, nil)

			if tc.unterminated {
				if interpErr == nil {
					t.Errorf("Interpolate(%q) should error (unterminated)", tc.input)
				}
				if blessErr == nil {
					t.Errorf("CheckGuardedParamRefs(%q) should error (unterminated)", tc.input)
				}
				return
			}
			if interpErr != nil {
				t.Errorf("Interpolate(%q) errored with all names set: %v", tc.input, interpErr)
			}
			if blessErr != nil {
				t.Errorf("CheckGuardedParamRefs(%q) errored with refs allowed: %v", tc.input, blessErr)
			}

			// For ref-bearing inputs, prove BOTH extract the identical first
			// name: with nothing allowed, bless names the first ref; with the
			// name unset, Interpolate reports the same unknown variable.
			if len(tc.refs) > 0 {
				first := tc.refs[0]
				refused := bless.CheckGuardedParamRefs([]bless.StepCheck{blessRunCheck(tc.input)}, nil)
				if refused == nil || !strings.Contains(refused.Error(), first) {
					t.Errorf("CheckGuardedParamRefs(%q) should refuse naming ${%s}, got %v", tc.input, first, refused)
				}
				bare := step.NewContext(nil)
				if _, e := bare.Interpolate(tc.input); e == nil || !strings.Contains(e.Error(), first) {
					t.Errorf("Interpolate(%q) unset should name ${%s}, got %v", tc.input, first, e)
				}
			}
		})
	}
}
