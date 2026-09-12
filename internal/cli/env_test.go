package cli

// Test plan for env.go
//
// newEnvCmd / newEnvCmdForClient (Classification: API handler / cobra command)
//
// keys
//   [x] Happy: names only, one per line, first-seen order
//   [x] Happy: a malformed line is skipped and noted on stderr
//   [x] Happy: an empty file produces empty stdout, exit 0
//   [x] Unhappy: a missing file errors
//
// set
//   [x] Happy: reads a value from piped stdin
//   [x] Happy: strips exactly one trailing "\n" / "\r\n"
//   [x] Happy: --clipboard sources the value from the clipboard
//   [x] Happy: --clipboard wins even when stdin is also piped
//   [x] Happy: an interactive TTY prompts via the no-echo seam
//   [x] Happy: a brand-new file lands at 0600
//   [x] Unhappy: empty stdin is refused
//   [x] Unhappy: a hostile argv key (`KEY=VALUE` shape) is refused with no
//       argument echoed anywhere in the error
//   [x] Unhappy: a duplicate key in the file is refused
//
// get
//   [x] Happy: --clipboard copies the value; stdout carries only the
//       confirmation line, never the value
//   [x] Unhappy: --clipboard is required — absent, nothing is printed or
//       copied
//   [x] Unhappy: a missing key errors
//   [x] Unhappy: a `get VALUE`-shaped mistake echoes nothing
//
// check (forgectl#104/#105: typed exit codes + --json)
//   [x] Happy: no drift → exit 0
//   [x] Unhappy: a missing key → exit 1 (drift)
//   [x] Unhappy: an extra-only key → exit 1 (drift), reported under extra:
//   [x] Unhappy: a missing --file → exit 2, distinct from drift
//   [x] Unhappy: a missing --example file → exit 2, distinct from drift
//   [x] Happy: --file and --example compose
//   [x] Happy: --json emits {"missing":[...],"extra":[...]} on a clean check
//   [x] Happy: --json reports missing/extra names, still exit 1
//   [x] Happy: --json arrays are [] (never null) on a clean check
//
// redact
//   [x] Happy: masks values; a multiline PEM-shaped fixture leaves no body
//       line
//   [x] Unhappy: a missing file errors
//
// containment
//   [x] Unhappy: a --file escaping the repo is refused (representative)
//
// Every value-bearing test also runs assertNoSecretInOutput (stdout,
// stderr, AND a captured slog default) against a sentinel value.

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	clippkg "github.com/cameronsjo/forgectl/internal/clip"
	envpkg "github.com/cameronsjo/forgectl/internal/env"
	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/module"
	sopspkg "github.com/cameronsjo/forgectl/internal/sops"
	"github.com/cameronsjo/forgectl/internal/theme"
)

// initEnvGitRepo makes dir a real (enough) git repo for env.Locate's
// walk-up — mirrors internal/env's identically-named test helper.
func initEnvGitRepo(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatalf("MkdirAll .git: %v", err)
	}
}

// envFixture builds a *env.Client wired over a FakeRunner for CLI tests —
// the FakeRunner's RunFunc is left for the test to set before it needs a
// specific pbcopy/pbpaste behavior.
func envFixture() (*envpkg.Client, *exec.FakeRunner) {
	fake := &exec.FakeRunner{}
	client := envpkg.NewClient(clippkg.New(fake, clippkg.WithGOOS("darwin")))
	return client, fake
}

// newEnvTestCmd builds the env command tree, supplying the two collaborators
// the --sops route needs.
//
// Both are real objects over fakes rather than nil. A nil sops client would
// make any test that accidentally reached the --sops route panic, and a panic
// says "this test is broken" where a failed assertion says "this code is
// wrong" — the distinction matters for the 42 tests here that are about the
// .env route and should never touch sops at all.
func newEnvTestCmd(client *envpkg.Client, th theme.Theme) *cobra.Command {
	cmd, _ := newEnvTestCmdWithClip(client, th)
	return cmd
}

// newEnvTestCmdWithClip is newEnvTestCmd plus a handle on the clipboard's own
// FakeRunner, for the tests that drive the --sops route's clipboard source.
// That route reads the clipboard DIRECTLY rather than through
// env.Client.SetFromClipboard, because SetFromClipboard runs the whole .env
// pipeline and would append a plaintext KEY=value line to an encrypted file —
// so it needs its own reachable fake.
func newEnvTestCmdWithClip(client *envpkg.Client, th theme.Theme) (*cobra.Command, *exec.FakeRunner) {
	clipFake := &exec.FakeRunner{}
	cmd := newEnvCmdForClient(
		client,
		sopspkg.NewClient(&exec.FakeSensitiveRunner{}),
		// WithSensitive() matches what newEnvCmd builds in production. Without
		// it the clipboard layer logs the pasted byte count, and a length is
		// itself signal about a secret — it distinguishes key types and tracks
		// rotations, which is the same reason `redact` masks to a fixed ****.
		// A fixture that omits it could not catch a regression that dropped it.
		clippkg.New(clipFake, clippkg.WithGOOS("darwin"), clippkg.WithSensitive()),
		th,
	)
	return cmd, clipFake
}

// forceNonTTY overrides the isTerminal seam to false (the piped-stdin
// branch) for the duration of the test, restoring it via t.Cleanup.
func forceNonTTY(t *testing.T) {
	t.Helper()
	prev := isTerminal
	isTerminal = func() bool { return false }
	t.Cleanup(func() { isTerminal = prev })
}

// forceTTYWithPassword overrides both the isTerminal and readPassword
// seams so `set`'s interactive no-echo branch runs without a real tty,
// returning the canned value.
func forceTTYWithPassword(t *testing.T, value string, err error) {
	t.Helper()
	prevTerm, prevPW := isTerminal, readPassword
	isTerminal = func() bool { return true }
	readPassword = func() (string, error) { return value, err }
	t.Cleanup(func() {
		isTerminal = prevTerm
		readPassword = prevPW
	})
}

// captureSlog installs a scratch slog default logger writing to a buffer,
// restored via t.Cleanup — the CLI-side half of the "no secret ever
// reaches slog" guarantee that assertNoSecretInOutput checks below.
func captureSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// assertNoSecretInOutput fails the test if sentinel appears anywhere in
// outputs — typically stdout, stderr, and a captureSlog buffer.
func assertNoSecretInOutput(t *testing.T, sentinel string, outputs ...string) {
	t.Helper()
	for _, out := range outputs {
		if strings.Contains(out, sentinel) {
			t.Fatalf("output leaked sentinel value %q: %q", sentinel, out)
		}
	}
}

// --- keys ---

func TestEnvKeysCmd_NamesOnly(t *testing.T) {
	repo := t.TempDir()
	initEnvGitRepo(t, repo)
	if err := os.WriteFile(filepath.Join(repo, ".env"), []byte("A=1\nB=2\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	t.Chdir(repo)

	client, _ := envFixture()
	cmd := newEnvTestCmd(client, theme.Theme{})
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"keys"})

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := "A\nB\n"; stdout.String() != want {
		t.Errorf("stdout = %q, want %q", stdout.String(), want)
	}
}

func TestEnvKeysCmd_SkipsMalformedNote(t *testing.T) {
	repo := t.TempDir()
	initEnvGitRepo(t, repo)
	if err := os.WriteFile(filepath.Join(repo, ".env"), []byte("not-a-line\nA=1\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	t.Chdir(repo)

	client, _ := envFixture()
	cmd := newEnvTestCmd(client, theme.Theme{})
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"keys"})

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := "A\n"; stdout.String() != want {
		t.Errorf("stdout = %q, want %q", stdout.String(), want)
	}
	if !strings.Contains(stderr.String(), "skipped 1 malformed line(s)") {
		t.Errorf("stderr = %q, want it to note the skipped malformed line", stderr.String())
	}
}

func TestEnvKeysCmd_EmptyFile_EmptyStdout(t *testing.T) {
	repo := t.TempDir()
	initEnvGitRepo(t, repo)
	if err := os.WriteFile(filepath.Join(repo, ".env"), []byte{}, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	t.Chdir(repo)

	client, _ := envFixture()
	cmd := newEnvTestCmd(client, theme.Theme{})
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"keys"})

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stdout.String() != "" {
		t.Errorf("stdout = %q, want empty", stdout.String())
	}
}

func TestEnvKeysCmd_MissingFile_Errors(t *testing.T) {
	repo := t.TempDir()
	initEnvGitRepo(t, repo)
	t.Chdir(repo)

	client, _ := envFixture()
	cmd := newEnvTestCmd(client, theme.Theme{})
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"keys"})

	if err := cmd.ExecuteContext(context.Background()); err == nil {
		t.Fatal("keys against a missing file returned nil error, want a refusal")
	}
}

// --- set ---

func TestEnvSetCmd_FromPipedStdin(t *testing.T) {
	repo := t.TempDir()
	initEnvGitRepo(t, repo)
	t.Chdir(repo)
	forceNonTTY(t)
	slogBuf := captureSlog(t)

	const sentinel = "s3ntinel-VALUE-77x"
	client, _ := envFixture()
	cmd := newEnvTestCmd(client, theme.Theme{})
	var stdout, stderr bytes.Buffer
	cmd.SetIn(strings.NewReader(sentinel + "\n"))
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"set", "KEY"})

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(repo, ".env"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if want := "KEY=" + sentinel + "\n"; string(got) != want {
		t.Errorf("file content = %q, want %q", got, want)
	}
	assertNoSecretInOutput(t, sentinel, stdout.String(), stderr.String(), slogBuf.String())
}

func TestEnvSetCmd_StripsTrailingNewline(t *testing.T) {
	cases := map[string]string{
		"LF":   "value1\n",
		"CRLF": "value1\r\n",
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			repo := t.TempDir()
			initEnvGitRepo(t, repo)
			t.Chdir(repo)
			forceNonTTY(t)

			client, _ := envFixture()
			cmd := newEnvTestCmd(client, theme.Theme{})
			cmd.SetIn(strings.NewReader(input))
			cmd.SetOut(new(bytes.Buffer))
			cmd.SetErr(new(bytes.Buffer))
			cmd.SetArgs([]string{"set", "KEY"})

			if err := cmd.ExecuteContext(context.Background()); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			got, err := os.ReadFile(filepath.Join(repo, ".env"))
			if err != nil {
				t.Fatalf("ReadFile: %v", err)
			}
			if want := "KEY=value1\n"; string(got) != want {
				t.Errorf("file content = %q, want %q", got, want)
			}
		})
	}
}

func TestEnvSetCmd_Clipboard(t *testing.T) {
	repo := t.TempDir()
	initEnvGitRepo(t, repo)
	t.Chdir(repo)

	const sentinel = "s3ntinel-VALUE-77x"
	client, fake := envFixture()
	fake.RunFunc = func(name string, _ []string) (string, error) {
		if name == "pbpaste" {
			return sentinel, nil
		}
		return "", nil
	}
	cmd := newEnvTestCmd(client, theme.Theme{})
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"set", "KEY", "--clipboard"})

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(repo, ".env"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if want := "KEY=" + sentinel + "\n"; string(got) != want {
		t.Errorf("file content = %q, want %q", got, want)
	}
}

func TestEnvSetCmd_ClipboardWinsOverPipedStdin(t *testing.T) {
	repo := t.TempDir()
	initEnvGitRepo(t, repo)
	t.Chdir(repo)
	forceNonTTY(t) // stdin is also "piped" — clipboard must still win

	const fromClipboard = "from-clipboard-value"
	client, fake := envFixture()
	fake.RunFunc = func(name string, _ []string) (string, error) {
		if name == "pbpaste" {
			return fromClipboard, nil
		}
		return "", nil
	}
	cmd := newEnvTestCmd(client, theme.Theme{})
	cmd.SetIn(strings.NewReader("from-stdin-value\n"))
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"set", "KEY", "--clipboard"})

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(repo, ".env"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if want := "KEY=" + fromClipboard + "\n"; string(got) != want {
		t.Errorf("file content = %q, want the clipboard value to win, got %q", got, want)
	}
}

func TestEnvSetCmd_TTYPrompt_ViaSeam(t *testing.T) {
	repo := t.TempDir()
	initEnvGitRepo(t, repo)
	t.Chdir(repo)
	const sentinel = "s3ntinel-VALUE-77x"
	forceTTYWithPassword(t, sentinel, nil)

	client, _ := envFixture()
	cmd := newEnvTestCmd(client, theme.Theme{})
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"set", "KEY"})

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(repo, ".env"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if want := "KEY=" + sentinel + "\n"; string(got) != want {
		t.Errorf("file content = %q, want %q", got, want)
	}
	if !strings.Contains(stderr.String(), "Value: ") {
		t.Errorf("stderr = %q, want the no-echo prompt text", stderr.String())
	}
	// The prompt must not name the key. A prompt precedes any write and is
	// abandoned on Ctrl-C, so it never earns the right to echo its argument
	// — and `env set sk_live_…` is an ordinary typo that would otherwise put
	// a live credential in the transcript with nothing written.
	//
	// Scoped to the prompt line rather than all of stderr: a bare
	// Contains(stderr, "KEY") would break on any future stderr line that
	// legitimately names a key, which is a test about the prompt failing for
	// an unrelated reason.
	if strings.Contains(stderr.String(), "Value for ") {
		t.Errorf("stderr = %q, want the prompt to name no key", stderr.String())
	}
	assertNoSecretInOutput(t, sentinel, stdout.String(), stderr.String())
}

func TestEnvSetCmd_NewFile_0600(t *testing.T) {
	repo := t.TempDir()
	initEnvGitRepo(t, repo)
	t.Chdir(repo)
	forceNonTTY(t)

	client, _ := envFixture()
	cmd := newEnvTestCmd(client, theme.Theme{})
	cmd.SetIn(strings.NewReader("value1\n"))
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"set", "KEY"})

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	fi, err := os.Stat(filepath.Join(repo, ".env"))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %o, want 0600", fi.Mode().Perm())
	}
}

func TestEnvSetCmd_EmptyStdin_Refused(t *testing.T) {
	repo := t.TempDir()
	initEnvGitRepo(t, repo)
	t.Chdir(repo)
	forceNonTTY(t)

	client, _ := envFixture()
	cmd := newEnvTestCmd(client, theme.Theme{})
	cmd.SetIn(strings.NewReader(""))
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"set", "KEY"})

	if err := cmd.ExecuteContext(context.Background()); err == nil {
		t.Fatal("set with empty stdin returned nil error, want a refusal")
	}
	if _, statErr := os.Stat(filepath.Join(repo, ".env")); !os.IsNotExist(statErr) {
		t.Error("a file was created despite the empty-value refusal")
	}
}

func TestEnvSetCmd_HostileArgvKey_RefusedNoArgumentEcho(t *testing.T) {
	repo := t.TempDir()
	initEnvGitRepo(t, repo)
	t.Chdir(repo)
	forceNonTTY(t)
	slogBuf := captureSlog(t)

	const hostileValue = "SENTINEL_VALUE_should_never_appear"
	hostileKey := "KEY=" + hostileValue

	client, _ := envFixture()
	cmd := newEnvTestCmd(client, theme.Theme{})
	// Even with stdin piped, the key check must fire before it's read.
	cmd.SetIn(strings.NewReader("unrelated\n"))
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"set", hostileKey})

	err := cmd.ExecuteContext(context.Background())
	if err == nil {
		t.Fatal("set with a KEY=VALUE-shaped argument returned nil error, want a refusal")
	}
	if strings.Contains(err.Error(), hostileValue) {
		t.Errorf("error %q echoes the rejected value", err.Error())
	}
	if strings.Contains(err.Error(), hostileKey) {
		t.Errorf("error %q echoes the rejected argument", err.Error())
	}
	if _, statErr := os.Stat(filepath.Join(repo, ".env")); !os.IsNotExist(statErr) {
		t.Error("a file was created despite the hostile-key refusal")
	}
	assertNoSecretInOutput(t, hostileValue, stdout.String(), stderr.String(), slogBuf.String(), err.Error())
}

func TestEnvSetCmd_DuplicateKey_Refused(t *testing.T) {
	repo := t.TempDir()
	initEnvGitRepo(t, repo)
	if err := os.WriteFile(filepath.Join(repo, ".env"), []byte("KEY=1\nKEY=2\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	t.Chdir(repo)
	forceNonTTY(t)

	client, _ := envFixture()
	cmd := newEnvTestCmd(client, theme.Theme{})
	cmd.SetIn(strings.NewReader("3\n"))
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"set", "KEY"})

	if err := cmd.ExecuteContext(context.Background()); err == nil {
		t.Fatal("set against a duplicate key returned nil error, want a refusal")
	}
}

func TestEnvSetCmd_EmptyStdin_KeyShapedSecretArg_NoTokenEcho(t *testing.T) {
	// The sibling of TestEnvGetCmd_KeyShapedSecret_RefusedNoArgumentEcho:
	// a plausible real secret pasted into the KEY argument slot
	// (sk_live_… is a valid ValidKey shape) with empty stdin. Nothing is
	// ever written on this path, so the argument never becomes a key in
	// any file — the error must not name it either.
	repo := t.TempDir()
	initEnvGitRepo(t, repo)
	t.Chdir(repo)
	forceNonTTY(t)
	slogBuf := captureSlog(t)

	const keyShapedSecret = "SEKRIT_valuelikelooking_ab12cd34"
	client, _ := envFixture()
	cmd := newEnvTestCmd(client, theme.Theme{})
	cmd.SetIn(strings.NewReader(""))
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"set", keyShapedSecret})

	err := cmd.ExecuteContext(context.Background())
	if err == nil {
		t.Fatal("set with empty stdin and a key-shaped-secret argument returned nil error, want a refusal")
	}
	if strings.Contains(err.Error(), keyShapedSecret) {
		t.Errorf("error %q echoes the argument — nothing was written, so it may not be a key at all", err.Error())
	}
	if _, statErr := os.Stat(filepath.Join(repo, ".env")); !os.IsNotExist(statErr) {
		t.Error("a file was created despite the empty-value refusal")
	}
	assertNoSecretInOutput(t, keyShapedSecret, stdout.String(), stderr.String(), slogBuf.String(), err.Error())
}

// TestNewEnvCmd_ClipWiredWithSensitive proves the PRODUCTION wiring
// (newEnvCmd, not the test-only newEnvCmdForClient) constructs its clip
// client with clippkg.WithSensitive() — envFixture's raw client bypasses
// this wiring entirely, so it can't stand in for this assertion.
func TestNewEnvCmd_ClipWiredWithSensitive(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("clip.Client's Darwin-only guard would fail this test on a non-macOS runner regardless of the wiring under test")
	}
	repo := t.TempDir()
	initEnvGitRepo(t, repo)
	const sentinel = "s3ntinel-VALUE-77x"
	if err := os.WriteFile(filepath.Join(repo, ".env"), []byte("KEY="+sentinel+"\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	t.Chdir(repo)
	slogBuf := captureSlog(t)

	fake := &exec.FakeRunner{}
	cmd := newEnvCmd(module.Deps{Runner: fake})
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"get", "KEY", "--clipboard"})

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(slogBuf.String(), "bytes=") {
		t.Errorf("slog output = %q, want no byte-length field — newEnvCmd must wire clip.WithSensitive()", slogBuf.String())
	}
	assertNoSecretInOutput(t, sentinel, slogBuf.String())
}

// --- get ---

func TestEnvGetCmd_Clipboard_ConfirmationOnly(t *testing.T) {
	repo := t.TempDir()
	initEnvGitRepo(t, repo)
	const sentinel = "s3ntinel-VALUE-77x"
	if err := os.WriteFile(filepath.Join(repo, ".env"), []byte("KEY="+sentinel+"\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	t.Chdir(repo)
	slogBuf := captureSlog(t)

	client, fake := envFixture()
	cmd := newEnvTestCmd(client, theme.Theme{})
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"get", "KEY", "--clipboard"})

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := "copied KEY to clipboard\n"; stdout.String() != want {
		t.Errorf("stdout = %q, want %q", stdout.String(), want)
	}
	call := fake.Last()
	if call.Name != "pbcopy" || call.Input != sentinel {
		t.Errorf("pbcopy call = %+v, want Input %q", call, sentinel)
	}
	assertNoSecretInOutput(t, sentinel, stdout.String(), stderr.String(), slogBuf.String())
}

func TestEnvGetCmd_RequiresClipboard(t *testing.T) {
	repo := t.TempDir()
	initEnvGitRepo(t, repo)
	const sentinel = "s3ntinel-VALUE-77x"
	if err := os.WriteFile(filepath.Join(repo, ".env"), []byte("KEY="+sentinel+"\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	t.Chdir(repo)

	client, fake := envFixture()
	cmd := newEnvTestCmd(client, theme.Theme{})
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"get", "KEY"})

	if err := cmd.ExecuteContext(context.Background()); err == nil {
		t.Fatal("get without --clipboard returned nil error, want a refusal")
	}
	if stdout.String() != "" {
		t.Errorf("stdout = %q, want empty (nothing printed)", stdout.String())
	}
	if len(fake.Calls) != 0 {
		t.Errorf("clipboard was touched %d times, want 0", len(fake.Calls))
	}
}

func TestEnvGetCmd_MissingKey_Errors(t *testing.T) {
	repo := t.TempDir()
	initEnvGitRepo(t, repo)
	if err := os.WriteFile(filepath.Join(repo, ".env"), []byte("OTHER=1\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	t.Chdir(repo)

	client, _ := envFixture()
	cmd := newEnvTestCmd(client, theme.Theme{})
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"get", "MISSING", "--clipboard"})

	if err := cmd.ExecuteContext(context.Background()); err == nil {
		t.Fatal("get with a missing key returned nil error, want a refusal")
	}
}

func TestEnvGetCmd_HostileArgvValue_RefusedNoArgumentEcho(t *testing.T) {
	repo := t.TempDir()
	initEnvGitRepo(t, repo)
	t.Chdir(repo)

	const hostileValue = "SENTINEL_should_never_appear!!"
	client, _ := envFixture()
	cmd := newEnvTestCmd(client, theme.Theme{})
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"get", hostileValue, "--clipboard"})

	err := cmd.ExecuteContext(context.Background())
	if err == nil {
		t.Fatal("get with a hostile value-shaped argument returned nil error, want a refusal")
	}
	if strings.Contains(err.Error(), hostileValue) {
		t.Errorf("error %q echoes the rejected argument", err.Error())
	}
	assertNoSecretInOutput(t, hostileValue, stdout.String(), stderr.String(), err.Error())
}

// TestEnvGetCmd_KeyShapedSecret_RefusedNoArgumentEcho is the sibling of the
// test above, and the sharper of the two: the sentinel here is a VALID key
// shape, so it sails past ValidKey and reaches the not-found branch instead
// of the rule-only refusal. Plenty of real API keys are pure
// [A-Za-z_][A-Za-z0-9_]* — a Stripe-style sk_live_… among them — so a user
// pasting a value into the key slot lands exactly here. The not-found error
// must name the file, never the token that missed.
func TestEnvGetCmd_KeyShapedSecret_RefusedNoArgumentEcho(t *testing.T) {
	repo := t.TempDir()
	initEnvGitRepo(t, repo)
	if err := os.WriteFile(filepath.Join(repo, ".env"), []byte("OTHER=1\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	t.Chdir(repo)

	const keyShapedSecret = "sk_live_S3NTINEL_valid_key_shape"
	client, _ := envFixture()
	cmd := newEnvTestCmd(client, theme.Theme{})
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"get", keyShapedSecret, "--clipboard"})

	err := cmd.ExecuteContext(context.Background())
	if err == nil {
		t.Fatal("get with a key-shaped secret returned nil error, want a refusal")
	}
	if strings.Contains(err.Error(), keyShapedSecret) {
		t.Errorf("error %q echoes the missing key — a value pasted into the key slot would leak", err.Error())
	}
	assertNoSecretInOutput(t, keyShapedSecret, stdout.String(), stderr.String(), err.Error())
}

// --- check ---

func TestEnvCheckCmd_NoDrift_ExitZero(t *testing.T) {
	repo := t.TempDir()
	initEnvGitRepo(t, repo)
	if err := os.WriteFile(filepath.Join(repo, ".env"), []byte("A=1\nB=2\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".env.example"), []byte("A=\nB=\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	t.Chdir(repo)

	client, _ := envFixture()
	cmd := newEnvTestCmd(client, theme.Theme{})
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"check"})

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// A clean check writes NOTHING to stdout: any stdout at all means
	// drift, so a caller can gate on emptiness without parsing. An empty
	// "extra:" header printed under a clean run reads as a truncated list
	// rather than as "there are none".
	if stdout.String() != "" {
		t.Errorf("stdout = %q, want empty on a clean check", stdout.String())
	}
	if !strings.Contains(stderr.String(), "matches") {
		t.Errorf("stderr = %q, want a match confirmation", stderr.String())
	}
}

func TestEnvCheckCmd_ExtraOnly_PrintsOnlyExtraSection(t *testing.T) {
	repo := t.TempDir()
	initEnvGitRepo(t, repo)
	if err := os.WriteFile(filepath.Join(repo, ".env"), []byte("A=1\nLOCAL_ONLY=2\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".env.example"), []byte("A=\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	t.Chdir(repo)

	client, _ := envFixture()
	cmd := newEnvTestCmd(client, theme.Theme{})
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"check"})

	// forgectl#104: drift is missing AND/OR extra — an extra-only key is
	// still reported keys-only, but it now fails the check (exit 1) too.
	err := cmd.ExecuteContext(context.Background())
	if err == nil {
		t.Fatal("extra-only drift returned nil error, want exit 1 (drift)")
	}
	if code := ExitCode(err); code != 1 {
		t.Errorf("ExitCode(err) = %d, want 1 (drift, not an absent-file failure)", code)
	}
	if strings.Contains(stdout.String(), "missing:") {
		t.Errorf("stdout = %q, want no missing: section when nothing is missing", stdout.String())
	}
	if !strings.Contains(stdout.String(), "extra:") || !strings.Contains(stdout.String(), "LOCAL_ONLY") {
		t.Errorf("stdout = %q, want the extra: section naming LOCAL_ONLY", stdout.String())
	}
}

func TestEnvCheckCmd_MissingKey_ExitOne(t *testing.T) {
	repo := t.TempDir()
	initEnvGitRepo(t, repo)
	if err := os.WriteFile(filepath.Join(repo, ".env"), []byte("A=1\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".env.example"), []byte("A=\nB=\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	t.Chdir(repo)

	client, _ := envFixture()
	cmd := newEnvTestCmd(client, theme.Theme{})
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"check"})

	err := cmd.ExecuteContext(context.Background())
	if err == nil {
		t.Fatal("check with a missing key returned nil error, want exit 1")
	}
	if code := ExitCode(err); code != 1 {
		t.Errorf("ExitCode(err) = %d, want 1 (drift)", code)
	}
	if !strings.Contains(stdout.String(), "  B") {
		t.Errorf("stdout = %q, want the missing key B reported under missing:", stdout.String())
	}
}

func TestEnvCheckCmd_ExtraKey_ReportedExitOne(t *testing.T) {
	repo := t.TempDir()
	initEnvGitRepo(t, repo)
	if err := os.WriteFile(filepath.Join(repo, ".env"), []byte("A=1\nLOCAL_ONLY=2\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".env.example"), []byte("A=\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	t.Chdir(repo)

	client, _ := envFixture()
	cmd := newEnvTestCmd(client, theme.Theme{})
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"check"})

	// forgectl#104: extra keys are still reported (an agent needs the
	// names), but they now count as drift — exit 1, not exit 0.
	err := cmd.ExecuteContext(context.Background())
	if err == nil {
		t.Fatal("check with an extra key returned nil error, want exit 1 (drift)")
	}
	if code := ExitCode(err); code != 1 {
		t.Errorf("ExitCode(err) = %d, want 1 (drift)", code)
	}
	if !strings.Contains(stdout.String(), "  LOCAL_ONLY") {
		t.Errorf("stdout = %q, want LOCAL_ONLY reported under extra:", stdout.String())
	}
}

func TestEnvCheckCmd_MissingExampleFile_ExitTwo(t *testing.T) {
	repo := t.TempDir()
	initEnvGitRepo(t, repo)
	if err := os.WriteFile(filepath.Join(repo, ".env"), []byte("A=1\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	t.Chdir(repo)

	client, _ := envFixture()
	cmd := newEnvTestCmd(client, theme.Theme{})
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"check"})

	err := cmd.ExecuteContext(context.Background())
	if err == nil {
		t.Fatal("check with a missing example file returned nil error, want a refusal")
	}
	if !strings.Contains(err.Error(), "example file") {
		t.Errorf("error %q, want it to name the example file distinctly from a drift failure", err.Error())
	}
	// forgectl#104: an absent file is a distinct failure class from drift —
	// exit 2, not the exit 1 a missing/extra key comparison produces.
	if code := ExitCode(err); code != 2 {
		t.Errorf("ExitCode(err) = %d, want 2 (absent file, not drift)", code)
	}
}

func TestEnvCheckCmd_MissingFile_ExitTwo(t *testing.T) {
	// The sibling of the missing-example case: --file itself absent (an
	// example present) must error too, before any drift comparison, with
	// the same distinct exit code.
	repo := t.TempDir()
	initEnvGitRepo(t, repo)
	if err := os.WriteFile(filepath.Join(repo, ".env.example"), []byte("A=\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	t.Chdir(repo)

	client, _ := envFixture()
	cmd := newEnvTestCmd(client, theme.Theme{})
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"check"})

	err := cmd.ExecuteContext(context.Background())
	if err == nil {
		t.Fatal("check with a missing --file returned nil error, want a refusal")
	}
	if code := ExitCode(err); code != 2 {
		t.Errorf("ExitCode(err) = %d, want 2 (absent file, not drift)", code)
	}
}

// checkErrorJSONWire mirrors checkErrorJSON's wire shape for decoding in
// tests without exporting the type.
type checkErrorJSONWire struct {
	Error string `json:"error"`
	Code  string `json:"code"`
	Path  string `json:"path"`
}

func TestEnvCheckCmd_JSON_MissingFile_OneStderrObject_ExitTwo(t *testing.T) {
	repo := t.TempDir()
	initEnvGitRepo(t, repo)
	if err := os.WriteFile(filepath.Join(repo, ".env.example"), []byte("A=\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	t.Chdir(repo)

	client, _ := envFixture()
	cmd := newEnvTestCmd(client, theme.Theme{})
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"check", "--json"})

	err := cmd.ExecuteContext(context.Background())
	if err == nil {
		t.Fatal("check --json with a missing --file returned nil error, want exit 2")
	}
	if code := ExitCode(err); code != 2 {
		t.Errorf("ExitCode(err) = %d, want 2", code)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want empty under --json exit 2", stdout.String())
	}

	dec := json.NewDecoder(&stderr)
	var got checkErrorJSONWire
	if decErr := dec.Decode(&got); decErr != nil {
		t.Fatalf("stderr = %q, not valid JSON: %v", stderr.String(), decErr)
	}
	if dec.More() {
		t.Fatalf("stderr carried more than one JSON value: %q", stderr.String())
	}
	if got.Error != "env file not found" {
		t.Errorf("error = %q, want %q", got.Error, "env file not found")
	}
	if got.Code != "file_not_found" {
		t.Errorf("code = %q, want %q", got.Code, "file_not_found")
	}
	if got.Path != ".env" {
		t.Errorf("path = %q, want repo-relative %q (never the resolved absolute path)", got.Path, ".env")
	}
	if strings.ContainsRune(stderr.String(), 0x1b) {
		t.Errorf("stderr contained an ESC byte: %q", stderr.String())
	}
}

func TestEnvCheckCmd_JSON_MissingExampleFile_OneStderrObject_ExitTwo(t *testing.T) {
	repo := t.TempDir()
	initEnvGitRepo(t, repo)
	if err := os.WriteFile(filepath.Join(repo, ".env"), []byte("A=1\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	t.Chdir(repo)

	client, _ := envFixture()
	cmd := newEnvTestCmd(client, theme.Theme{})
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"check", "--json"})

	err := cmd.ExecuteContext(context.Background())
	if err == nil {
		t.Fatal("check --json with a missing --example returned nil error, want exit 2")
	}
	if code := ExitCode(err); code != 2 {
		t.Errorf("ExitCode(err) = %d, want 2", code)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want empty under --json exit 2", stdout.String())
	}

	var got checkErrorJSONWire
	if decErr := json.Unmarshal(stderr.Bytes(), &got); decErr != nil {
		t.Fatalf("stderr = %q, not valid JSON: %v", stderr.String(), decErr)
	}
	if got.Path != ".env.example" {
		t.Errorf("path = %q, want repo-relative %q", got.Path, ".env.example")
	}
}

func TestEnvCheckCmd_JSON_Clean_EmptyArraysNotNull(t *testing.T) {
	repo := t.TempDir()
	initEnvGitRepo(t, repo)
	if err := os.WriteFile(filepath.Join(repo, ".env"), []byte("A=1\nB=2\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".env.example"), []byte("A=\nB=\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	t.Chdir(repo)

	client, _ := envFixture()
	cmd := newEnvTestCmd(client, theme.Theme{})
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"check", "--json"})

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var got checkJSON
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("stdout = %q, not valid JSON: %v", stdout.String(), err)
	}
	if got.Missing == nil || len(got.Missing) != 0 {
		t.Errorf("Missing = %#v, want a non-nil empty slice", got.Missing)
	}
	if got.Extra == nil || len(got.Extra) != 0 {
		t.Errorf("Extra = %#v, want a non-nil empty slice", got.Extra)
	}
	// --json suppresses the human "matches" reassurance too — a caller
	// parsing stdout as JSON must not see it interleaved with prose, and
	// nothing about a clean check needs to reach stderr either.
	if stderr.String() != "" {
		t.Errorf("stderr = %q, want empty in --json mode", stderr.String())
	}
}

func TestEnvCheckCmd_JSON_Drift_ReportsNamesAndExitsOne(t *testing.T) {
	repo := t.TempDir()
	initEnvGitRepo(t, repo)
	if err := os.WriteFile(filepath.Join(repo, ".env"), []byte("A=1\nLOCAL_ONLY=2\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".env.example"), []byte("A=\nB=\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	t.Chdir(repo)

	client, _ := envFixture()
	cmd := newEnvTestCmd(client, theme.Theme{})
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"check", "--json"})

	err := cmd.ExecuteContext(context.Background())
	if err == nil {
		t.Fatal("check --json with drift returned nil error, want exit 1")
	}
	if code := ExitCode(err); code != 1 {
		t.Errorf("ExitCode(err) = %d, want 1 (drift)", code)
	}
	// Drift is a comparison result, not the missing-file class the
	// silentCodedError stderr object exists for — nothing should land on
	// stderr here.
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want empty for a drift result under --json", stderr.String())
	}

	var got checkJSON
	if jsonErr := json.Unmarshal(stdout.Bytes(), &got); jsonErr != nil {
		t.Fatalf("stdout = %q, not valid JSON: %v", stdout.String(), jsonErr)
	}
	if len(got.Missing) != 1 || got.Missing[0] != "B" {
		t.Errorf("Missing = %#v, want [\"B\"]", got.Missing)
	}
	if len(got.Extra) != 1 || got.Extra[0] != "LOCAL_ONLY" {
		t.Errorf("Extra = %#v, want [\"LOCAL_ONLY\"]", got.Extra)
	}
}

func TestEnvCheckCmd_FileAndExampleFlagsCompose(t *testing.T) {
	repo := t.TempDir()
	initEnvGitRepo(t, repo)
	if err := os.WriteFile(filepath.Join(repo, ".env.prod"), []byte("A=1\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".env.example"), []byte("A=\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	t.Chdir(repo)

	client, _ := envFixture()
	cmd := newEnvTestCmd(client, theme.Theme{})
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"check", "--file", ".env.prod", "--example", ".env.example"})

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// --- redact ---

func TestEnvRedactCmd_MasksValues(t *testing.T) {
	repo := t.TempDir()
	initEnvGitRepo(t, repo)
	const sentinel = "s3ntinel-VALUE-77x"
	if err := os.WriteFile(filepath.Join(repo, ".env"), []byte("KEY="+sentinel+"\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	t.Chdir(repo)
	slogBuf := captureSlog(t)

	client, _ := envFixture()
	cmd := newEnvTestCmd(client, theme.Theme{})
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"redact"})

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := "KEY=****\n"; stdout.String() != want {
		t.Errorf("stdout = %q, want %q", stdout.String(), want)
	}
	assertNoSecretInOutput(t, sentinel, stdout.String(), stderr.String(), slogBuf.String())
}

// pemFixture mirrors internal/env/document_test.go's identically-named
// fixture — a CERTIFICATE (not a private key) label with placeholder
// base64-shaped content, spanning two physical lines inside one quoted
// value.
const pemFixture = "-----BEGIN CERTIFICATE-----\n" +
	"MIIBoXsomeBase64ContentHereThatSpansALineOfItsOwn\n" +
	"-----END CERTIFICATE-----"

func TestEnvRedactCmd_MultilinePEM_NoBodyLine(t *testing.T) {
	repo := t.TempDir()
	initEnvGitRepo(t, repo)
	content := "PEM_CERT=\"" + pemFixture + "\"\n"
	if err := os.WriteFile(filepath.Join(repo, ".env"), []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	t.Chdir(repo)

	client, _ := envFixture()
	cmd := newEnvTestCmd(client, theme.Theme{})
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"redact"})

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := "PEM_CERT=****\n"; stdout.String() != want {
		t.Errorf("stdout = %q, want a single masked line with no PEM body", stdout.String())
	}
	assertNoSecretInOutput(t, "MIIBoXsomeBase64ContentHereThatSpansALineOfItsOwn", stdout.String())
}

func TestEnvRedactCmd_MissingFile_Errors(t *testing.T) {
	repo := t.TempDir()
	initEnvGitRepo(t, repo)
	t.Chdir(repo)

	client, _ := envFixture()
	cmd := newEnvTestCmd(client, theme.Theme{})
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"redact"})

	if err := cmd.ExecuteContext(context.Background()); err == nil {
		t.Fatal("redact against a missing file returned nil error, want a refusal")
	}
}

// --- containment (representative) ---

// --- --any-file / non-env-file refusal (RCE fix) ---

func TestEnvCmds_NonEnvFile_Refused(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"keys", []string{"keys", "--file", ".git/config"}},
		{"get", []string{"get", "KEY", "--clipboard", "--file", ".git/config"}},
		{"redact", []string{"redact", "--file", ".git/config"}},
		{"check", []string{"check", "--file", ".git/config"}},
		{"set", []string{"set", "INJECTED", "--file", ".git/config"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			repo := t.TempDir()
			initEnvGitRepo(t, repo)
			const original = "[core]\n\trepositoryformatversion = 0\n"
			gitConfig := filepath.Join(repo, ".git", "config")
			if err := os.WriteFile(gitConfig, []byte(original), 0o644); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
			t.Chdir(repo)
			forceNonTTY(t)

			client, _ := envFixture()
			cmd := newEnvTestCmd(client, theme.Theme{})
			cmd.SetIn(strings.NewReader("payload\n"))
			cmd.SetOut(new(bytes.Buffer))
			cmd.SetErr(new(bytes.Buffer))
			cmd.SetArgs(c.args)

			if err := cmd.ExecuteContext(context.Background()); err == nil {
				t.Fatalf("%s --file .git/config returned nil error, want a refusal", c.name)
			}
			got, err := os.ReadFile(gitConfig)
			if err != nil {
				t.Fatalf("ReadFile: %v", err)
			}
			if string(got) != original {
				t.Errorf(".git/config content changed: %q, want unchanged %q", got, original)
			}
		})
	}
}

func TestEnvKeysCmd_EnvShapedNames_Accepted(t *testing.T) {
	names := []string{".env", ".env.local", ".env.prod", "prod.env"}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			repo := t.TempDir()
			initEnvGitRepo(t, repo)
			if err := os.WriteFile(filepath.Join(repo, name), []byte("A=1\n"), 0o600); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
			t.Chdir(repo)

			client, _ := envFixture()
			cmd := newEnvTestCmd(client, theme.Theme{})
			var stdout bytes.Buffer
			cmd.SetOut(&stdout)
			cmd.SetErr(new(bytes.Buffer))
			cmd.SetArgs([]string{"keys", "--file", name})

			if err := cmd.ExecuteContext(context.Background()); err != nil {
				t.Fatalf("keys --file %s: %v", name, err)
			}
			if want := "A\n"; stdout.String() != want {
				t.Errorf("stdout = %q, want %q", stdout.String(), want)
			}
		})
	}
}

func TestEnvSetCmd_AnyFile_NonTTY_RefusedOutright(t *testing.T) {
	repo := t.TempDir()
	initEnvGitRepo(t, repo)
	const original = "[core]\n"
	gitConfig := filepath.Join(repo, ".git", "config")
	if err := os.WriteFile(gitConfig, []byte(original), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	t.Chdir(repo)
	forceNonTTY(t) // isTerminal() == false — --any-file must refuse before ever prompting

	client, _ := envFixture()
	cmd := newEnvTestCmd(client, theme.Theme{})
	cmd.SetIn(strings.NewReader("value\n"))
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"set", "KEY", "--file", ".git/config", "--any-file"})

	err := cmd.ExecuteContext(context.Background())
	if err == nil {
		t.Fatal("set --any-file with no tty returned nil error, want a refusal")
	}
	if !strings.Contains(err.Error(), "interactive terminal") {
		t.Errorf("error = %q, want it to name the tty requirement", err.Error())
	}
	got, rerr := os.ReadFile(gitConfig)
	if rerr != nil {
		t.Fatalf("ReadFile: %v", rerr)
	}
	if string(got) != original {
		t.Errorf(".git/config content changed: %q, want unchanged %q", got, original)
	}
}

func TestEnvSetCmd_AnyFile_TTYConfirmedYes_Allowed(t *testing.T) {
	repo := t.TempDir()
	initEnvGitRepo(t, repo)
	gitConfig := filepath.Join(repo, ".git", "config")
	if err := os.WriteFile(gitConfig, []byte("[core]\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	t.Chdir(repo)
	// forceTTYWithPassword stubs BOTH isTerminal and readPassword: isTerminal
	// gates the --any-file confirmation AND resolveSetValue's own
	// interactive-prompt branch, so a real tty stub for one is a real tty
	// stub for the other too — readPassword must be stubbed alongside it.
	forceTTYWithPassword(t, "value1", nil)

	prevConfirm := confirmAnyFile
	confirmAnyFile = func(theme.Theme, string) (bool, error) { return true, nil }
	t.Cleanup(func() { confirmAnyFile = prevConfirm })

	client, _ := envFixture()
	cmd := newEnvTestCmd(client, theme.Theme{})
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"set", "KEY", "--file", ".git/config", "--any-file"})

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("set --any-file with confirmation stubbed yes: %v", err)
	}
	got, err := os.ReadFile(gitConfig)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if want := "[core]\nKEY=value1\n"; string(got) != want {
		t.Errorf(".git/config content = %q, want %q", got, want)
	}
}

func TestEnvSetCmd_AnyFile_TTYConfirmedNo_Refused(t *testing.T) {
	repo := t.TempDir()
	initEnvGitRepo(t, repo)
	const original = "[core]\n"
	gitConfig := filepath.Join(repo, ".git", "config")
	if err := os.WriteFile(gitConfig, []byte(original), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	t.Chdir(repo)

	prevTerm := isTerminal
	isTerminal = func() bool { return true }
	t.Cleanup(func() { isTerminal = prevTerm })

	prevConfirm := confirmAnyFile
	confirmAnyFile = func(theme.Theme, string) (bool, error) { return false, nil }
	t.Cleanup(func() { confirmAnyFile = prevConfirm })

	client, _ := envFixture()
	cmd := newEnvTestCmd(client, theme.Theme{})
	cmd.SetIn(strings.NewReader("value1\n"))
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"set", "KEY", "--file", ".git/config", "--any-file"})

	if err := cmd.ExecuteContext(context.Background()); err == nil {
		t.Fatal("set --any-file with confirmation declined returned nil error, want a refusal")
	}
	got, rerr := os.ReadFile(gitConfig)
	if rerr != nil {
		t.Fatalf("ReadFile: %v", rerr)
	}
	if string(got) != original {
		t.Errorf(".git/config content changed: %q, want unchanged %q", got, original)
	}
}

func TestEnvCheckCmd_AnyFile_ConfirmsBothFileAndExample(t *testing.T) {
	// check resolves --file AND --example, so --any-file must gate each
	// independently — two confirmations when both are non-env-named. This
	// pins that the confirm seam is consulted twice, once per path, rather
	// than a single gate covering both.
	repo := t.TempDir()
	initEnvGitRepo(t, repo)
	if err := os.WriteFile(filepath.Join(repo, "file.cfg"), []byte("A=1\n"), 0o600); err != nil {
		t.Fatalf("WriteFile file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, "example.cfg"), []byte("A=\n"), 0o600); err != nil {
		t.Fatalf("WriteFile example: %v", err)
	}
	t.Chdir(repo)

	prevTerm := isTerminal
	isTerminal = func() bool { return true }
	t.Cleanup(func() { isTerminal = prevTerm })

	var confirmed []string
	prevConfirm := confirmAnyFile
	confirmAnyFile = func(_ theme.Theme, msg string) (bool, error) { confirmed = append(confirmed, msg); return true, nil }
	t.Cleanup(func() { confirmAnyFile = prevConfirm })

	client, _ := envFixture()
	cmd := newEnvTestCmd(client, theme.Theme{})
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"check", "--file", "file.cfg", "--example", "example.cfg", "--any-file"})

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("check --any-file with both confirmations stubbed yes: %v", err)
	}
	if len(confirmed) != 2 {
		t.Errorf("confirmAnyFile called %d time(s), want 2 (once per non-env path)", len(confirmed))
	}
}

// TestResolveAllowAnyFile_SymlinkBindsToResolvedPath is the regression test
// for the fix binding --any-file's confirmation to the CANONICAL resolved
// path rather than the raw --file argument: a `.env` symlinked to
// `.git/config` must prompt with the real target, or a human approving
// "--file .env --any-file" would be confirming a file they never saw.
func TestResolveAllowAnyFile_SymlinkBindsToResolvedPath(t *testing.T) {
	repo := t.TempDir()
	initEnvGitRepo(t, repo)
	gitConfig := filepath.Join(repo, ".git", "config")
	if err := os.WriteFile(gitConfig, []byte("[core]\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	link := filepath.Join(repo, ".env")
	if err := os.Symlink(gitConfig, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	prevTerm := isTerminal
	isTerminal = func() bool { return true }
	t.Cleanup(func() { isTerminal = prevTerm })

	var gotMsg string
	prevConfirm := confirmAnyFile
	confirmAnyFile = func(_ theme.Theme, msg string) (bool, error) { gotMsg = msg; return true, nil }
	t.Cleanup(func() { confirmAnyFile = prevConfirm })

	target, err := resolveEnvTarget(true, ".env", repo, theme.Theme{})
	if err != nil {
		t.Fatalf("resolveEnvTarget: %v", err)
	}
	if want := filepath.Join(".git", "config"); target.Rel() != want {
		t.Errorf("target.Rel() = %q, want %q", target.Rel(), want)
	}
	if !strings.Contains(gotMsg, filepath.Join(".git", "config")) {
		t.Errorf("confirm message = %q, want it to contain the RESOLVED path %s", gotMsg, filepath.Join(".git", "config"))
	}
	// The prompt's own boilerplate legitimately contains the substring
	// ".env" ("…is not a recognized env file (.env, .env.*, or *.env)…"),
	// so this checks for the RAW ARGUMENT quoted as the prompt's subject
	// (the old, buggy form) rather than a bare ".env" substring match.
	if strings.Contains(gotMsg, `".env" is not a recognized`) {
		t.Errorf("confirm message = %q, want it bound to the resolved path, not the raw --file argument %q", gotMsg, ".env")
	}
}

// TestResolveEnvTarget_EnvNamedTarget_NoConfirmation proves an
// already-env-named resolved target skips the confirmation seam entirely —
// Target.Clear passes it regardless of --any-file, so a prompt would be pure
// noise (and, on a non-tty run, a spurious refusal).
func TestResolveEnvTarget_EnvNamedTarget_NoConfirmation(t *testing.T) {
	repo := t.TempDir()
	initEnvGitRepo(t, repo)
	if err := os.WriteFile(filepath.Join(repo, ".env"), []byte("A=1\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	calls := 0
	prevConfirm := confirmAnyFile
	confirmAnyFile = func(theme.Theme, string) (bool, error) { calls++; return true, nil }
	t.Cleanup(func() { confirmAnyFile = prevConfirm })
	// isTerminal is deliberately left at its real value: if the confirm
	// seam were reached at all on a non-tty test run, the TTY gate would
	// refuse before ever calling confirmAnyFile — so allow=true with 0
	// calls proves the env-named short-circuit fired, not just that the
	// prompt was skipped for some other reason.

	target, err := resolveEnvTarget(true, ".env", repo, theme.Theme{})
	if err != nil {
		t.Fatalf("resolveEnvTarget: %v", err)
	}
	if target.Rel() != ".env" {
		t.Errorf("target.Rel() = %q, want %q", target.Rel(), ".env")
	}
	if calls != 0 {
		t.Errorf("confirmAnyFile called %d time(s), want 0 for an already-env-named target", calls)
	}
}

// TestEnvSetCmd_ConfirmedPathIsWrittenPath is the regression test for the
// time-of-check/time-of-use defect the Target type exists to close, and it is
// written as the exploit rather than as a unit assertion because the exploit
// is the only thing that distinguishes the fixed shape from the broken one.
//
// The broken shape resolved --file once to build the confirmation prompt,
// returned a bare bool, and let the caller resolve the raw flag string AGAIN
// to decide what to write. The stub below stands in for the operator's
// think-time at that prompt: it repoints the symlink before answering yes.
// Under the old code the prompt said "notes.txt" and the write landed in
// .git/config — and `core.fsmonitor` is run by the next `git status`, so the
// payload is arbitrary execution.
//
// The primary assertion is that the write went to the path the human was
// shown, not that it refused — either would be safe, and landing on the
// flipped target is the only unsafe outcome.
//
// But a purely negative assertion is a check that cannot distinguish "the fix
// works" from "the command stopped working", so this also asserts the write
// LANDED on the confirmed file. Without that arm the test stays green if
// `--any-file` regresses to refusing unconditionally, or if the symlink branch
// stops resolving at all.
func TestEnvSetCmd_ConfirmedPathIsWrittenPath(t *testing.T) {
	repo := t.TempDir()
	initEnvGitRepo(t, repo)

	const gitConfigOriginal = "[core]\n\trepositoryformatversion = 0\n"
	gitConfig := filepath.Join(repo, ".git", "config")
	// 0644 is what git itself writes, and the fixture has to be realistic:
	// the whole point is that this file is an ordinary, world-readable config
	// that becomes an execution sink when a key lands in it.
	if err := os.WriteFile(gitConfig, []byte(gitConfigOriginal), 0o644); err != nil { //nolint:gosec // G306: matches git's own mode; see above
		t.Fatalf("WriteFile the repo config: %v", err)
	}
	decoy := filepath.Join(repo, "notes.txt")
	if err := os.WriteFile(decoy, []byte("SEED=0\n"), 0o600); err != nil {
		t.Fatalf("WriteFile notes.txt: %v", err)
	}

	link := filepath.Join(repo, "link")
	if err := os.Symlink(decoy, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	prevTerm := isTerminal
	isTerminal = func() bool { return true }
	t.Cleanup(func() { isTerminal = prevTerm })

	var gotMsg string
	prevConfirm := confirmAnyFile
	confirmAnyFile = func(_ theme.Theme, msg string) (bool, error) {
		gotMsg = msg
		// The flip, inside the confirmation window.
		if err := os.Remove(link); err != nil {
			return false, err
		}
		if err := os.Symlink(gitConfig, link); err != nil {
			return false, err
		}
		return true, nil
	}
	t.Cleanup(func() { confirmAnyFile = prevConfirm })

	t.Chdir(repo)
	client, _ := envFixture()
	cmd := newEnvTestCmd(client, theme.Theme{})
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"set", "fsmonitor", "--file", "link", "--any-file"})

	// isTerminal is stubbed true above, so `set` takes the no-echo prompt
	// branch and never reads cmd.InOrStdin(); readPassword is the seam.
	const payload = "/tmp/pwn.sh"
	prevRead := readPassword
	readPassword = func() (string, error) { return payload, nil }
	t.Cleanup(func() { readPassword = prevRead })

	execErr := cmd.ExecuteContext(context.Background())

	if !strings.Contains(gotMsg, "notes.txt") {
		t.Fatalf("confirm message = %q, want it to name notes.txt (the pre-flip resolved target)", gotMsg)
	}

	// The load-bearing assertion: .git/config is untouched, whether the
	// command succeeded against the confirmed file or refused outright.
	got, err := os.ReadFile(gitConfig) //nolint:gosec // G304: a path this test built inside its own t.TempDir()
	if err != nil {
		t.Fatalf("ReadFile the repo config: %v", err)
	}
	if string(got) != gitConfigOriginal {
		t.Fatalf(".git/config was written (exec err = %v): %q\nwant unchanged %q — the confirmed path was notes.txt", execErr, got, gitConfigOriginal)
	}
	if strings.Contains(string(got), "fsmonitor") {
		t.Fatal("the repo config gained an fsmonitor entry — the write followed the flipped symlink")
	}

	// The positive arm: the write reached the file the human confirmed. This
	// is what keeps the test able to go red — a command that refused
	// everything, or stopped resolving the symlink at all, would satisfy every
	// assertion above.
	if execErr != nil {
		t.Fatalf("set returned %v, want it to succeed against the confirmed target", execErr)
	}
	decoyGot, err := os.ReadFile(decoy) //nolint:gosec // G304: a path this test built inside its own t.TempDir()
	if err != nil {
		t.Fatalf("ReadFile the confirmed target: %v", err)
	}
	if !strings.Contains(string(decoyGot), "fsmonitor="+payload) {
		t.Errorf("the confirmed target = %q, want it to carry the written key", decoyGot)
	}
}

// TestEnvSetCmd_ParentSwapDuringConfirmation is the second half of the
// confirmation-window regression, and the reason env.Target carries an open
// directory descriptor rather than just a resolved path.
//
// Carrying the path fixed the symlinked-TARGET case and left this one wide
// open: with every consumer still opening that path BY STRING, swapping an
// intermediate DIRECTORY during the prompt redirected the write exactly as
// before. A security review reproduced the original payload this way against
// the half-fixed tree — a prompt reading "sub/config" wrote core.fsmonitor
// into .git/config and exited zero.
//
// So the assertion is the same one as the sibling test, against a different
// attack: the write reaches the directory that was resolved, whatever the path
// now spells.
func TestEnvSetCmd_ParentSwapDuringConfirmation(t *testing.T) {
	repo := t.TempDir()
	initEnvGitRepo(t, repo)

	const configOriginal = "[core]\n\trepositoryformatversion = 0\n"
	repoConfig := filepath.Join(repo, ".git", "config")
	if err := os.WriteFile(repoConfig, []byte(configOriginal), 0o644); err != nil { //nolint:gosec // G306: matches the mode git itself writes
		t.Fatalf("WriteFile the repo config: %v", err)
	}

	// `sub` is a REAL directory at resolution time. That is load-bearing: if
	// it were already a symlink, EvalSymlinks would resolve it away during
	// resolution and flipping it afterwards could not affect anything — which
	// is a test that passes against the broken code too. The attack needs a
	// genuine directory that is REPLACED by a symlink inside the window.
	subDir := filepath.Join(repo, "sub")
	if err := os.Mkdir(subDir, 0o750); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(subDir, "config"), []byte("SEED=0\n"), 0o600); err != nil {
		t.Fatalf("WriteFile the decoy: %v", err)
	}
	// Where the original directory is moved to, so the test can read back
	// what the pinned descriptor actually wrote.
	movedDir := filepath.Join(repo, "sub-moved")

	prevTerm := isTerminal
	isTerminal = func() bool { return true }
	t.Cleanup(func() { isTerminal = prevTerm })

	const payload = "/tmp/pwn.sh"
	prevRead := readPassword
	readPassword = func() (string, error) { return payload, nil }
	t.Cleanup(func() { readPassword = prevRead })

	var gotMsg string
	prevConfirm := confirmAnyFile
	confirmAnyFile = func(_ theme.Theme, msg string) (bool, error) {
		gotMsg = msg
		// The flip, inside the confirmation window: move the real `sub` aside
		// and put a symlink to `.git` in its place. Any later walk of the
		// string "<repo>/sub/config" now lands on .git/config.
		if err := os.Rename(subDir, movedDir); err != nil {
			return false, err
		}
		if err := os.Symlink(filepath.Join(repo, ".git"), subDir); err != nil {
			return false, err
		}
		return true, nil
	}
	t.Cleanup(func() { confirmAnyFile = prevConfirm })

	t.Chdir(repo)
	client, _ := envFixture()
	cmd := newEnvTestCmd(client, theme.Theme{})
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"set", "fsmonitor", "--file", filepath.Join("sub", "config"), "--any-file"})

	execErr := cmd.ExecuteContext(context.Background())

	if !strings.Contains(gotMsg, filepath.Join("sub", "config")) {
		t.Fatalf("confirm message = %q, want it to name the confirmed target", gotMsg)
	}

	got, err := os.ReadFile(repoConfig) //nolint:gosec // G304: a path this test built inside its own t.TempDir()
	if err != nil {
		t.Fatalf("ReadFile the repo config: %v", err)
	}
	if string(got) != configOriginal {
		t.Fatalf("the repo config was written (exec err = %v): %q\nwant unchanged — the confirmed directory was sub/, not .git/", execErr, got)
	}

	// Positive arm, same reasoning as the sibling test: without it, a command
	// that refused everything would pass. The write must have reached the
	// ORIGINAL directory, which is now at sub-moved/ — that is what proves the
	// descriptor followed the inode rather than the name.
	if execErr != nil {
		t.Fatalf("set returned %v, want it to succeed against the confirmed target", execErr)
	}
	decoyGot, err := os.ReadFile(filepath.Join(movedDir, "config")) //nolint:gosec // G304: a path this test built inside its own t.TempDir()
	if err != nil {
		t.Fatalf("ReadFile the confirmed target: %v", err)
	}
	if !strings.Contains(string(decoyGot), "fsmonitor="+payload) {
		t.Errorf("the confirmed target = %q, want it to carry the written key", decoyGot)
	}
}

func TestEnvKeysCmd_OutsideRepo_Refused(t *testing.T) {
	base := t.TempDir()
	repo := filepath.Join(base, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	initEnvGitRepo(t, repo)
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(outside, "secret.env"), []byte("KEY=1\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	t.Chdir(repo)

	client, _ := envFixture()
	cmd := newEnvTestCmd(client, theme.Theme{})
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"keys", "--file", "../outside/secret.env"})

	if err := cmd.ExecuteContext(context.Background()); err == nil {
		t.Fatal("keys with a --file escaping the repo returned nil error, want a refusal")
	}
}
