package bless

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	osexec "os/exec"
	"path/filepath"
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

// TestEnsureKey_PlantedKeyAbortsBootstrap reproduces the planted-key bypass at
// the Go seam: an agent drops a non-presence key at the compiled-in label, so
// Enroll fails ErrLabelExists (the blob already exists) and the idempotent
// fallback calls PublicKey — where the helper's presence probe rejects the key
// with exit 6. EnsureKey MUST propagate ErrKeyNotPresenceGated so trust init
// aborts rather than anointing the agent's key as the anchor.
func TestEnsureKey_PlantedKeyAbortsBootstrap(t *testing.T) {
	fake := &exec.FakeRunner{RunFunc: func(_ string, args []string) (string, error) {
		switch args[0] {
		case "enroll":
			return "", exitErr{3} // blob already on disk (planted)
		case "pubkey":
			return "", exitErr{6} // presence probe: not presence-gated
		}
		return "", nil
	}}
	hb := fakeHelper(t, fake)

	if _, err := EnsureKey(context.Background(), hb, "l"); !errors.Is(err, ErrKeyNotPresenceGated) {
		t.Fatalf("EnsureKey = %v, want ErrKeyNotPresenceGated propagated (a planted key must abort bootstrap)", err)
	}
	if fake.Last().Args[0] != "pubkey" {
		t.Errorf("expected the pubkey fallback to run, got %v", fake.Last().Args)
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

// stubAnchorReadback points the post-install read-back seam at the given bytes
// (the anchor file's contents as they would be on disk after the sudo leg).
func stubAnchorReadback(t *testing.T, contents []byte) {
	t.Helper()
	prev := readAnchorFile
	readAnchorFile = func() ([]byte, error) { return contents, nil }
	t.Cleanup(func() { readAnchorFile = prev })
}

// anchorLine renders a public key exactly as InstallAnchor writes it.
func anchorLine(der []byte) []byte {
	return []byte(base64.StdEncoding.EncodeToString(der) + "\n")
}

// TestInstallAnchor_SingleSudoLegCarriesKeyInArgv pins the fix for the staging
// vulnerability: the key must never pass through a file the invoking user can
// write. A same-UID agent could overwrite such a file during the (interactive,
// human-paced) sudo password prompt and have root install ITS key as the anchor.
// So: one sudo leg, key carried as a positional argv element, no temp path
// anywhere in the argv.
func TestInstallAnchor_SingleSudoLegCarriesKeyInArgv(t *testing.T) {
	fake := &exec.FakeRunner{}
	key := genKey(t)
	pubDER := mustPubDER(t, &key.PublicKey)
	stubAnchorReadback(t, anchorLine(pubDER))

	if err := InstallAnchor(context.Background(), fake, pubDER); err != nil {
		t.Fatalf("InstallAnchor: %v", err)
	}

	if len(fake.Calls) != 1 {
		t.Fatalf("expected exactly 1 sudo leg, got %d: %+v", len(fake.Calls), fake.Calls)
	}
	c := fake.Calls[0]
	if !c.Interactive {
		t.Error("the sudo leg must be Interactive (the human types the password)")
	}
	// sudo MUST be invoked by absolute path: a bare "sudo" resolves via the
	// human's PATH, which an agent can poison (a fake sudo phishes the password).
	if c.Name != sudoPath {
		t.Errorf("leg name = %q, want the absolute %q (a bare name is PATH-hijackable)", c.Name, sudoPath)
	}
	if !filepath.IsAbs(c.Name) {
		t.Errorf("sudo must be invoked by an absolute path, got %q", c.Name)
	}

	// argv is: /bin/sh -c <script> _ <b64key> — the inner shell is absolute too.
	b64 := base64.StdEncoding.EncodeToString(pubDER)
	if len(c.Args) != 5 {
		t.Fatalf("argv = %v, want 5 elements (sh -c <script> _ <b64key>)", c.Args)
	}
	if c.Args[0] != shPath || c.Args[1] != "-c" {
		t.Errorf("argv must invoke %s -c, got %v %v", shPath, c.Args[0], c.Args[1])
	}
	if !filepath.IsAbs(c.Args[0]) {
		t.Errorf("the inner shell must be invoked by an absolute path, got %q", c.Args[0])
	}
	if c.Args[3] != "_" {
		t.Errorf("argv[3] = %q, want the \"_\" $0 placeholder", c.Args[3])
	}
	// The key is the LAST positional — never interpolated into the script text.
	if got := c.Args[len(c.Args)-1]; got != b64 {
		t.Errorf("last argv element = %q, want the base64 key %q", got, b64)
	}

	script := c.Args[2]
	if strings.Contains(script, b64) {
		t.Error("the key must NOT be interpolated into the script text; it is a positional argument")
	}
	if !strings.Contains(script, `"$1"`) {
		t.Error(`the script must consume the key as the positional "$1"`)
	}
	if !strings.Contains(script, "set -C") {
		t.Error("the script must set noclobber (set -C) so the > redirect is an O_EXCL create")
	}
	if !strings.Contains(script, "exit 3") {
		t.Error("the script must refuse an existing anchor with exit 3")
	}
	if !strings.Contains(script, AnchorPath) {
		t.Errorf("the script must write the compiled-in anchor path %q", AnchorPath)
	}

	// No staging file, anywhere in the argv: the whole point of the fix.
	for i, a := range c.Args {
		if strings.Contains(a, os.TempDir()) || strings.Contains(a, "forgectl-anchor-") {
			t.Errorf("argv[%d] = %q references a temp staging path; the key must travel in argv only", i, a)
		}
	}
}

// TestInstallAnchor_RefusesExistingAnchor: the script's exit 3 surfaces as a
// typed error, so a re-run of `trust init` reports cleanly instead of silently
// replacing a live root of trust.
func TestInstallAnchor_RefusesExistingAnchor(t *testing.T) {
	fake := &exec.FakeRunner{InteractiveErr: exitErr{3}}
	key := genKey(t)
	stubAnchorReadback(t, nil) // must never be reached

	err := InstallAnchor(context.Background(), fake, mustPubDER(t, &key.PublicKey))
	if !errors.Is(err, ErrAnchorExists) {
		t.Fatalf("InstallAnchor = %v, want ErrAnchorExists", err)
	}
}

// TestInstallAnchor_PostVerifyCatchesSwappedKey is the belt to the argv braces:
// if the anchor on disk after the privileged leg is NOT the key we enrolled,
// something raced us and InstallAnchor must fail loudly rather than leave a
// foreign root of trust in place.
func TestInstallAnchor_PostVerifyCatchesSwappedKey(t *testing.T) {
	ours := genKey(t)
	theirs := genKey(t)
	stubAnchorReadback(t, anchorLine(mustPubDER(t, &theirs.PublicKey)))

	err := InstallAnchor(context.Background(), &exec.FakeRunner{}, mustPubDER(t, &ours.PublicKey))
	if err == nil {
		t.Fatal("expected an error when the installed anchor is a different key")
	}
	if !strings.Contains(err.Error(), "NOT the key that was just enrolled") {
		t.Errorf("error must name the mismatch loudly, got: %v", err)
	}
}

// TestInstallAnchor_PostVerifyCatchesGarbage: an unparseable anchor is a failed
// install, not a success.
func TestInstallAnchor_PostVerifyCatchesGarbage(t *testing.T) {
	key := genKey(t)
	stubAnchorReadback(t, []byte("not-a-key\n"))

	if err := InstallAnchor(context.Background(), &exec.FakeRunner{}, mustPubDER(t, &key.PublicKey)); err == nil {
		t.Fatal("expected an error when the installed anchor does not parse")
	}
}

// TestInstallAnchor_RefusesUnparseableKey: never install bytes we would not
// accept back — a malformed anchor is unrecoverable without root.
func TestInstallAnchor_RefusesUnparseableKey(t *testing.T) {
	fake := &exec.FakeRunner{}
	if err := InstallAnchor(context.Background(), fake, []byte("garbage")); err == nil {
		t.Fatal("expected an error for a non-PKIX key")
	}
	if len(fake.Calls) != 0 {
		t.Errorf("no sudo leg may run for an invalid key, got %+v", fake.Calls)
	}
}

// TestAnchorInstallScript_Behavior runs the REAL script text — the compiled-in
// paths swapped for a temp dir, and the chown legs (root's job, which is what
// the sudo is for) neutered — so the security properties are exercised rather
// than merely grepped for. Everything load-bearing survives the swap: the
// symlink refusals, the existence refusal, and the noclobber create.
func TestAnchorInstallScript_Behavior(t *testing.T) {
	render := func(dir, anchor string) string {
		s := anchorInstallScript
		s = strings.Replace(s, "ANCHOR='"+AnchorPath+"'", "ANCHOR='"+anchor+"'", 1)
		s = strings.Replace(s, "DIR='"+filepath.Dir(AnchorPath)+"'", "DIR='"+dir+"'", 1)
		s = strings.ReplaceAll(s, "chown root:wheel", "true")
		return s
	}
	run := func(t *testing.T, dir, anchor, key string) int {
		t.Helper()
		cmd := osexec.Command("/bin/sh", "-c", render(dir, anchor), "_", key)
		if err := cmd.Run(); err != nil {
			var ee *osexec.ExitError
			if errors.As(err, &ee) {
				return ee.ExitCode()
			}
			t.Fatalf("run script: %v", err)
		}
		return 0
	}

	t.Run("fresh install writes the key 0644", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "forgectl")
		anchor := filepath.Join(dir, "trust-anchor.pub")
		if code := run(t, dir, anchor, "KEYBYTES"); code != 0 {
			t.Fatalf("exit = %d, want 0", code)
		}
		got, err := os.ReadFile(anchor)
		if err != nil {
			t.Fatalf("read anchor: %v", err)
		}
		if string(got) != "KEYBYTES\n" {
			t.Errorf("anchor = %q, want %q", got, "KEYBYTES\n")
		}
		fi, err := os.Stat(anchor)
		if err != nil {
			t.Fatalf("stat anchor: %v", err)
		}
		if fi.Mode().Perm() != 0o644 {
			t.Errorf("anchor mode = %o, want 644", fi.Mode().Perm())
		}
	})

	t.Run("an existing anchor is refused with exit 3", func(t *testing.T) {
		dir := t.TempDir()
		anchor := filepath.Join(dir, "trust-anchor.pub")
		writeFile(t, anchor, []byte("ORIGINAL\n"))

		if code := run(t, dir, anchor, "EVIL"); code != 3 {
			t.Fatalf("exit = %d, want 3 (anchor exists)", code)
		}
		got, err := os.ReadFile(anchor)
		if err != nil {
			t.Fatalf("read anchor: %v", err)
		}
		if string(got) != "ORIGINAL\n" {
			t.Errorf("the existing anchor was overwritten: %q", got)
		}
	})

	// A symlink at the anchor path must never be written THROUGH — that would
	// place root-blessed content wherever the link points.
	t.Run("a symlink at the anchor path is refused", func(t *testing.T) {
		dir := t.TempDir()
		anchor := filepath.Join(dir, "trust-anchor.pub")
		target := filepath.Join(dir, "target")
		writeFile(t, target, []byte("TARGET\n"))
		if err := os.Symlink(target, anchor); err != nil {
			t.Fatalf("symlink: %v", err)
		}

		if code := run(t, dir, anchor, "EVIL"); code != 3 {
			t.Fatalf("exit = %d, want 3 (symlink at the anchor path)", code)
		}
		got, err := os.ReadFile(target)
		if err != nil {
			t.Fatalf("read target: %v", err)
		}
		if string(got) != "TARGET\n" {
			t.Errorf("the symlink target was written through: %q", got)
		}
	})

	t.Run("a symlinked parent directory is refused with exit 4", func(t *testing.T) {
		base := t.TempDir()
		real := filepath.Join(base, "real")
		if err := os.Mkdir(real, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		dir := filepath.Join(base, "forgectl")
		if err := os.Symlink(real, dir); err != nil {
			t.Fatalf("symlink: %v", err)
		}
		anchor := filepath.Join(dir, "trust-anchor.pub")

		if code := run(t, dir, anchor, "EVIL"); code != 4 {
			t.Fatalf("exit = %d, want 4 (symlinked parent dir)", code)
		}
		if _, err := os.Stat(filepath.Join(real, "trust-anchor.pub")); !os.IsNotExist(err) {
			t.Error("the anchor was written through the symlinked directory")
		}
	})
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
