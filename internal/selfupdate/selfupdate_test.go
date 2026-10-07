package selfupdate

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/meta"
)

func TestIsSourceBuild(t *testing.T) {
	orig := meta.Version
	t.Cleanup(func() { meta.Version = orig })

	meta.Version = "dev"
	if !IsSourceBuild() {
		t.Error("IsSourceBuild() = false with meta.Version=dev, want true")
	}

	meta.Version = "1.2.3"
	if IsSourceBuild() {
		t.Error("IsSourceBuild() = true with meta.Version=1.2.3, want false")
	}
}

func TestCheckOutdated_UpToDate(t *testing.T) {
	fr := &exec.FakeRunner{RunFunc: func(_ string, _ []string) (string, error) {
		return "", nil
	}}
	outdated, detail, err := CheckOutdated(context.Background(), fr)
	if err != nil {
		t.Fatalf("CheckOutdated: %v", err)
	}
	if outdated {
		t.Errorf("outdated = true on empty brew output, want false")
	}
	if detail != "" {
		t.Errorf("detail = %q, want empty", detail)
	}
	assertBrewArgv(t, fr, []string{"brew", "outdated", "--cask", CaskRef})
	assertNoAutoUpdate(t, fr)
}

func TestCheckOutdated_Outdated(t *testing.T) {
	fr := &exec.FakeRunner{RunFunc: func(_ string, _ []string) (string, error) {
		return "cameronsjo/tap/forgectl (0.9.0) < 0.10.0", nil
	}}
	outdated, detail, err := CheckOutdated(context.Background(), fr)
	if err != nil {
		t.Fatalf("CheckOutdated: %v", err)
	}
	if !outdated {
		t.Error("outdated = false with non-empty brew output, want true")
	}
	if detail == "" {
		t.Error("detail is empty, want brew's outdated line")
	}
}

func TestCheckOutdated_Error(t *testing.T) {
	fr := &exec.FakeRunner{RunFunc: func(_ string, _ []string) (string, error) {
		return "", &exec.CommandError{Name: "brew", Stderr: "brew: command not found", ExitCode: 127, Err: errors.New("exit status 127")}
	}}
	if _, _, err := CheckOutdated(context.Background(), fr); err == nil {
		t.Error("CheckOutdated returned nil error on a real brew failure")
	}
}

func TestUpgrade_RunsUpdateThenUpgradeInOrder(t *testing.T) {
	fr := &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		return strings.Join(append([]string{name}, args...), " ") + " ok", nil
	}}
	res, err := Upgrade(context.Background(), fr, nil)
	out := res.Output
	if err != nil {
		t.Fatalf("Upgrade: %v", err)
	}
	if len(fr.Calls) != 2 {
		t.Fatalf("got %d calls, want 2: %+v", len(fr.Calls), fr.Calls)
	}
	assertArgv(t, fr.Calls[0], []string{"brew", "update"})
	assertArgv(t, fr.Calls[1], []string{"brew", "upgrade", "--cask", CaskRef})
	if !strings.Contains(out, "brew update ok") || !strings.Contains(out, "brew upgrade --cask "+CaskRef+" ok") {
		t.Errorf("Upgrade output = %q, want both steps' output present", out)
	}
}

func TestUpgrade_StopsAfterUpdateFailure(t *testing.T) {
	fr := &exec.FakeRunner{RunFunc: func(name string, _ []string) (string, error) {
		if name == "brew" {
			return "", &exec.CommandError{Name: "brew", Stderr: "network unreachable", Err: errors.New("exit status 1")}
		}
		return "", nil
	}}
	_, err := Upgrade(context.Background(), fr, nil)
	if err == nil {
		t.Fatal("Upgrade returned nil error when brew update failed")
	}
	// A failed `brew update` must never reach `brew upgrade` — that's the
	// "never leaves a half-applied step" guarantee this test pins.
	if len(fr.Calls) != 1 {
		t.Fatalf("got %d calls, want 1 (upgrade must not run after update fails): %+v", len(fr.Calls), fr.Calls)
	}
}

func assertArgv(t *testing.T, call exec.Call, want []string) {
	t.Helper()
	got := append([]string{call.Name}, call.Args...)
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("argv = %v, want %v", got, want)
	}
}

func assertBrewArgv(t *testing.T, fr *exec.FakeRunner, want []string) {
	t.Helper()
	if len(fr.Calls) != 1 {
		t.Fatalf("got %d calls, want 1: %+v", len(fr.Calls), fr.Calls)
	}
	assertArgv(t, fr.Calls[0], want)
}

func assertNoAutoUpdate(t *testing.T, fr *exec.FakeRunner) {
	t.Helper()
	assertHomebrewSafeEnv(t, fr.Last())
}

// assertHomebrewSafeEnv pins the full hardened env set on one brew call:
// HOMEBREW_NO_AUTO_UPDATE=1, plus HOMEBREW_ARTIFACT_DOMAIN,
// HOMEBREW_CASK_OPTS, and HOMEBREW_BREW_GIT_REMOTE forced to "" — an
// ambient value for any of the latter three (e.g. from a direnv-managed
// .envrc) could redirect where the upgrade artifact or tap comes from on
// the one command whose output is a new binary on $PATH.
func assertHomebrewSafeEnv(t *testing.T, call exec.Call) {
	t.Helper()
	want := map[string]string{
		"HOMEBREW_NO_AUTO_UPDATE":  "1",
		"HOMEBREW_ARTIFACT_DOMAIN": "",
		"HOMEBREW_CASK_OPTS":       "",
		"HOMEBREW_BREW_GIT_REMOTE": "",
	}
	for k, v := range want {
		if got, ok := call.Env[k]; !ok || got != v {
			t.Errorf("%s = %q (present=%v), want %q — a security-relevant HOMEBREW_* var isn't pinned on this brew call", k, got, ok, v)
		}
	}
}

// TestBrewCalls_PinSecurityRelevantEnv exercises every brew-shelling entry
// point in this package and confirms each pins the full hardened env, not
// just HOMEBREW_NO_AUTO_UPDATE — a regression here would let an ambient
// HOMEBREW_ARTIFACT_DOMAIN/CASK_OPTS/BREW_GIT_REMOTE redirect brew's
// download or tap on `forgectl upgrade`, the one command whose output is a
// new binary on $PATH.
func TestBrewCalls_PinSecurityRelevantEnv(t *testing.T) {
	fr := &exec.FakeRunner{RunFunc: func(_ string, _ []string) (string, error) { return "", nil }}
	if _, _, err := CheckOutdated(context.Background(), fr); err != nil {
		t.Fatalf("CheckOutdated: %v", err)
	}
	assertHomebrewSafeEnv(t, fr.Last())

	fr = &exec.FakeRunner{RunFunc: func(_ string, _ []string) (string, error) { return "", nil }}
	if _, err := Upgrade(context.Background(), fr, nil); err != nil {
		t.Fatalf("Upgrade: %v", err)
	}
	for _, call := range fr.Calls {
		assertHomebrewSafeEnv(t, call)
	}
}

// failingCommand is a brew failure the way OSRunner reports one: stdout in
// Output, stderr in Stderr, no output returned alongside.
func failingCommand(name string) error {
	return &exec.CommandError{Name: name, Output: "==> Downloading", Stderr: "Error: boom\x1b[2J", ExitCode: 1, Err: errors.New("exit status 1")}
}

func TestUpgrade_UpgradeFailure_CarriesStepAndOutput(t *testing.T) {
	fr := &exec.FakeRunner{RunFunc: func(_ string, args []string) (string, error) {
		switch args[0] {
		case "upgrade":
			return "", failingCommand("brew")
		case "list":
			return "forgectl 0.31.0", nil
		case "outdated":
			return "forgectl (0.31.0) < 0.32.0", nil
		}
		return "updated", nil
	}}
	_, err := Upgrade(context.Background(), fr, nil)
	var se *StepError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want a *StepError", err)
	}
	if !errors.Is(err, ErrCaskUpgrade) || errors.Is(err, ErrTapUpdate) {
		t.Errorf("err = %v, want ErrCaskUpgrade only", err)
	}
	var ce *exec.CommandError
	if !errors.As(err, &ce) {
		t.Error("the CommandError is no longer on the chain")
	}
	if se.Step != "brew upgrade --cask "+CaskRef || se.ExitCode != 1 {
		t.Errorf("step = %q exit = %d", se.Step, se.ExitCode)
	}
	if !strings.Contains(se.Output, "==> Downloading") || !strings.Contains(se.Output, "Error: boom") {
		t.Errorf("Output = %q, want stdout then stderr", se.Output)
	}
	if se.Cause != "" {
		t.Errorf("Cause = %q, want none when the cask is installed and outdated", se.Cause)
	}
	if strings.Contains(err.Error(), "boom") {
		t.Errorf("Error() = %q, echoes brew's text", err)
	}
}

func TestUpgrade_UpdateFailure_CarriesOutput(t *testing.T) {
	fr := &exec.FakeRunner{RunFunc: func(_ string, _ []string) (string, error) { return "", failingCommand("brew") }}
	_, err := Upgrade(context.Background(), fr, nil)
	var se *StepError
	if !errors.As(err, &se) || !errors.Is(err, ErrTapUpdate) || se.Step != "brew update" {
		t.Fatalf("err = %v, want a brew update StepError", err)
	}
	if !strings.Contains(se.Output, "Error: boom") {
		t.Errorf("Output = %q", se.Output)
	}
}

// brew exits non-zero though the cask is installed and current: success.
func TestUpgrade_AlreadyCurrentIsSuccess(t *testing.T) {
	fr := &exec.FakeRunner{RunFunc: func(_ string, args []string) (string, error) {
		switch args[0] {
		case "upgrade":
			return "", failingCommand("brew")
		case "list":
			return "forgectl 0.31.0", nil
		}
		return "", nil // update, outdated
	}}
	res, err := Upgrade(context.Background(), fr, nil)
	if err != nil || !res.AlreadyCurrent {
		t.Fatalf("res = %+v err = %v, want AlreadyCurrent and no error", res, err)
	}
	for _, c := range fr.Calls {
		assertHomebrewSafeEnv(t, c)
	}
}

// brew exits non-zero because forgectl is not a cask install: say so.
func TestUpgrade_NotInstalledAsCask_Diagnosed(t *testing.T) {
	fr := &exec.FakeRunner{RunFunc: func(_ string, args []string) (string, error) {
		switch args[0] {
		case "upgrade":
			return "", failingCommand("brew")
		case "list":
			return "", &exec.CommandError{Name: "brew", ExitCode: 1, Err: errors.New("exit status 1")}
		}
		return "", nil
	}}
	_, err := Upgrade(context.Background(), fr, nil)
	var se *StepError
	if !errors.As(err, &se) || !strings.Contains(se.Cause, "not installed as a Homebrew cask") {
		t.Fatalf("err = %v, want the not-a-cask cause", err)
	}
	// The probe names the unqualified token: brew list --cask rejects the
	// tap-qualified reference for an installed cask.
	if last := fr.Calls[2].Args; last[len(last)-1] != "forgectl" {
		t.Errorf("list probe args = %v, want the bare cask token", last)
	}
}

func TestUpgrade_StreamPrintsHeadersAndSanitizedOutput(t *testing.T) {
	fr := &exec.FakeRunner{RunFunc: func(_ string, args []string) (string, error) {
		return "line from " + args[0] + " \x1b[2J\x1b]0;pwned\x07", nil
	}}
	var stream bytes.Buffer
	if _, err := Upgrade(context.Background(), fr, &stream); err != nil {
		t.Fatal(err)
	}
	got := stream.String()
	for _, want := range []string{"== brew update\n", "== brew upgrade --cask " + CaskRef + "\n", "line from update", "line from upgrade"} {
		if !strings.Contains(got, want) {
			t.Errorf("stream = %q, want %q", got, want)
		}
	}
	if strings.ContainsAny(got, "\x1b\x07") {
		t.Errorf("stream = %q, carries a raw control byte", got)
	}
}

func TestUpgrade_StreamFailureShowsOutputAndStops(t *testing.T) {
	fr := &exec.FakeRunner{RunFunc: func(_ string, _ []string) (string, error) { return "", failingCommand("brew") }}
	var stream bytes.Buffer
	if _, err := Upgrade(context.Background(), fr, &stream); err == nil {
		t.Fatal("want an error")
	}
	if got := stream.String(); !strings.Contains(got, "== brew update") || !strings.Contains(got, "Error: boom") || strings.ContainsRune(got, 0x1b) {
		t.Errorf("stream = %q, want header and escaped failure output", got)
	}
}

// fakeBrewPath puts a `brew` script that prints to both streams and exits with
// the code of the step named in BREW_FAIL on PATH.
func fakeBrewPath(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell fake brew")
	}
	dir := t.TempDir()
	script := "#!/bin/sh\necho \"out:$1\"\necho \"err:$1 \\033[2J\" >&2\n[ \"$1\" = \"$BREW_FAIL\" ] && exit 3\nexit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "brew"), []byte(script), 0o700); err != nil { //nolint:gosec // the fake brew must be executable
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// The real OSRunner path: output streams live from a child, interleaved and
// escaped, and a failing step keeps its exit status.
func TestUpgrade_OSRunnerStreamsLive(t *testing.T) {
	fakeBrewPath(t)
	t.Setenv("BREW_FAIL", "")
	var stream bytes.Buffer
	res, err := Upgrade(context.Background(), exec.OSRunner{}, &stream)
	if err != nil {
		t.Fatal(err)
	}
	got := stream.String()
	for _, want := range []string{"== brew update", "out:update", "err:update", "out:upgrade"} {
		if !strings.Contains(got, want) {
			t.Errorf("stream = %q, want %q", got, want)
		}
	}
	if strings.ContainsRune(got, 0x1b) {
		t.Errorf("stream = %q, carries a raw ESC", got)
	}
	if !strings.Contains(res.Output, "out:upgrade") {
		t.Errorf("Output = %q, want the capture kept", res.Output)
	}

	t.Setenv("BREW_FAIL", "upgrade")
	stream.Reset()
	_, err = Upgrade(context.Background(), exec.OSRunner{}, &stream)
	var se *StepError
	if !errors.As(err, &se) || se.ExitCode != 3 || !strings.Contains(se.Output, "err:upgrade") {
		t.Fatalf("err = %v (%+v), want exit 3 with the streamed output captured", err, se)
	}
}

func TestUpgrade_OSRunnerCapturesWithoutStream(t *testing.T) {
	fakeBrewPath(t)
	t.Setenv("BREW_FAIL", "update")
	_, err := Upgrade(context.Background(), exec.OSRunner{}, nil)
	var se *StepError
	if !errors.As(err, &se) || se.Step != "brew update" || !strings.Contains(se.Output, "out:update") || !strings.Contains(se.Output, "err:update") {
		t.Fatalf("err = %v (%+v), want the captured stdout and stderr", err, se)
	}
}

func TestTail(t *testing.T) {
	in := "a\n\nb\r\nc \x1b[2J\nd\ne\n"
	got := Tail(in, 3)
	if strings.ContainsRune(got, 0x1b) {
		t.Errorf("Tail = %q, carries a raw ESC", got)
	}
	if lines := strings.Split(got, "\n"); len(lines) != 3 || !strings.HasPrefix(lines[0], "c ") || lines[1] != "d" || lines[2] != "e" {
		t.Errorf("Tail = %q, want the last 3 lines", got)
	}
	if Tail("", 5) != "" {
		t.Error("Tail of nothing is not empty")
	}
}

func TestSafeWriter_CRLFSplitAcrossWrites(t *testing.T) {
	var out bytes.Buffer
	w := newSafeWriter(&out)
	_, _ = w.Write([]byte("a\r"))
	_, _ = w.Write([]byte("\nb\n"))
	if got := out.String(); got != "a\nb\n" {
		t.Errorf("out = %q, want a and b with no blank line", got)
	}
}

// A line past the cap is withheld whole, so a credential-shaped word cannot be
// split across two separately redacted chunks.
func TestSafeWriter_OverlongLineWithheld(t *testing.T) {
	var out bytes.Buffer
	w := newSafeWriter(&out)
	long := strings.Repeat("x", maxLineBytes-3) + " https://user:hunter2@example.com/x " + strings.Repeat("y", 100)
	_, _ = w.Write([]byte(long))
	_, _ = w.Write([]byte("tail\nnext\n"))
	got := out.String()
	if strings.Contains(got, "hunter2") || strings.Contains(got, "xxxx") || strings.Contains(got, "tail") {
		t.Errorf("out = %q, shows part of the over-long line", got)
	}
	if !strings.Contains(got, "withheld") || !strings.HasSuffix(got, "next\n") {
		t.Errorf("out = %q, want the marker and the following line", got)
	}
}
