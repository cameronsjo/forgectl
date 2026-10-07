package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/meta"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/selfupdate"
)

// stubUpgradeLookPath overrides upgradeLookPath for the duration of the
// test, restoring it via t.Cleanup — mirrors stubConfirmSeams's pattern
// (update_test.go).
func stubUpgradeLookPath(t *testing.T, found ...string) {
	t.Helper()
	set := make(map[string]bool, len(found))
	for _, n := range found {
		set[n] = true
	}
	prev := upgradeLookPath
	upgradeLookPath = func(name string) (string, error) {
		if set[name] {
			return "/usr/local/bin/" + name, nil
		}
		return "", errors.New("not found")
	}
	t.Cleanup(func() { upgradeLookPath = prev })
}

func setMetaVersion(t *testing.T, v string) {
	t.Helper()
	prev := meta.Version
	meta.Version = v
	t.Cleanup(func() { meta.Version = prev })
}

func execUpgrade(t *testing.T, runner exec.Runner, args ...string) (stdout string, err error) {
	t.Helper()
	stdout, _, err = execUpgradeBoth(t, runner, args...)
	return stdout, err
}

// execUpgradeBoth also returns stderr, where brew's output tail and live
// stream go. The stderr is not a terminal unless a test stubs it.
func execUpgradeBoth(t *testing.T, runner exec.Runner, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	deps := module.Deps{Cfg: config.Config{}, Runner: runner}
	cmd := newUpgradeCmd(deps)
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(args)
	err = cmd.Execute()
	return out.String(), errOut.String(), err
}

func stubUpgradeTTY(t *testing.T, tty bool) {
	t.Helper()
	prev := upgradeStderrIsTTY
	upgradeStderrIsTTY = func() bool { return tty }
	t.Cleanup(func() { upgradeStderrIsTTY = prev })
}

// brewFailing fails the named brew subcommand with output on both streams.
func brewFailing(sub string) *exec.FakeRunner {
	return &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		if args[0] == sub {
			return "", &exec.CommandError{Name: name, Args: args, Output: "==> Downloading", Stderr: "Error: SERVERTEXT\x1b[2J", ExitCode: 1, Err: errors.New("exit status 1")}
		}
		switch args[0] {
		case "list":
			return "forgectl 1.0.0", nil
		case "outdated":
			return "cameronsjo/tap/forgectl (1.0.0) < 1.1.0", nil
		}
		return "", nil
	}}
}

func TestUpgrade_SourceBuild_WarnsAndExitsZero(t *testing.T) {
	setMetaVersion(t, "dev")
	stubUpgradeLookPath(t, "brew") // present but must never be consulted

	fr := &exec.FakeRunner{}
	stdout, err := execUpgrade(t, fr)
	if err != nil {
		t.Fatalf("Execute() = %v (exit %d), want nil (source build warns, never refuses)", err, ExitCode(err))
	}
	if !bytes.Contains([]byte(stdout), []byte("built from source")) {
		t.Errorf("stdout = %q, want the source-build warning", stdout)
	}
	if len(fr.Calls) != 0 {
		t.Errorf("source build invoked %d shell command(s), want 0 (nothing to upgrade)", len(fr.Calls))
	}
}

func TestUpgrade_BrewMissing_ExitsOne(t *testing.T) {
	setMetaVersion(t, "1.0.0")
	stubUpgradeLookPath(t) // nothing found

	fr := &exec.FakeRunner{}
	_, err := execUpgrade(t, fr)
	if err == nil {
		t.Fatal("Execute() = nil, want an error (brew missing)")
	}
	if ExitCode(err) != 1 {
		t.Errorf("ExitCode = %d, want 1", ExitCode(err))
	}
}

func TestUpgrade_Check_UpToDate(t *testing.T) {
	setMetaVersion(t, "1.0.0")
	stubUpgradeLookPath(t, "brew")

	fr := &exec.FakeRunner{RunFunc: func(_ string, _ []string) (string, error) { return "", nil }}
	stdout, err := execUpgrade(t, fr, "--check")
	if err != nil {
		t.Fatalf("Execute() = %v, want nil; stdout=%q", err, stdout)
	}
	if !bytes.Contains([]byte(stdout), []byte("up to date")) {
		t.Errorf("stdout = %q, want an up-to-date message", stdout)
	}
	// --check must never touch `brew upgrade` — only `brew outdated`.
	for _, c := range fr.Calls {
		if len(c.Args) > 0 && c.Args[0] == "upgrade" {
			t.Errorf("--check ran %v, want no mutating brew upgrade call", c.Args)
		}
	}
}

func TestUpgrade_Check_Outdated(t *testing.T) {
	setMetaVersion(t, "1.0.0")
	stubUpgradeLookPath(t, "brew")

	fr := &exec.FakeRunner{RunFunc: func(_ string, _ []string) (string, error) {
		return "cameronsjo/tap/forgectl (1.0.0) < 1.1.0", nil
	}}
	stdout, err := execUpgrade(t, fr, "--check")
	if err != nil {
		t.Fatalf("Execute() = %v, want nil; stdout=%q", err, stdout)
	}
	if !bytes.Contains([]byte(stdout), []byte("update available")) {
		t.Errorf("stdout = %q, want an update-available message", stdout)
	}
}

func TestUpgrade_Apply_RunsUpdateThenUpgrade(t *testing.T) {
	setMetaVersion(t, "1.0.0")
	stubUpgradeLookPath(t, "brew")

	fr := &exec.FakeRunner{RunFunc: func(_ string, _ []string) (string, error) { return "done", nil }}
	stdout, err := execUpgrade(t, fr)
	if err != nil {
		t.Fatalf("Execute() = %v, want nil; stdout=%q", err, stdout)
	}
	if len(fr.Calls) != 2 {
		t.Fatalf("got %d brew calls, want 2 (update, upgrade): %+v", len(fr.Calls), fr.Calls)
	}
	if fr.Calls[0].Args[0] != "update" || fr.Calls[1].Args[0] != "upgrade" {
		t.Errorf("call order = %v then %v, want update then upgrade", fr.Calls[0].Args, fr.Calls[1].Args)
	}
	if fr.Calls[1].Args[len(fr.Calls[1].Args)-1] != selfupdate.CaskRef {
		t.Errorf("upgrade call = %v, want it to name the cask %s", fr.Calls[1].Args, selfupdate.CaskRef)
	}
}

func TestUpgrade_Apply_UpdateFailure_NeverRunsUpgrade(t *testing.T) {
	setMetaVersion(t, "1.0.0")
	stubUpgradeLookPath(t, "brew")

	fr := &exec.FakeRunner{RunFunc: func(name string, _ []string) (string, error) {
		return "", &exec.CommandError{Name: name, Stderr: "network unreachable"}
	}}
	_, err := execUpgrade(t, fr)
	if err == nil {
		t.Fatal("Execute() = nil, want an error (brew update failed)")
	}
	if ExitCode(err) != 1 {
		t.Errorf("ExitCode = %d, want 1", ExitCode(err))
	}
	if len(fr.Calls) != 1 {
		t.Errorf("got %d calls, want 1 — a failed update must never reach upgrade: %+v", len(fr.Calls), fr.Calls)
	}
}

// TestUpgrade_Check_NeverEchoesBrew pins #738: `upgrade --check` words both
// outcomes from fixed text and version tokens, never from brew's stdout or
// its CommandError (argv plus stderr, which relays the tap's server).
func TestUpgrade_Check_NeverEchoesBrew(t *testing.T) {
	setMetaVersion(t, "1.0.0")
	stubUpgradeLookPath(t, "brew")
	const marker = "SERVERTEXT\x1b[2J"

	fr := &exec.FakeRunner{RunFunc: func(_ string, _ []string) (string, error) {
		return marker + " cameronsjo/tap/forgectl (1.0.0_1) != 1.1.0 " + marker, nil
	}}
	stdout, err := execUpgrade(t, fr, "--check")
	if err != nil {
		t.Fatalf("Execute() = %v, want nil", err)
	}
	if want := "update available: forgectl 1.0.0_1 installed, 1.1.0 available\n"; stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}

	fr = &exec.FakeRunner{RunFunc: func(name string, _ []string) (string, error) {
		return "", &exec.CommandError{Name: name, Stderr: marker}
	}}
	stdout, err = execUpgrade(t, fr, "--check")
	if err == nil {
		t.Fatal("Execute() = nil, want an error (brew outdated failed)")
	}
	if ExitCode(err) != 1 {
		t.Errorf("ExitCode = %d, want 1", ExitCode(err))
	}
	if msg := err.Error() + stdout; bytes.Contains([]byte(msg), []byte("SERVERTEXT")) || bytes.Contains([]byte(msg), []byte("outdated --cask")) {
		t.Errorf("output = %q, echoes brew's text or argv", msg)
	}
	var ce *exec.CommandError
	if !errors.As(err, &ce) {
		t.Errorf("the CommandError is no longer on the chain: %v", err)
	}
}

// TestUpgrade_Apply_NeverEchoesBrew pins #761: the outcome line and the error
// are fixed text, never brew's stdout or its CommandError (argv plus stderr,
// which relays the tap's server). Brew's output reaches only stderr, escaped,
// so stdout and the error carry none of it. It still tells a failed tap
// refresh from a failed cask upgrade.
func TestUpgrade_Apply_NeverEchoesBrew(t *testing.T) {
	setMetaVersion(t, "1.0.0")
	stubUpgradeLookPath(t, "brew")
	const marker = "SERVERTEXT\x1b[2J"

	fr := &exec.FakeRunner{RunFunc: func(_ string, _ []string) (string, error) { return marker, nil }}
	stdout, err := execUpgrade(t, fr)
	if err != nil {
		t.Fatalf("Execute() = %v, want nil", err)
	}
	if bytes.Contains([]byte(stdout), []byte("SERVERTEXT")) {
		t.Errorf("stdout = %q, relays brew's output", stdout)
	}
	if !bytes.Contains([]byte(stdout), []byte("forgectl upgraded")) {
		t.Errorf("stdout = %q, want the fixed success line", stdout)
	}

	for _, tc := range []struct {
		name     string
		failArg  string
		wantText string
	}{
		{"update fails", "update", "brew update failed"},
		{"upgrade fails", "upgrade", "may be unchanged"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fr := &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
				if args[0] == tc.failArg {
					return marker, &exec.CommandError{Name: name, Args: args, Stderr: marker}
				}
				return marker, nil
			}}
			stdout, err := execUpgrade(t, fr)
			if err == nil {
				t.Fatal("Execute() = nil, want an error")
			}
			if ExitCode(err) != 1 {
				t.Errorf("ExitCode = %d, want 1", ExitCode(err))
			}
			msg := err.Error() + stdout
			if bytes.Contains([]byte(msg), []byte("SERVERTEXT")) || bytes.Contains([]byte(msg), []byte("--cask "+selfupdate.CaskRef)) {
				t.Errorf("output = %q, echoes brew's text or argv", msg)
			}
			if !bytes.Contains([]byte(err.Error()), []byte(tc.wantText)) {
				t.Errorf("error = %q, want it to name the failed step (%q)", err, tc.wantText)
			}
			var ce *exec.CommandError
			if !errors.As(err, &ce) {
				t.Errorf("the CommandError is no longer on the chain: %v", err)
			}
		})
	}
}

// TestUpgrade_Apply_SuccessNamesVersions pins #761 (a): the success line names
// the from and to versions, rebuilt from version tokens in brew's upgrade
// output, never its text; with no versions to read it keeps the plain line.
func TestUpgrade_Apply_SuccessNamesVersions(t *testing.T) {
	setMetaVersion(t, "1.0.0")
	stubUpgradeLookPath(t, "brew")

	fr := &exec.FakeRunner{RunFunc: func(_ string, args []string) (string, error) {
		if args[0] == "upgrade" {
			return "==> Upgrading 1 outdated package:\ncameronsjo/tap/forgectl 1.0.0 -> 1.1.0 SERVERTEXT\x1b[2J", nil
		}
		return "", nil
	}}
	stdout, err := execUpgrade(t, fr)
	if err != nil {
		t.Fatalf("Execute() = %v, want nil", err)
	}
	if !bytes.Contains([]byte(stdout), []byte("forgectl upgraded 1.0.0 → 1.1.0")) {
		t.Errorf("stdout = %q, want the from and to versions", stdout)
	}
	if bytes.Contains([]byte(stdout), []byte("SERVERTEXT")) {
		t.Errorf("stdout = %q, relays brew's text", stdout)
	}
}

// TestUpgrade_Apply_InterruptIsNotNetwork pins #761 (c): a Ctrl-C reads as an
// interrupt, whether the runner surfaces context.Canceled or, as os/exec does
// for a killed child, a plain signal exit under a canceled context.
func TestUpgrade_Apply_InterruptIsNotNetwork(t *testing.T) {
	setMetaVersion(t, "1.0.0")
	stubUpgradeLookPath(t, "brew")

	for _, tc := range []struct {
		name      string
		cancelCtx bool
		cause     error
	}{
		{"context.Canceled on the chain", false, context.Canceled},
		{"signal exit under a canceled context", true, errors.New("signal: killed")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fr := &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
				return "", &exec.CommandError{Name: name, Args: args, Err: tc.cause}
			}}
			ctx, cancel := context.WithCancel(context.Background())
			if tc.cancelCtx {
				cancel()
			} else {
				defer cancel()
			}
			cmd := newUpgradeCmd(module.Deps{Cfg: config.Config{}, Runner: fr})
			cmd.SetOut(new(bytes.Buffer))
			cmd.SetArgs(nil)
			err := cmd.ExecuteContext(ctx)
			if err == nil {
				t.Fatal("Execute() = nil, want an error")
			}
			if !bytes.Contains([]byte(err.Error()), []byte("interrupted")) || bytes.Contains([]byte(err.Error()), []byte("network")) {
				t.Errorf("error = %q, want the interrupt wording, not a network fault", err)
			}
		})
	}
}

// A failure with no live stream shows the failed step's last lines on stderr,
// escaped, and names the step; stdout and the error stay fixed text.
func TestUpgrade_Apply_FailureShowsOutputTail(t *testing.T) {
	setMetaVersion(t, "1.0.0")
	stubUpgradeLookPath(t, "brew")
	stubUpgradeTTY(t, false)

	for _, tc := range []struct{ fail, step string }{
		{"update", "brew update"},
		{"upgrade", "brew upgrade --cask " + selfupdate.CaskRef},
	} {
		t.Run(tc.fail, func(t *testing.T) {
			stdout, stderr, err := execUpgradeBoth(t, brewFailing(tc.fail))
			if err == nil || ExitCode(err) != 1 {
				t.Fatalf("err = %v, want exit 1", err)
			}
			if !strings.Contains(stderr, "== last lines of "+tc.step) || !strings.Contains(stderr, "Error: SERVERTEXT") || !strings.Contains(stderr, "==> Downloading") {
				t.Errorf("stderr = %q, want the step name and brew's output tail", stderr)
			}
			if strings.ContainsRune(stderr, 0x1b) || !strings.Contains(stderr, `\x1b`) && !strings.Contains(stderr, `\u001b`) {
				t.Errorf("stderr = %q, want ESC escaped, not raw", stderr)
			}
			if strings.Contains(stdout+err.Error(), "SERVERTEXT") {
				t.Errorf("stdout/error echo brew's text: %q / %q", stdout, err)
			}
		})
	}
}

func TestUpgrade_Apply_TailCappedAtTwentyLines(t *testing.T) {
	setMetaVersion(t, "1.0.0")
	stubUpgradeLookPath(t, "brew")
	stubUpgradeTTY(t, false)
	var b strings.Builder
	for i := 1; i <= 50; i++ {
		b.WriteString("line" + strings.Repeat("x", i%3) + "-" + string(rune('A'+i%26)) + "\n")
	}
	fr := &exec.FakeRunner{RunFunc: func(name string, _ []string) (string, error) {
		return "", &exec.CommandError{Name: name, Output: strings.TrimRight(b.String(), "\n"), ExitCode: 1, Err: errors.New("exit status 1")}
	}}
	_, stderr, _ := execUpgradeBoth(t, fr)
	if got := strings.Count(stderr, "\nline"); got != upgradeTailLines {
		t.Errorf("tail has %d lines, want %d", got, upgradeTailLines)
	}
}

// On a terminal the output streams under a header per step, and a failure does
// not print the tail a second time.
func TestUpgrade_Apply_StreamsOnTTY(t *testing.T) {
	setMetaVersion(t, "1.0.0")
	stubUpgradeLookPath(t, "brew")
	stubUpgradeTTY(t, true)

	fr := &exec.FakeRunner{RunFunc: func(_ string, args []string) (string, error) { return "output of " + args[0], nil }}
	_, stderr, err := execUpgradeBoth(t, fr)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"== brew update", "output of update", "== brew upgrade --cask " + selfupdate.CaskRef, "output of upgrade"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr = %q, want %q", stderr, want)
		}
	}

	_, stderr, err = execUpgradeBoth(t, brewFailing("upgrade"))
	if err == nil {
		t.Fatal("want an error")
	}
	if got := strings.Count(stderr, "Error: SERVERTEXT"); got != 1 {
		t.Errorf("failure output printed %d times, want once (streamed, not repeated as a tail): %q", got, stderr)
	}
	if strings.Contains(stderr, "last lines of") {
		t.Errorf("stderr = %q, repeats the tail after streaming", stderr)
	}
}

func TestUpgrade_JSON_Failure(t *testing.T) {
	setMetaVersion(t, "1.0.0")
	stubUpgradeLookPath(t, "brew")
	stubUpgradeTTY(t, true) // --json must not stream even on a terminal

	stdout, stderr, err := execUpgradeBoth(t, brewFailing("upgrade"), "--json")
	if err == nil || ExitCode(err) != 1 {
		t.Fatalf("err = %v, want exit 1", err)
	}
	var doc upgradeJSON
	if jerr := json.Unmarshal([]byte(stdout), &doc); jerr != nil {
		t.Fatalf("stdout is not JSON: %v\n%q", jerr, stdout)
	}
	if doc.OK || doc.Error == nil {
		t.Fatalf("doc = %+v, want ok=false with an error object", doc)
	}
	e := doc.Error
	if e.Step != "brew upgrade --cask "+selfupdate.CaskRef || e.ExitCode != 1 || e.Message == "" {
		t.Errorf("error = %+v", e)
	}
	if !strings.Contains(e.OutputTail, "Error: SERVERTEXT") || strings.ContainsRune(e.OutputTail, 0x1b) {
		t.Errorf("output_tail = %q, want brew's escaped tail", e.OutputTail)
	}
	if strings.Contains(stderr, "SERVERTEXT") {
		t.Errorf("stderr = %q, --json must not stream or print the tail", stderr)
	}
}

func TestUpgrade_JSON_Success(t *testing.T) {
	setMetaVersion(t, "1.0.0")
	stubUpgradeLookPath(t, "brew")
	fr := &exec.FakeRunner{RunFunc: func(_ string, args []string) (string, error) {
		if args[0] == "upgrade" {
			return "cameronsjo/tap/forgectl 1.0.0 -> 1.1.0", nil
		}
		return "", nil
	}}
	stdout, err := execUpgrade(t, fr, "--json")
	if err != nil {
		t.Fatal(err)
	}
	var doc upgradeJSON
	if jerr := json.Unmarshal([]byte(stdout), &doc); jerr != nil {
		t.Fatalf("stdout is not JSON: %v\n%q", jerr, stdout)
	}
	if !doc.OK || doc.From != "1.0.0" || doc.To != "1.1.0" || doc.Error != nil {
		t.Errorf("doc = %+v", doc)
	}
}

func TestUpgrade_JSON_WithCheckIsUsageError(t *testing.T) {
	setMetaVersion(t, "1.0.0")
	stubUpgradeLookPath(t, "brew")
	_, err := execUpgrade(t, &exec.FakeRunner{}, "--json", "--check")
	if err == nil || ExitCode(err) != exitUsage {
		t.Fatalf("err = %v (exit %d), want a usage error", err, ExitCode(err))
	}
}

// brew exits non-zero though forgectl is installed and current: success.
func TestUpgrade_Apply_AlreadyCurrentIsSuccess(t *testing.T) {
	setMetaVersion(t, "1.0.0")
	stubUpgradeLookPath(t, "brew")
	stubUpgradeTTY(t, false)
	fr := brewFailing("upgrade")
	run := fr.RunFunc
	fr.RunFunc = func(name string, args []string) (string, error) {
		if args[0] == "outdated" {
			return "", nil
		}
		return run(name, args)
	}
	stdout, _, err := execUpgradeBoth(t, fr)
	if err != nil {
		t.Fatalf("err = %v, want success when nothing is outdated", err)
	}
	if !strings.Contains(stdout, "already up to date") {
		t.Errorf("stdout = %q", stdout)
	}
}
