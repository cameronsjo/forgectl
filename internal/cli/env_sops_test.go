package cli

// Test plan for the --sops route's CLI gate (internal/cli/env.go's
// runEnvSetSops) and the hidden editor (sops_edit.go).
//
// These never run sops: they pin the CLI's own refusals, which all fire BEFORE
// the subprocess. internal/sops' gated integration tests cover the subprocess.
//
// The gate
//   [x] A dotted path is accepted where ValidKey would reject it — the branch
//   [x] A hostile key shape still refuses, and before any input is read
//   [x] --any-file with --sops refuses rather than being silently inert
//   [x] A target with a non-SOPS name refuses, naming the allowed shapes
//   [x] A missing target refuses without attempting creation
//   [x] A .env target refuses (the two allowlists do not overlap)
//   [x] No refusal echoes the value
//
// The editor
//   [x] Refuses with no work directory in the environment
//   [x] Refuses on a nonce mismatch
//   [x] Refuses a second invocation in one run

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/theme"
)

const sopsFixtureDoc = `agentgateway:
    llm_key_hermes: ENC[AES256_GCM,data:abc,type:str]
sops:
    mac: ENC[AES256_GCM,data:def,type:str]
    version: 3.13.3
`

// sopsCLIFixture writes a repo containing a plausible encrypted file.
func sopsCLIFixture(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	initEnvGitRepo(t, repo)
	if err := os.WriteFile(filepath.Join(repo, "secrets.sops.yaml"), []byte(sopsFixtureDoc), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return repo
}

// sopsWorkdirFixture creates a directory named the way the driver names its
// own, because resolveWorkdir requires that prefix. A bare t.TempDir() is
// rejected — which is the constraint working, and the reason this helper
// exists rather than each test hand-rolling a path.
func sopsWorkdirFixture(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), workdirPrefix+"fixture")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	return dir
}

// runEnvSet drives the command tree and returns both streams and its error.
func runEnvSet(t *testing.T, repo string, args ...string) (stdoutText, stderrText string, err error) {
	t.Helper()
	t.Chdir(repo)
	client, _ := envFixture()
	cmd := newEnvTestCmd(client, theme.Theme{})
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs(args)
	runErr := cmd.ExecuteContext(context.Background())
	return stdout.String(), stderr.String(), runErr
}

// TestEnvSetSops_DottedPathPassesTheGate proves the key gate BRANCHES.
//
// `agentgateway.llm_key_hermes` fails env.ValidKey, which forbids dots — so
// before the branch existed, every --sops invocation refused on the key before
// --sops was ever consulted. The assertion is that the refusal is NOT the
// key-pattern one; the command still fails here, because these tests have no
// sops binary wired, and that is fine: the gate is what is under test.
func TestEnvSetSops_DottedPathPassesTheGate(t *testing.T) {
	repo := sopsCLIFixture(t)
	forceNonTTY(t)

	_, _, err := runEnvSet(t, repo, "set", "agentgateway.llm_key_hermes", "--sops")
	if err == nil {
		return // A machine with sops wired could legitimately succeed.
	}
	if strings.Contains(err.Error(), envKeyPattern) {
		t.Errorf("error = %q, want the dotted path to pass the key gate rather than hit ValidKey", err.Error())
	}
}

func TestEnvSetSops_Refusals(t *testing.T) {
	const sentinel = "s3ntinel-VALUE-77x"

	cases := []struct {
		name    string
		setup   func(t *testing.T, repo string)
		args    []string
		wantMsg string
	}{
		{
			name:    "a hostile key shape",
			args:    []string{"set", "KEY=VALUE", "--sops"},
			wantMsg: "path segments must match",
		},
		{
			name:    "a path with a shell metacharacter",
			args:    []string{"set", "block.key;rm -rf /", "--sops"},
			wantMsg: "path segments must match",
		},
		{
			name:    "--any-file combined with --sops",
			args:    []string{"set", "block.key", "--sops", "--any-file"},
			wantMsg: "--any-file does not apply with --sops",
		},
		{
			name: "a target whose name is not a SOPS shape",
			setup: func(t *testing.T, repo string) {
				if err := os.WriteFile(filepath.Join(repo, "values.yaml"), []byte(sopsFixtureDoc), 0o600); err != nil {
					t.Fatalf("WriteFile: %v", err)
				}
			},
			args:    []string{"set", "block.key", "--sops", "--file", "values.yaml"},
			wantMsg: "requires a target named one of",
		},
		{
			name:    "a .env target",
			args:    []string{"set", "block.key", "--sops", "--file", ".env"},
			wantMsg: "requires a target named one of",
		},
		{
			name:    "a missing target",
			args:    []string{"set", "block.key", "--sops", "--file", "secrets.prod.yaml"},
			wantMsg: "does not create one",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			repo := sopsCLIFixture(t)
			if c.setup != nil {
				c.setup(t, repo)
			}
			forceTTYWithPassword(t, sentinel, nil)

			stdout, stderr, err := runEnvSet(t, repo, c.args...)
			if err == nil {
				t.Fatalf("command succeeded, want a refusal\nstdout: %q", stdout)
			}
			if !strings.Contains(err.Error(), c.wantMsg) {
				t.Errorf("error = %q, want it to mention %q", err.Error(), c.wantMsg)
			}
			assertNoSecretInOutput(t, sentinel, stdout, stderr+err.Error())

			// The encrypted fixture must be untouched by any refusal.
			got, readErr := os.ReadFile(filepath.Join(repo, "secrets.sops.yaml")) //nolint:gosec // G304: a fixture this test created
			if readErr != nil {
				t.Fatalf("ReadFile: %v", readErr)
			}
			if string(got) != sopsFixtureDoc {
				t.Error("the encrypted fixture changed on a refusal")
			}
		})
	}
}

// TestEnvSetSops_HostileKeyRefusesBeforeReadingInput pins the ordering the
// .env route already defends: a refusable key must not consume stdin, or a
// secret piped in is read (and held in memory) for a command that was never
// going to run.
func TestEnvSetSops_HostileKeyRefusesBeforeReadingInput(t *testing.T) {
	repo := sopsCLIFixture(t)
	forceNonTTY(t)
	t.Chdir(repo)

	client, _ := envFixture()
	cmd := newEnvTestCmd(client, theme.Theme{})
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))
	// A reader that records whether it was consulted at all.
	probe := &readProbe{data: "s3ntinel-VALUE-77x"}
	cmd.SetIn(probe)
	cmd.SetArgs([]string{"set", "not a valid path", "--sops"})

	if err := cmd.ExecuteContext(context.Background()); err == nil {
		t.Fatal("command succeeded, want a refusal")
	}
	if probe.reads != 0 {
		t.Errorf("stdin was read %d time(s) before the key refusal, want 0", probe.reads)
	}
}

// readProbe counts Read calls so a test can assert stdin was never consulted.
type readProbe struct {
	data  string
	reads int
	pos   int
}

func (r *readProbe) Read(p []byte) (int, error) {
	r.reads++
	if r.pos >= len(r.data) {
		return 0, os.ErrClosed
	}
	n := copy(p, r.data[r.pos:])
	r.pos += n
	return n, nil
}

func TestSopsEdit_RefusesWithoutTheProtocol(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T) (workdir string)
		want  string
	}{
		{
			name:  "no work directory in the environment",
			setup: func(t *testing.T) string { return "" },
			want:  "invoked by forgectl",
		},
		{
			name: "no nonce file",
			setup: func(t *testing.T) string {
				dir := sopsWorkdirFixture(t)
				t.Setenv(sopsNonceEnv, "whatever")
				return dir
			},
			want: "invoked by forgectl",
		},
		{
			name: "a nonce mismatch",
			setup: func(t *testing.T) string {
				dir := sopsWorkdirFixture(t)
				if err := os.WriteFile(filepath.Join(dir, sopsNonceFile), []byte("the-real-nonce"), 0o600); err != nil {
					t.Fatalf("WriteFile: %v", err)
				}
				t.Setenv(sopsNonceEnv, "a-guess")
				return dir
			},
			want: "invoked by forgectl",
		},
		{
			name: "an empty nonce in the environment",
			setup: func(t *testing.T) string {
				dir := sopsWorkdirFixture(t)
				if err := os.WriteFile(filepath.Join(dir, sopsNonceFile), []byte("the-real-nonce"), 0o600); err != nil {
					t.Fatalf("WriteFile: %v", err)
				}
				t.Setenv(sopsNonceEnv, "")
				return dir
			},
			want: "invoked by forgectl",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			doc := filepath.Join(t.TempDir(), "doc.yaml")
			if err := os.WriteFile(doc, []byte("block:\n    k: 'v'\n"), 0o600); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
			original, err := os.ReadFile(doc) //nolint:gosec // G304: a path this test created
			if err != nil {
				t.Fatalf("ReadFile: %v", err)
			}

			workdir := c.setup(t)
			t.Setenv(sopsWorkdirEnv, workdir)
			t.Setenv(sopsPathEnv, "block.k")

			if err := runSopsEdit(doc); err == nil {
				t.Fatal("runSopsEdit succeeded, want a refusal")
			} else if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error = %q, want it to mention %q", err.Error(), c.want)
			}

			after, err := os.ReadFile(doc) //nolint:gosec // G304: a path this test created
			if err != nil {
				t.Fatalf("ReadFile: %v", err)
			}
			if !bytes.Equal(original, after) {
				t.Error("the document was modified despite the refusal")
			}
		})
	}
}

// TestSopsEdit_OnceOnly proves the counter terminates a loop: whatever drives
// a second invocation in one run, the second refuses.
func TestSopsEdit_OnceOnly(t *testing.T) {
	workdir := sopsWorkdirFixture(t)
	const nonce = "a-test-nonce"
	if err := os.WriteFile(filepath.Join(workdir, sopsNonceFile), []byte(nonce), 0o600); err != nil {
		t.Fatalf("WriteFile nonce: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workdir, sopsValueFile), []byte("a-value"), 0o600); err != nil {
		t.Fatalf("WriteFile value: %v", err)
	}
	t.Setenv(sopsWorkdirEnv, workdir)
	t.Setenv(sopsNonceEnv, nonce)
	t.Setenv(sopsPathEnv, "block.k")

	doc := filepath.Join(t.TempDir(), "doc.yaml")
	if err := os.WriteFile(doc, []byte("block:\n    k: 'old'\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if err := runSopsEdit(doc); err != nil {
		t.Fatalf("the first invocation failed: %v", err)
	}
	edited, err := os.ReadFile(doc) //nolint:gosec // G304: a path this test created
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(string(edited), "k: 'a-value'") {
		t.Errorf("the first invocation did not write the value: %q", edited)
	}

	if err := runSopsEdit(doc); err == nil {
		t.Fatal("the second invocation succeeded, want a refusal")
	} else if !strings.Contains(err.Error(), "more than once") {
		t.Errorf("error = %q, want it to name the once-only rule", err.Error())
	}
}

// TestSopsEdit_RecordsTheOutcome pins the relay the driver depends on for the
// added-versus-replaced distinction.
func TestSopsEdit_RecordsTheOutcome(t *testing.T) {
	workdir := sopsWorkdirFixture(t)
	const nonce = "a-test-nonce"
	if err := os.WriteFile(filepath.Join(workdir, sopsNonceFile), []byte(nonce), 0o600); err != nil {
		t.Fatalf("WriteFile nonce: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workdir, sopsValueFile), []byte("a-value"), 0o600); err != nil {
		t.Fatalf("WriteFile value: %v", err)
	}
	t.Setenv(sopsWorkdirEnv, workdir)
	t.Setenv(sopsNonceEnv, nonce)
	t.Setenv(sopsPathEnv, "block.added")

	doc := filepath.Join(t.TempDir(), "doc.yaml")
	if err := os.WriteFile(doc, []byte("block:\n    existing: 'x'\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := runSopsEdit(doc); err != nil {
		t.Fatalf("runSopsEdit: %v", err)
	}

	recorded, err := os.ReadFile(filepath.Join(workdir, sopsResultFile)) //nolint:gosec // G304: a path this test created
	if err != nil {
		t.Fatalf("ReadFile result: %v", err)
	}
	if got := strings.TrimSpace(string(recorded)); got != "added" {
		t.Errorf("recorded outcome = %q, want %q", got, "added")
	}
}
