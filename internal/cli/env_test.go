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
// check
//   [x] Happy: no drift → exit 0
//   [x] Unhappy: a missing key → exit 1
//   [x] Happy: an extra key is reported but stays exit 0
//   [x] Unhappy: a missing --example file is its own distinct error
//   [x] Happy: --file and --example compose
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
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	clippkg "github.com/cameronsjo/forgectl/internal/clip"
	envpkg "github.com/cameronsjo/forgectl/internal/env"
	"github.com/cameronsjo/forgectl/internal/exec"
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
	cmd := newEnvCmdForClient(client)
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
	cmd := newEnvCmdForClient(client)
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
	cmd := newEnvCmdForClient(client)
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
	cmd := newEnvCmdForClient(client)
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
	cmd := newEnvCmdForClient(client)
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
			cmd := newEnvCmdForClient(client)
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
	cmd := newEnvCmdForClient(client)
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
	cmd := newEnvCmdForClient(client)
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
	cmd := newEnvCmdForClient(client)
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
	if !strings.Contains(stderr.String(), "Value for KEY: ") {
		t.Errorf("stderr = %q, want the no-echo prompt text", stderr.String())
	}
	assertNoSecretInOutput(t, sentinel, stdout.String(), stderr.String())
}

func TestEnvSetCmd_NewFile_0600(t *testing.T) {
	repo := t.TempDir()
	initEnvGitRepo(t, repo)
	t.Chdir(repo)
	forceNonTTY(t)

	client, _ := envFixture()
	cmd := newEnvCmdForClient(client)
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
	cmd := newEnvCmdForClient(client)
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
	cmd := newEnvCmdForClient(client)
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
	cmd := newEnvCmdForClient(client)
	cmd.SetIn(strings.NewReader("3\n"))
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"set", "KEY"})

	if err := cmd.ExecuteContext(context.Background()); err == nil {
		t.Fatal("set against a duplicate key returned nil error, want a refusal")
	}
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
	cmd := newEnvCmdForClient(client)
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
	cmd := newEnvCmdForClient(client)
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
	cmd := newEnvCmdForClient(client)
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
	cmd := newEnvCmdForClient(client)
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
	cmd := newEnvCmdForClient(client)
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"check"})

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(stdout.String(), "missing:") || !strings.Contains(stdout.String(), "extra:") {
		t.Errorf("stdout = %q, want missing:/extra: headers", stdout.String())
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
	cmd := newEnvCmdForClient(client)
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"check"})

	err := cmd.ExecuteContext(context.Background())
	if err == nil {
		t.Fatal("check with a missing key returned nil error, want exit 1")
	}
	if !strings.Contains(stdout.String(), "  B") {
		t.Errorf("stdout = %q, want the missing key B reported under missing:", stdout.String())
	}
}

func TestEnvCheckCmd_ExtraKey_ReportedExitZero(t *testing.T) {
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
	cmd := newEnvCmdForClient(client)
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"check"})

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("unexpected error (extra keys must not fail check): %v", err)
	}
	if !strings.Contains(stdout.String(), "  LOCAL_ONLY") {
		t.Errorf("stdout = %q, want LOCAL_ONLY reported under extra:", stdout.String())
	}
}

func TestEnvCheckCmd_MissingExampleFile_DistinctError(t *testing.T) {
	repo := t.TempDir()
	initEnvGitRepo(t, repo)
	if err := os.WriteFile(filepath.Join(repo, ".env"), []byte("A=1\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	t.Chdir(repo)

	client, _ := envFixture()
	cmd := newEnvCmdForClient(client)
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
	cmd := newEnvCmdForClient(client)
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
	cmd := newEnvCmdForClient(client)
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
	cmd := newEnvCmdForClient(client)
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
	cmd := newEnvCmdForClient(client)
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"redact"})

	if err := cmd.ExecuteContext(context.Background()); err == nil {
		t.Fatal("redact against a missing file returned nil error, want a refusal")
	}
}

// --- containment (representative) ---

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
	cmd := newEnvCmdForClient(client)
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"keys", "--file", "../outside/secret.env"})

	if err := cmd.ExecuteContext(context.Background()); err == nil {
		t.Fatal("keys with a --file escaping the repo returned nil error, want a refusal")
	}
}
