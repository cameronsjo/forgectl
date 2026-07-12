package cli

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
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
	"github.com/cameronsjo/forgectl/internal/workflow"
)

// A minimal user workflow with a single run step and no required params, so
// dry-run BuildPlan succeeds without any --param.
const cliValidWorkflow = `dsl_version = 1
name = "demo"
version = "1.0.0"

[[step]]
uses = "run"
cmd = "echo"
args = ["hi"]
`

// cliRedirectConfigDir points os.UserConfigDir (config.WorkflowsDir /
// TrustStorePath) at a temp dir on macOS and Linux, and returns the user
// workflows directory.
func cliRedirectConfigDir(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("XDG_CONFIG_HOME", tmp)
	dir, err := config.WorkflowsDir()
	if err != nil {
		t.Fatalf("WorkflowsDir: %v", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll %s: %v", dir, err)
	}
	return dir
}

// cliWriteUserWorkflow writes a <name>.workflow.toml under the user workflows
// dir and returns its path.
func cliWriteUserWorkflow(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(dir, name+".workflow.toml")
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatalf("write user workflow %s: %v", p, err)
	}
	return p
}

// swapVerifier replaces verifierFactory for the duration of a test.
func swapVerifier(t *testing.T, v workflow.Verifier) {
	t.Helper()
	old := verifierFactory
	verifierFactory = func() workflow.Verifier { return v }
	t.Cleanup(func() { verifierFactory = old })
}

// swapTrustStorer replaces trustStorerFactory for the duration of a test.
func swapTrustStorer(t *testing.T, ts trustStorer) {
	t.Helper()
	old := trustStorerFactory
	trustStorerFactory = func() trustStorer { return ts }
	t.Cleanup(func() { trustStorerFactory = old })
}

// spyVerifier records whether Verify was consulted — the assertion behind the
// builtin/dry-run skip tests.
type spyVerifier struct {
	consulted bool
	err       error
}

func (s *spyVerifier) Verify(string, []byte) error { s.consulted = true; return s.err }

// fakeVerifier returns a canned error (or nil) — the CLI policy tests don't
// exercise real crypto, they exercise the run/verify verbs' handling.
type fakeVerifier struct{ err error }

func (f fakeVerifier) Verify(string, []byte) error { return f.err }

// fakeTrustStorer serves a canned Store (or error) for the bless/trust verbs.
type fakeTrustStorer struct {
	store bless.Store
	err   error
}

func (f fakeTrustStorer) TrustedStore() (bless.Store, error) { return f.store, f.err }

// cliGenKey / cliPubDER build P-256 test material without importing the bless
// package's internal test helpers.
func cliGenKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate P-256 key: %v", err)
	}
	return k
}

func cliPubDER(t *testing.T, k *ecdsa.PrivateKey) []byte {
	t.Helper()
	der, err := bless.EncodePublicKey(&k.PublicKey)
	if err != nil {
		t.Fatalf("encode public key: %v", err)
	}
	return der
}

// cliFakeHelperEnv points FORGECTL_BLESS_HELPER at a real (dummy) on-disk file
// so bless.NewHelperBlesser's existence check passes; the FakeRunner supplies
// the canned replies.
func cliFakeHelperEnv(t *testing.T) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "forgectl-bless-helper")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write dummy helper: %v", err)
	}
	t.Setenv("FORGECTL_BLESS_HELPER", p)
}

// cliExitErr mimics os/exec.ExitError's ExitCode so mapHelperError's errors.As
// walk can map a helper exit code to a bless sentinel.
type cliExitErr struct{ code int }

func (e cliExitErr) Error() string { return fmt.Sprintf("exit status %d", e.code) }
func (e cliExitErr) ExitCode() int { return e.code }

func TestWorkflowRun_UnsignedUserWorkflowRefused(t *testing.T) {
	dir := cliRedirectConfigDir(t)
	cliWriteUserWorkflow(t, dir, "demo", []byte(cliValidWorkflow))
	swapVerifier(t, fakeVerifier{err: fmt.Errorf("%w: run 'forgectl workflow bless demo' to approve this file", bless.ErrUnblessed)})

	cmd := newWorkflowRunCmd(module.Deps{Runner: &exec.FakeRunner{}})
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"demo"})

	err := cmd.ExecuteContext(context.Background())
	if err == nil {
		t.Fatal("an unsigned user workflow must be refused")
	}
	if !errors.Is(err, bless.ErrUnblessed) {
		t.Errorf("error should wrap ErrUnblessed, got %v", err)
	}
	if !strings.Contains(err.Error(), "demo") || !strings.Contains(err.Error(), "workflow bless") {
		t.Errorf("error should name the workflow and the bless hint: %v", err)
	}
}

func TestWorkflowRun_DryRunSkipsVerifier(t *testing.T) {
	dir := cliRedirectConfigDir(t)
	cliWriteUserWorkflow(t, dir, "demo", []byte(cliValidWorkflow))
	spy := &spyVerifier{}
	swapVerifier(t, spy)

	cmd := newWorkflowRunCmd(module.Deps{Runner: &exec.FakeRunner{}})
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetArgs([]string{"demo", "--dry-run"})

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("dry-run of an unblessed file should succeed: %v", err)
	}
	if spy.consulted {
		t.Error("dry-run must NOT consult the verifier")
	}
}

func TestWorkflowRun_BuiltinSkipsVerifier(t *testing.T) {
	cliRedirectConfigDir(t)
	spy := &spyVerifier{}
	swapVerifier(t, spy)

	cmd := newWorkflowRunCmd(module.Deps{Runner: &exec.FakeRunner{}})
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))
	// No --param repo: the run errors later at BuildPlan (missing required
	// param), AFTER the deliberately-skipped verify. The assertion is only that
	// a builtin never consults the verifier — the Builtin branch, not dry-run.
	cmd.SetArgs([]string{"clean-room-review"})
	_ = cmd.ExecuteContext(context.Background())

	if spy.consulted {
		t.Error("a builtin run must NOT consult the verifier")
	}
}

func TestWorkflowRun_TamperedRefused(t *testing.T) {
	dir := cliRedirectConfigDir(t)
	cliWriteUserWorkflow(t, dir, "demo", []byte(cliValidWorkflow))
	swapVerifier(t, fakeVerifier{err: fmt.Errorf("%w: workflow signature does not verify", bless.ErrTampered)})

	cmd := newWorkflowRunCmd(module.Deps{Runner: &exec.FakeRunner{}})
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"demo"})

	err := cmd.ExecuteContext(context.Background())
	if !errors.Is(err, bless.ErrTampered) {
		t.Fatalf("want ErrTampered, got %v", err)
	}
	if !strings.Contains(err.Error(), "demo") {
		t.Errorf("error should name the workflow: %v", err)
	}
}

func TestWorkflowVerify_BuiltinExempt(t *testing.T) {
	cliRedirectConfigDir(t)
	spy := &spyVerifier{}
	swapVerifier(t, spy)

	cmd := newWorkflowVerifyCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"clean-room-review"})

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("verify of a builtin should succeed: %v", err)
	}
	if spy.consulted {
		t.Error("a builtin verify must NOT consult the verifier")
	}
	if !strings.Contains(out.String(), "exempt") {
		t.Errorf("output should mark the builtin exempt: %q", out.String())
	}
}

func TestWorkflowVerify_BlessedValid(t *testing.T) {
	dir := cliRedirectConfigDir(t)
	cliWriteUserWorkflow(t, dir, "demo", []byte(cliValidWorkflow))
	swapVerifier(t, fakeVerifier{err: nil})

	cmd := newWorkflowVerifyCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"demo"})

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !strings.Contains(out.String(), "blessed and valid") {
		t.Errorf("output should confirm valid: %q", out.String())
	}
}

func TestWorkflowVerify_FailsNonZero(t *testing.T) {
	dir := cliRedirectConfigDir(t)
	cliWriteUserWorkflow(t, dir, "demo", []byte(cliValidWorkflow))
	swapVerifier(t, fakeVerifier{err: fmt.Errorf("%w", bless.ErrUnblessed)})

	cmd := newWorkflowVerifyCmd()
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"demo"})

	if err := cmd.ExecuteContext(context.Background()); err == nil {
		t.Fatal("verify of an unblessed file must return an error (non-zero exit)")
	}
}

func TestWorkflowTrustList(t *testing.T) {
	store := bless.Store{
		Schema:      bless.StoreSchema,
		AnchorKeyID: "sha256:1111111111111111111111111111111111111111111111111111111111111111",
		Keys: []bless.TrustedKey{{
			KeyID:   "sha256:2222222222222222222222222222222222222222222222222222222222222222",
			Machine: "sjomba",
			AddedAt: "2026-07-12T00:00:00Z",
		}},
	}
	swapTrustStorer(t, fakeTrustStorer{store: store})

	cmd := newWorkflowTrustListCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs(nil)

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("trust list: %v", err)
	}
	body := out.String()
	for _, want := range []string{store.AnchorKeyID, store.Keys[0].KeyID, "sjomba"} {
		if !strings.Contains(body, want) {
			t.Errorf("trust list output missing %q:\n%s", want, body)
		}
	}
}

func TestWorkflowTrustList_NoAnchorGivesActionableError(t *testing.T) {
	swapTrustStorer(t, fakeTrustStorer{err: fmt.Errorf("%w", bless.ErrNoAnchor)})

	cmd := newWorkflowTrustListCmd()
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs(nil)

	err := cmd.ExecuteContext(context.Background())
	if err == nil || !strings.Contains(err.Error(), "trust init") {
		t.Fatalf("want an actionable trust-init error, got %v", err)
	}
}
