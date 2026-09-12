package sops

// Integration tests for driver.go. They mint an age identity, encrypt a
// fixture, and drive the REAL sops binary through the real sensitive runner.
//
//   [x] A byte-exact round-trip: what goes in is what decrypts out
//   [x] `--extract --output` writes NO trailing newline (the measurement the
//       byte-exact comparison depends on)
//   [x] The encrypted diff is one content line plus sops' two metadata lines,
//       and untouched values keep byte-identical ciphertext
//   [x] The value is NOT present in cleartext anywhere in the file
//   [x] A wrong path refuses and leaves the file byte-identical
//   [x] The rc=200 "file has not changed" case reports success
//   [x] An unencrypted_suffix-matching path refuses before running sops
//   [x] A non-SOPS file refuses
//   [x] The EDITOR override actually took — no real editor was involved
//
// The skip is GATED. FORGECTL_REQUIRE_SOPS_INTEGRATION=1 turns a missing
// sops or age-keygen into a failure rather than a skip, and CI sets it. A CI
// step that installs a tool is a file anyone can edit in a PR; the env gate is
// what makes its removal go red instead of silently skipping the only tests
// that exercise the subprocess.

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/env"
	fcexec "github.com/cameronsjo/forgectl/internal/exec"
)

// requireTools resolves the binaries these tests need, or decides between a
// skip and a failure based on the gate.
func requireTools(t *testing.T) {
	t.Helper()
	required := os.Getenv("FORGECTL_REQUIRE_SOPS_INTEGRATION") == "1"
	for _, bin := range []string{"sops", "age-keygen"} {
		if _, err := exec.LookPath(bin); err != nil {
			if required {
				t.Fatalf("%s is not on PATH and FORGECTL_REQUIRE_SOPS_INTEGRATION=1 — the integration tests must run here", bin)
			}
			t.Skipf("%s is not on PATH; set FORGECTL_REQUIRE_SOPS_INTEGRATION=1 to make this a failure", bin)
		}
	}
}

const fixturePlaintext = `agentgateway:
    llm_key_hermes: seedvalue
    other_key: untouched
    plain_unencrypted: visible
top:
    a: b
`

// sopsFixture builds a git repository containing an encrypted
// secrets.sops.yaml, and returns the repo root and a resolved Target for it.
func sopsFixture(t *testing.T) (repo string, target env.Target) {
	t.Helper()
	requireTools(t)

	// A short base dir, not t.TempDir(): the age key path and the work
	// directory both live under here, and t.TempDir() embeds the whole test
	// name, which has bitten socket-path limits elsewhere in this repo.
	repo, err := os.MkdirTemp("", "fcsops")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(repo) })

	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o750); err != nil {
		t.Fatalf("MkdirAll .git: %v", err)
	}

	keyPath := filepath.Join(repo, "age.key")
	keyOut, err := exec.CommandContext(t.Context(), "age-keygen", "-o", keyPath).CombinedOutput() //nolint:gosec // G204: a fixed tool name with arguments this test constructed
	if err != nil {
		t.Fatalf("age-keygen: %v\n%s", err, keyOut)
	}
	pubOut, err := exec.CommandContext(t.Context(), "age-keygen", "-y", keyPath).Output() //nolint:gosec // G204: a fixed tool name with arguments this test constructed
	if err != nil {
		t.Fatalf("age-keygen -y: %v", err)
	}
	recipient := strings.TrimSpace(string(pubOut))

	// unencrypted_suffix is in the creation rule on purpose: every encrypted
	// file in the estate this feature targets carries it, so the cleartext
	// refusal is exercised against a realistic file rather than a contrived
	// one.
	rules := "creation_rules:\n  - path_regex: .*\n    age: " + recipient + "\n    unencrypted_suffix: _unencrypted\n"
	if err := os.WriteFile(filepath.Join(repo, ".sops.yaml"), []byte(rules), 0o600); err != nil {
		t.Fatalf("WriteFile .sops.yaml: %v", err)
	}

	plainPath := filepath.Join(repo, "plain.yaml")
	if err := os.WriteFile(plainPath, []byte(fixturePlaintext), 0o600); err != nil {
		t.Fatalf("WriteFile plain.yaml: %v", err)
	}

	t.Setenv("SOPS_AGE_KEY_FILE", keyPath)
	encPath := filepath.Join(repo, "secrets.sops.yaml")
	encrypt := exec.CommandContext(t.Context(), "sops", "--encrypt", "--output", encPath, plainPath) //nolint:gosec // G204: a fixed tool name with arguments this test constructed
	// sops discovers .sops.yaml by walking up from its WORKING DIRECTORY, not
	// from the input file's path — so without this it reports "config file not
	// found, or has no creation rules" while the config sits right beside the
	// input.
	encrypt.Dir = repo
	encOut, err := encrypt.CombinedOutput()
	if err != nil {
		t.Fatalf("sops --encrypt: %v\n%s", err, encOut)
	}
	if err := os.Remove(plainPath); err != nil {
		t.Fatalf("Remove plain.yaml: %v", err)
	}

	target, err = env.ResolveTarget("secrets.sops.yaml", repo)
	if err != nil {
		t.Fatalf("ResolveTarget: %v", err)
	}
	t.Cleanup(target.Close)
	return repo, target
}

// testClient builds a Client over the REAL sensitive runner, so these tests
// drive actual subprocesses rather than a fake. Pointing the editor at a
// built forgectl is buildForgectl's job, not this one.
func testClient(t *testing.T) *Client {
	t.Helper()
	return NewClient(fcexec.NewOSSensitiveRunner())
}

// buildForgectl compiles the module's main package to a temp path and points
// this process's os.Executable at it, so selfEditorCommand resolves to a real
// forgectl carrying this tree's __sops-edit.
func buildForgectl(t *testing.T) {
	t.Helper()
	// Gated like the tool checks: under FORGECTL_REQUIRE_SOPS_INTEGRATION a
	// missing toolchain is a failure, not a skip. Without this, the gate had a
	// hole — every test in this file would have skipped silently on a runner
	// with no `go` on PATH while the gate reported nothing wrong.
	if _, err := exec.LookPath("go"); err != nil {
		if os.Getenv("FORGECTL_REQUIRE_SOPS_INTEGRATION") == "1" {
			t.Fatalf("go is not on PATH and FORGECTL_REQUIRE_SOPS_INTEGRATION=1: %v", err)
		}
		t.Skipf("go is not on PATH: %v", err)
	}
	dir, err := os.MkdirTemp("", "fcbin")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	bin := filepath.Join(dir, "forgectl")
	// The module root is two levels up from internal/sops.
	out, err := exec.CommandContext(t.Context(), "go", "build", "-o", bin, "../..").CombinedOutput() //nolint:gosec // G204: a fixed tool name with arguments this test constructed
	if err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	prev := executablePath
	executablePath = func() (string, error) { return bin, nil }
	t.Cleanup(func() { executablePath = prev })
}

func TestIntegration_RoundTripAndDiffShape(t *testing.T) {
	_, target := sopsFixture(t)
	buildForgectl(t)

	before, err := os.ReadFile(target.Abs()) //nolint:gosec // G304: a fixture this test created
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	const value = "a-new-value-not-a-real-secret"
	outcome, err := testClient(t).SetValue(context.Background(), target, "agentgateway.llm_key_hermes", value)
	if err != nil {
		t.Fatalf("SetValue: %v", err)
	}
	if outcome != OutcomeReplaced {
		t.Errorf("outcome = %v, want replaced", outcome)
	}

	after, err := os.ReadFile(target.Abs()) //nolint:gosec // G304: a fixture this test created
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	// The value must not appear in cleartext anywhere.
	if bytes.Contains(after, []byte(value)) {
		t.Error("the value appears in cleartext in the encrypted file")
	}

	// A round-trip through sops proves what landed.
	landed := extractValue(t, target.Abs(), `["agentgateway"]["llm_key_hermes"]`)
	if landed != value {
		t.Errorf("decrypted = %q, want %q", landed, value)
	}

	// Untouched values keep byte-identical ciphertext, which is the property
	// that makes a one-key write reviewable.
	beforeLines := lineMap(before)
	afterLines := lineMap(after)
	for _, key := range []string{"other_key", "a"} {
		if beforeLines[key] == "" {
			t.Fatalf("fixture has no %q line to compare", key)
		}
		if beforeLines[key] != afterLines[key] {
			t.Errorf("the untouched %q line changed:\n before %q\n after  %q", key, beforeLines[key], afterLines[key])
		}
	}

	// The changed-line count: one content line plus sops' lastmodified and
	// mac. Counted directly rather than through git, so the test needs no
	// repository history.
	changed := 0
	for key, line := range afterLines {
		if beforeLines[key] != line {
			changed++
		}
	}
	if changed != 3 {
		t.Errorf("%d lines changed, want 3 (the value plus sops' lastmodified and mac): %v", changed, changedKeys(beforeLines, afterLines))
	}

}

// TestIntegration_ExtractWritesNoTrailingNewline pins the measurement the
// driver's byte-exact comparison depends on. If sops ever started appending a
// terminator, the comparison would fail for every value and the driver would
// restore every write — so this is the assertion that explains why there is
// no strip.
// TestIntegration_EditorOverrideTook proves no real editor was involved, which
// the test plan claimed and nothing asserted.
//
// It matters because the whole design rests on sops running OUR editor. If the
// override silently failed, sops would fall back to $EDITOR or vi — and on a
// CI runner with neither, or with a non-interactive one, the run could still
// look like a pass for the wrong reason. Pointing the override at a script
// that records its invocation is the only way to see the difference.
func TestIntegration_EditorOverrideTook(t *testing.T) {
	_, target := sopsFixture(t)

	marker := filepath.Join(t.TempDir(), "invoked")
	script := filepath.Join(t.TempDir(), "fake-editor.sh")
	body := "#!/bin/sh\necho \"$1\" > " + marker + "\nexit 1\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil { //nolint:gosec // G306: a script this test must be able to execute
		t.Fatalf("WriteFile: %v", err)
	}

	prev := executablePath
	// selfEditorCommand appends " __sops-edit", which the script ignores.
	executablePath = func() (string, error) { return script, nil }
	t.Cleanup(func() { executablePath = prev })

	// Exits 1, so the driver restores and refuses — that is expected. What is
	// under test is that the script ran at all.
	_, err := testClient(t).SetValue(context.Background(), target, "agentgateway.llm_key_hermes", "v")
	if err == nil {
		t.Fatal("SetValue succeeded with an editor that exits 1, want a refusal")
	}

	recorded, readErr := os.ReadFile(marker) //nolint:gosec // G304: a path this test created
	if readErr != nil {
		t.Fatalf("the editor override did not run — sops used something else: %v", readErr)
	}
	// sops passes the decrypted temp file as the only argument.
	if handed := strings.TrimSpace(string(recorded)); handed == "" {
		t.Error("the editor ran with no argument, want the decrypted temp path")
	} else if handed == target.Abs() {
		t.Errorf("the editor was handed the ENCRYPTED file (%s), want sops' decrypted temp copy", handed)
	}
}

func TestIntegration_ExtractWritesNoTrailingNewline(t *testing.T) {
	_, target := sopsFixture(t)

	out := filepath.Join(t.TempDir(), "landed")
	run := exec.CommandContext(t.Context(), "sops", "--decrypt", "--extract", `["agentgateway"]["llm_key_hermes"]`, "--output", out, target.Abs()) //nolint:gosec // G204: a fixed tool name with arguments this test constructed
	if combined, err := run.CombinedOutput(); err != nil {
		t.Fatalf("sops --extract: %v\n%s", err, combined)
	}
	got, err := os.ReadFile(out) //nolint:gosec // G304: a path this test created
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if want := "seedvalue"; string(got) != want {
		t.Errorf("extract wrote %q, want exactly %q with no terminator", got, want)
	}
}

func TestIntegration_IdempotentReSetReportsSuccess(t *testing.T) {
	_, target := sopsFixture(t)
	buildForgectl(t)

	const value = "the-same-value-twice"
	client := testClient(t)
	if _, err := client.SetValue(context.Background(), target, "agentgateway.llm_key_hermes", value); err != nil {
		t.Fatalf("first SetValue: %v", err)
	}

	// The second write hands sops identical bytes, so sops exits 200 with
	// "File has not changed, exiting." That is success — the key holds the
	// requested value — and reporting it as a failure would break every
	// idempotent re-run.
	outcome, err := client.SetValue(context.Background(), target, "agentgateway.llm_key_hermes", value)
	if err != nil {
		t.Fatalf("second SetValue: %v — the rc=200 unchanged case must report success", err)
	}
	if outcome == OutcomeUnspecified {
		t.Error("outcome is unspecified on the unchanged path")
	}
	if landed := extractValue(t, target.Abs(), `["agentgateway"]["llm_key_hermes"]`); landed != value {
		t.Errorf("decrypted = %q, want %q", landed, value)
	}
}

func TestIntegration_RefusalsLeaveTheFileByteIdentical(t *testing.T) {
	// Gated at the PARENT, not only inside each subtest's fixture. Without
	// this the parent reports PASS on a machine with no sops while every
	// subtest skips — a green that reached nothing, and under the CI gate a
	// green sitting next to its own siblings' failures.
	requireTools(t)

	cases := []struct {
		name    string
		path    string
		wantMsg string
	}{
		{
			name:    "a missing block",
			path:    "nosuchblock.key",
			wantMsg: "no block",
		},
		{
			name: "a path the file would store in cleartext",
			// The fixture's creation rule sets unencrypted_suffix, so sops
			// would write this key in the clear beside its encrypted
			// siblings — and a decrypt round-trip would pass.
			path:    "agentgateway.something_unencrypted",
			wantMsg: "stored in the clear",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, target := sopsFixture(t)
			buildForgectl(t)

			before, err := os.ReadFile(target.Abs()) //nolint:gosec // G304: a fixture this test created
			if err != nil {
				t.Fatalf("ReadFile: %v", err)
			}

			const sentinel = "s3ntinel-VALUE-77x"
			_, err = testClient(t).SetValue(context.Background(), target, c.path, sentinel)
			if err == nil {
				t.Fatal("SetValue returned nil error, want a refusal")
			}
			if !strings.Contains(err.Error(), c.wantMsg) {
				t.Errorf("error = %q, want it to mention %q", err.Error(), c.wantMsg)
			}
			if strings.Contains(err.Error(), sentinel) {
				t.Errorf("error %q echoed the value", err.Error())
			}

			after, err := os.ReadFile(target.Abs()) //nolint:gosec // G304: a fixture this test created
			if err != nil {
				t.Fatalf("ReadFile: %v", err)
			}
			if !bytes.Equal(before, after) {
				t.Error("the file changed on a refusal, want it byte-identical")
			}
			// And no work directory was left behind beside it.
			assertNoWorkDirLeft(t, filepath.Dir(target.Abs()))
		})
	}
}

func TestIntegration_NonSOPSFileRefused(t *testing.T) {
	repo, _ := sopsFixture(t)

	plain := filepath.Join(repo, "secrets.yaml")
	if err := os.WriteFile(plain, []byte("block:\n    key: value\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	target, err := env.ResolveTarget("secrets.yaml", repo)
	if err != nil {
		t.Fatalf("ResolveTarget: %v", err)
	}
	defer target.Close()

	// The name passes the allowlist; the CONTENT is what refuses. That is the
	// narrowing the two checks together provide.
	_, err = testClient(t).SetValue(context.Background(), target, "block.key", "v")
	if err == nil {
		t.Fatal("SetValue against a plain YAML file returned nil error, want a refusal")
	}
	if !strings.Contains(err.Error(), "not a SOPS document") {
		t.Errorf("error = %q, want it to name the missing sops: block", err.Error())
	}
}

// extractValue decrypts one value through the real sops binary.
func extractValue(t *testing.T, file, address string) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "extracted")
	run := exec.CommandContext(t.Context(), "sops", "--decrypt", "--extract", address, "--output", out, file) //nolint:gosec // G204: a fixed tool name with arguments this test constructed
	if combined, err := run.CombinedOutput(); err != nil {
		t.Fatalf("sops --extract: %v\n%s", err, combined)
	}
	got, err := os.ReadFile(out) //nolint:gosec // G304: a path this test created
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	return string(got)
}

// lineMap indexes a document's `key: value` lines by key, so two versions can
// be compared per key rather than by diff position.
func lineMap(doc []byte) map[string]string {
	out := map[string]string{}
	for _, raw := range strings.Split(string(doc), "\n") {
		trimmed := strings.TrimSpace(raw)
		key, _, found := strings.Cut(trimmed, ":")
		if !found || key == "" {
			continue
		}
		out[key] = trimmed
	}
	return out
}

func changedKeys(before, after map[string]string) []string {
	var keys []string
	for key, line := range after {
		if before[key] != line {
			keys = append(keys, key)
		}
	}
	return keys
}

// assertNoWorkDirLeft proves the deferred cleanup ran: a work directory left
// beside the target would hold the staged value at 0600, which is a durable
// secret on disk nobody asked for.
func assertNoWorkDirLeft(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".forgectl-sops-") {
			t.Errorf("a work directory was left behind: %s", e.Name())
		}
	}
}
