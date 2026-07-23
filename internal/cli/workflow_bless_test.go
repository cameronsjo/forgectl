package cli

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/rand"
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

// --- trust rebuild (#86) ---------------------------------------------------

// Anchor lets the existing fakeTrustStorer satisfy the extended trustStorer
// interface. The bless / trust-list tests that use fakeTrustStorer never call it;
// rebuild tests drive fakeTrustReader, which controls the anchor and the store
// legs independently.
func (f fakeTrustStorer) Anchor() (*ecdsa.PublicKey, string, error) {
	return nil, f.store.AnchorKeyID, f.err
}

// fakeTrustReader controls the anchor leg (Anchor) and the store leg
// (TrustedStore) independently — the shape rebuild needs, where the anchor is
// intact but the store is missing (storeErr set) or holds a peer.
type fakeTrustReader struct {
	anchorFP  string
	anchorErr error
	store     bless.Store
	storeErr  error
}

func (f fakeTrustReader) Anchor() (*ecdsa.PublicKey, string, error) {
	return nil, f.anchorFP, f.anchorErr
}
func (f fakeTrustReader) TrustedStore() (bless.Store, error) { return f.store, f.storeErr }

func swapTrustReader(t *testing.T, r trustStorer) {
	t.Helper()
	old := trustStorerFactory
	trustStorerFactory = func() trustStorer { return r }
	t.Cleanup(func() { trustStorerFactory = old })
}

// rebuildSpyBlesser is a Blesser that FAILS THE TEST if Enroll is ever called —
// the guard that rebuild never mints a key. PublicKey serves a canned key (or a
// canned error, to drive the not-present / not-presence-gated branches); Sign
// signs with key so a rebuilt store's crypto actually verifies.
type rebuildSpyBlesser struct {
	t       *testing.T
	key     *ecdsa.PrivateKey
	pubDER  []byte
	pubErr  error
	signErr error
	signed  bool
}

func (b *rebuildSpyBlesser) Enroll(context.Context, string) ([]byte, error) {
	b.t.Errorf("rebuild must NEVER Enroll — a rebuild must not mint a key")
	return nil, errors.New("enroll is forbidden during rebuild")
}
func (b *rebuildSpyBlesser) PublicKey(context.Context, string) ([]byte, error) {
	if b.pubErr != nil {
		return nil, b.pubErr
	}
	return b.pubDER, nil
}
func (b *rebuildSpyBlesser) Sign(_ context.Context, _ string, digest [32]byte) ([]byte, error) {
	if b.signErr != nil {
		return nil, b.signErr
	}
	b.signed = true
	return ecdsa.SignASN1(rand.Reader, b.key, digest[:])
}

func installBlesser(t *testing.T, b bless.Blesser) {
	t.Helper()
	prev := blesserFactory
	blesserFactory = func(context.Context, exec.Runner) (bless.Blesser, error) { return b, nil }
	t.Cleanup(func() { blesserFactory = prev })
}

// assertStoreVerifiesUnderAnchor replays the TrustedStore checks against a store
// the CLI wrote: the sidecar must name the anchor fingerprint, its signature must
// verify under the anchor key over the trust-domain digest of the store bytes,
// and the decoded store must root on that anchor and enroll exactly wantEnrolled.
// (The real Verifier.TrustedStore reads the compiled-in AnchorPath, which has no
// test seam — TestTrustedStore_SingleMachineAnchorIsSigner proves the same shape
// passes the real path; here we prove the CLI produced that shape.)
func assertStoreVerifiesUnderAnchor(t *testing.T, storePath string, anchorKey *ecdsa.PrivateKey, wantEnrolled string) {
	t.Helper()
	anchorFP := bless.Fingerprint(cliPubDER(t, anchorKey))
	storeBytes, err := os.ReadFile(storePath)
	if err != nil {
		t.Fatalf("read rebuilt store: %v", err)
	}
	sidecar, err := os.ReadFile(bless.SidecarPath(storePath))
	if err != nil {
		t.Fatalf("read rebuilt store sidecar: %v", err)
	}
	env, err := bless.DecodeEnvelope(sidecar)
	if err != nil {
		t.Fatalf("decode store sidecar: %v", err)
	}
	if env.KeyID != anchorFP {
		t.Errorf("store sidecar key_id = %q, want anchor %q", env.KeyID, anchorFP)
	}
	sig, err := base64.StdEncoding.DecodeString(env.Signature)
	if err != nil {
		t.Fatalf("store sidecar signature base64: %v", err)
	}
	dg := bless.TaggedDigest(bless.DomainTrust, storeBytes)
	if !ecdsa.VerifyASN1(&anchorKey.PublicKey, dg[:], sig) {
		t.Error("rebuilt store signature does not verify under the anchor key")
	}
	store, err := bless.DecodeStore(storeBytes)
	if err != nil {
		t.Fatalf("decode rebuilt store: %v", err)
	}
	if store.AnchorKeyID != anchorFP {
		t.Errorf("rebuilt store anchor_key_id = %q, want %q", store.AnchorKeyID, anchorFP)
	}
	if len(store.Keys) != 1 || store.Keys[0].KeyID != wantEnrolled {
		t.Errorf("rebuilt store enrolls %+v, want the single key %s", store.Keys, wantEnrolled)
	}
}

func mustStorePath(t *testing.T) string {
	t.Helper()
	p, err := config.TrustStorePath()
	if err != nil {
		t.Fatalf("TrustStorePath: %v", err)
	}
	return p
}

// TestWorkflowTrustRebuild_HappyPath is guard (v) AND guard (i): the anchor is
// intact, the store is missing, this machine's key IS the anchor — so rebuild
// re-writes a store that re-verifies. The spy blesser fails the test if Enroll is
// reached (never-mint), and spyAnchorInstall asserts the anchor is never touched.
func TestWorkflowTrustRebuild_HappyPath(t *testing.T) {
	cliRedirectConfigDir(t)
	key := cliGenKey(t)
	pubDER := cliPubDER(t, key)
	keyID := bless.Fingerprint(pubDER)

	// Anchor is this machine's key; the store is genuinely gone (the clean
	// recovery case → ErrTrustStoreMissing, which proceeds without a verify note).
	swapTrustReader(t, fakeTrustReader{anchorFP: keyID, storeErr: bless.ErrTrustStoreMissing})
	spy := &rebuildSpyBlesser{t: t, key: key, pubDER: pubDER}
	installBlesser(t, spy)
	anchorCalls := spyAnchorInstall(t, nil)

	cmd := newWorkflowTrustRebuildCmd(module.Deps{Runner: &exec.FakeRunner{}})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs(nil)
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("rebuild: %v", err)
	}

	if !spy.signed {
		t.Error("rebuild must sign the reconstructed store")
	}
	if len(*anchorCalls) != 0 {
		t.Errorf("rebuild must NEVER install the anchor, got %d calls", len(*anchorCalls))
	}
	if !strings.Contains(out.String(), "OVERWRITES") {
		t.Errorf("rebuild must print a loud overwrite warning, got %q", out.String())
	}
	if strings.Contains(out.String(), "could not be verified") {
		t.Errorf("a genuinely-absent store must rebuild silently, but printed a verify note: %q", out.String())
	}
	// The written store re-verifies, enrolling ONLY this machine's key.
	assertStoreVerifiesUnderAnchor(t, mustStorePath(t), key, keyID)
	// The atomic write leaves no staged temp files behind.
	assertNoTrustTempFiles(t, mustStorePath(t))
}

// assertNoTrustTempFiles fails if writeStoreAndSidecar left any staged temp file
// in the store directory — they must all be renamed into place or cleaned up.
func assertNoTrustTempFiles(t *testing.T, storePath string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(storePath), ".*.tmp-*"))
	if err != nil {
		t.Fatalf("glob temp files: %v", err)
	}
	if len(matches) != 0 {
		t.Errorf("writeStoreAndSidecar left staged temp files: %v", matches)
	}
}

// TestWorkflowTrustRebuild_UnverifiableStoreProceedsWithNote covers the #86
// robustness fix: a store that EXISTS but cannot be read or verified (corruption,
// or a transient I/O error — surfaced as a non-Missing error) is still a recovery
// case, so rebuild proceeds — but says so explicitly, never silently, because an
// unreadable store might still enroll a peer the rebuild will drop.
func TestWorkflowTrustRebuild_UnverifiableStoreProceedsWithNote(t *testing.T) {
	cliRedirectConfigDir(t)
	key := cliGenKey(t)
	pubDER := cliPubDER(t, key)
	keyID := bless.Fingerprint(pubDER)

	// A present-but-unverifiable store surfaces as ErrTrustStoreInvalid (not Missing).
	swapTrustReader(t, fakeTrustReader{anchorFP: keyID, storeErr: bless.ErrTrustStoreInvalid})
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
		t.Errorf("an unverifiable store must be noted, not silently overwritten, got %q", out.String())
	}
	assertStoreVerifiesUnderAnchor(t, mustStorePath(t), key, keyID)
}

// TestWorkflowTrustRebuild_HardAbortsOnPlantedKey is guard (ii): a key that can
// sign without presence (ErrKeyNotPresenceGated) must abort the rebuild — never
// be anointed into a fresh store.
func TestWorkflowTrustRebuild_HardAbortsOnPlantedKey(t *testing.T) {
	cliRedirectConfigDir(t)
	key := cliGenKey(t)
	keyID := bless.Fingerprint(cliPubDER(t, key))
	swapTrustReader(t, fakeTrustReader{anchorFP: keyID, storeErr: bless.ErrTrustStoreInvalid})
	installBlesser(t, &rebuildSpyBlesser{t: t, key: key, pubErr: bless.ErrKeyNotPresenceGated})
	spyAnchorInstall(t, nil)

	cmd := newWorkflowTrustRebuildCmd(module.Deps{Runner: &exec.FakeRunner{}})
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs(nil)
	err := cmd.ExecuteContext(context.Background())
	if err == nil || !errors.Is(err, bless.ErrKeyNotPresenceGated) {
		t.Fatalf("rebuild = %v, want ErrKeyNotPresenceGated", err)
	}
	if _, serr := os.Stat(mustStorePath(t)); !os.IsNotExist(serr) {
		t.Error("rebuild must not write a store when the key is not presence-gated")
	}
}

// TestWorkflowTrustRebuild_RefusesWhenKeyIsNotAnchor is guard (iii): only the
// anchor-holding machine may rebuild. A machine whose key is not the anchor is
// refused, and no store is written.
func TestWorkflowTrustRebuild_RefusesWhenKeyIsNotAnchor(t *testing.T) {
	cliRedirectConfigDir(t)
	machineKey := cliGenKey(t)
	machinePub := cliPubDER(t, machineKey)

	// The anchor is a DIFFERENT key than this machine holds.
	anchorKey := cliGenKey(t)
	anchorFP := bless.Fingerprint(cliPubDER(t, anchorKey))

	swapTrustReader(t, fakeTrustReader{anchorFP: anchorFP, storeErr: bless.ErrTrustStoreInvalid})
	installBlesser(t, &rebuildSpyBlesser{t: t, key: machineKey, pubDER: machinePub,
		signErr: fmt.Errorf("sign must not be reached when this machine is not the anchor")})
	spyAnchorInstall(t, nil)

	cmd := newWorkflowTrustRebuildCmd(module.Deps{Runner: &exec.FakeRunner{}})
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs(nil)
	err := cmd.ExecuteContext(context.Background())
	if err == nil || !strings.Contains(err.Error(), "not the trust anchor") {
		t.Fatalf("rebuild = %v, want a not-the-anchor refusal", err)
	}
	if _, serr := os.Stat(mustStorePath(t)); !os.IsNotExist(serr) {
		t.Error("rebuild must not write a store when this machine is not the anchor")
	}
}

// TestWorkflowTrustRebuild_RefusesWhenNoAnchor is guard (iv): a missing or
// non-root anchor means there is nothing to rebuild against — refuse and route
// the user to `trust init`.
func TestWorkflowTrustRebuild_RefusesWhenNoAnchor(t *testing.T) {
	cliRedirectConfigDir(t)
	swapTrustReader(t, fakeTrustReader{anchorErr: fmt.Errorf("%w: missing", bless.ErrNoAnchor)})
	// A blesser that fails the test if reached — the anchor check gates before it.
	installBlesser(t, &rebuildSpyBlesser{t: t, key: cliGenKey(t),
		pubErr: fmt.Errorf("PublicKey must not be reached when there is no anchor")})
	spyAnchorInstall(t, nil)

	cmd := newWorkflowTrustRebuildCmd(module.Deps{Runner: &exec.FakeRunner{}})
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs(nil)
	err := cmd.ExecuteContext(context.Background())
	if err == nil || !strings.Contains(err.Error(), "trust init") {
		t.Fatalf("rebuild = %v, want a no-anchor refusal routing to trust init", err)
	}
	if _, serr := os.Stat(mustStorePath(t)); !os.IsNotExist(serr) {
		t.Error("rebuild must not write a store when there is no anchor")
	}
}

// TestWorkflowTrustRebuild_RefusesWhenStoreEnrollsPeer is the forward-coupling
// guard (SECURITY-REVIEW 3a): if a currently-valid store enrolls a peer machine,
// rebuilding would SILENTLY DROP it — so refuse. Today `trust add` is
// unimplemented (the single-machine invariant), so this exercises the guard
// against a hand-built two-machine store.
func TestWorkflowTrustRebuild_RefusesWhenStoreEnrollsPeer(t *testing.T) {
	cliRedirectConfigDir(t)
	key := cliGenKey(t)
	pubDER := cliPubDER(t, key)
	keyID := bless.Fingerprint(pubDER)

	// A peer key enrolled ALONGSIDE this machine in a currently-valid store.
	peer := cliGenKey(t)
	peerPub := cliPubDER(t, peer)
	peerID := bless.Fingerprint(peerPub)
	twoMachine := bless.Store{
		Schema:      bless.StoreSchema,
		AnchorKeyID: keyID,
		Keys: []bless.TrustedKey{
			{KeyID: keyID, Machine: "self", Pubkey: base64.StdEncoding.EncodeToString(pubDER), AddedAt: "2026-07-12T00:00:00Z"},
			{KeyID: peerID, Machine: "peer", Pubkey: base64.StdEncoding.EncodeToString(peerPub), AddedAt: "2026-07-12T00:00:00Z"},
		},
	}
	swapTrustReader(t, fakeTrustReader{anchorFP: keyID, store: twoMachine})
	installBlesser(t, &rebuildSpyBlesser{t: t, key: key, pubDER: pubDER,
		signErr: fmt.Errorf("sign must not be reached when a peer would be dropped")})
	spyAnchorInstall(t, nil)

	cmd := newWorkflowTrustRebuildCmd(module.Deps{Runner: &exec.FakeRunner{}})
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs(nil)
	err := cmd.ExecuteContext(context.Background())
	if err == nil || !strings.Contains(err.Error(), peerID) {
		t.Fatalf("rebuild = %v, want a refusal naming the peer that would be dropped", err)
	}
}

// TestTrust_NoPeerEnrollmentVerb_GuardsRebuild is the blocking coupling test
// (SECURITY-REVIEW 3b). runTrustRebuild reconstructs the enrolled set SOLELY from
// this machine's key, dropping any peer — safe ONLY while the single-machine
// invariant holds (no `trust add`, no multi-key enrollment). If a peer-enrollment
// verb ever ships under `trust`, this fails and forces a human to revisit
// rebuild's drop-peer semantics before that feature lands (issue #86).
func TestTrust_NoPeerEnrollmentVerb_GuardsRebuild(t *testing.T) {
	cmd := newWorkflowTrustCmd(module.Deps{Runner: &exec.FakeRunner{}})
	got := map[string]bool{}
	for _, c := range cmd.Commands() {
		got[c.Name()] = true
	}
	want := map[string]bool{"init": true, "rebuild": true, "list": true}
	for name := range want {
		if !got[name] {
			t.Errorf("trust is missing the %q subcommand", name)
		}
	}
	for name := range got {
		if !want[name] {
			t.Fatalf("trust grew a %q subcommand — if this is peer enrollment (trust add / multi-key), "+
				"revisit runTrustRebuild: it reconstructs a SINGLE-machine store and would silently drop peers (issue #86)", name)
		}
	}
}
