package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/exec"
	updatepkg "github.com/cameronsjo/forgectl/internal/update"
)

// fakeUpdateStep builds a minimal Step over name whose Check/Apply each
// shell out to a single command so tests can assert argv via the
// FakeRunner, mirroring internal/update's own fakeStep helper.
func fakeUpdateStep(name string, destructive bool, applyErr error) updatepkg.Step {
	return updatepkg.Step{
		Name:        name,
		Destructive: destructive,
		Check: func(_ context.Context, run exec.Runner) (string, error) {
			return run.Run(context.Background(), name, "check")
		},
		Apply: func(_ context.Context, run exec.Runner) (string, error) {
			_, err := run.Run(context.Background(), name, "apply")
			if err != nil {
				return "", err
			}
			return "", applyErr
		},
	}
}

// runUpdate builds `update` over client/cfg and executes it with args,
// mirroring runPreflight's shape (preflight_test.go).
func runUpdate(t *testing.T, client *updatepkg.Client, cfg config.UpdateConfig, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	cmd := newUpdateCmdForClient(client, cfg)
	var outBuf, errBuf bytes.Buffer
	cmd.SetOut(&outBuf)
	cmd.SetErr(&errBuf)
	cmd.SetArgs(args)
	err = cmd.Execute()
	return outBuf.String(), errBuf.String(), err
}

func TestUpdateCheck_NeverMutatesEvenDestructiveSteps(t *testing.T) {
	fr := &exec.FakeRunner{}
	client := updatepkg.New(fr, updatepkg.WithSteps([]updatepkg.Step{fakeUpdateStep("brew", true, nil)}))
	cfg := config.UpdateConfig{LogDir: t.TempDir()}

	stdout, _, err := runUpdate(t, client, cfg, "check")
	if err != nil {
		t.Fatalf("Execute() = %v (exit %d), want nil", err, ExitCode(err))
	}
	if len(fr.Calls) != 1 || fr.Calls[0].Args[len(fr.Calls[0].Args)-1] != "check" {
		t.Errorf("check invoked something other than the Check command: %+v", fr.Calls)
	}
	if !bytes.Contains([]byte(stdout), []byte("brew")) {
		t.Errorf("stdout summary missing step name: %q", stdout)
	}
}

// stubConfirmSeams overrides isTerminal and confirmUpdateDestructive for the
// duration of the test, restoring both via t.Cleanup — the pattern env_test.go
// already uses for the identical isTerminal/confirmAnyFile seam pair.
func stubConfirmSeams(t *testing.T, terminal bool, confirmFn func(string) (bool, error)) *[]string {
	t.Helper()
	prevTerm := isTerminal
	isTerminal = func() bool { return terminal }
	t.Cleanup(func() { isTerminal = prevTerm })

	var prompts []string
	prevConfirm := confirmUpdateDestructive
	confirmUpdateDestructive = func(msg string) (bool, error) {
		prompts = append(prompts, msg)
		return confirmFn(msg)
	}
	t.Cleanup(func() { confirmUpdateDestructive = prevConfirm })
	return &prompts
}

// TestUpdateRun_NoYes_NoTTY_SkipsWithoutPrompting covers path 1 of 3: no
// --yes and no terminal — the destructive step is skipped, and the
// confirmation seam is never even invoked (there's nothing to ask a
// non-interactive caller).
func TestUpdateRun_NoYes_NoTTY_SkipsWithoutPrompting(t *testing.T) {
	prompts := stubConfirmSeams(t, false, func(string) (bool, error) { return true, nil })

	fr := &exec.FakeRunner{}
	client := updatepkg.New(fr, updatepkg.WithSteps([]updatepkg.Step{fakeUpdateStep("brew", true, nil)}))
	cfg := config.UpdateConfig{LogDir: t.TempDir()}

	stdout, stderr, err := runUpdate(t, client, cfg, "run")
	if err != nil {
		t.Fatalf("Execute() = %v (exit %d), want nil (skip is not a failure)", err, ExitCode(err))
	}
	if len(fr.Calls) != 0 {
		t.Errorf("destructive step's Apply command ran without --yes: %+v", fr.Calls)
	}
	if len(*prompts) != 0 {
		t.Errorf("confirmation prompt fired with no terminal: %v", *prompts)
	}
	if !bytes.Contains([]byte(stdout), []byte("skip")) {
		t.Errorf("stdout summary missing a skip line: %q", stdout)
	}
	if !bytes.Contains([]byte(stderr), []byte("skip")) {
		t.Errorf("stderr transcript missing a skip line: %q", stderr)
	}
}

// TestUpdateRun_NoYes_TTY_ConfirmAccepts_Applies covers path 2 of 3: no
// --yes, a real terminal, and the user accepts the prompt — the destructive
// step applies exactly as if --yes had been passed.
func TestUpdateRun_NoYes_TTY_ConfirmAccepts_Applies(t *testing.T) {
	prompts := stubConfirmSeams(t, true, func(string) (bool, error) { return true, nil })

	fr := &exec.FakeRunner{}
	client := updatepkg.New(fr, updatepkg.WithSteps([]updatepkg.Step{fakeUpdateStep("brew", true, nil)}))
	cfg := config.UpdateConfig{LogDir: t.TempDir()}

	stdout, _, err := runUpdate(t, client, cfg, "run")
	if err != nil {
		t.Fatalf("Execute() = %v (exit %d), want nil", err, ExitCode(err))
	}
	if len(fr.Calls) != 1 || fr.Calls[0].Args[len(fr.Calls[0].Args)-1] != "apply" {
		t.Errorf("expected the Apply command to run once after accepting the prompt, got %+v", fr.Calls)
	}
	if len(*prompts) != 1 {
		t.Fatalf("got %d prompt(s), want exactly 1", len(*prompts))
	}
	if !bytes.Contains([]byte((*prompts)[0]), []byte("brew")) {
		t.Errorf("prompt %q does not name the affected step", (*prompts)[0])
	}
	if !bytes.Contains([]byte(stdout), []byte("1 ok, 0 skipped, 0 failed")) {
		t.Errorf("stdout summary shows a skip despite the prompt being accepted: %q", stdout)
	}
}

// TestUpdateRun_NoYes_TTY_ConfirmDeclines_Skips covers path 3 of 3: no
// --yes, a real terminal, and the user declines — the destructive step is
// skipped exactly as it would be with no terminal at all.
func TestUpdateRun_NoYes_TTY_ConfirmDeclines_Skips(t *testing.T) {
	prompts := stubConfirmSeams(t, true, func(string) (bool, error) { return false, nil })

	fr := &exec.FakeRunner{}
	client := updatepkg.New(fr, updatepkg.WithSteps([]updatepkg.Step{fakeUpdateStep("brew", true, nil)}))
	cfg := config.UpdateConfig{LogDir: t.TempDir()}

	stdout, _, err := runUpdate(t, client, cfg, "run")
	if err != nil {
		t.Fatalf("Execute() = %v (exit %d), want nil (a decline is not a failure)", err, ExitCode(err))
	}
	if len(fr.Calls) != 0 {
		t.Errorf("destructive step's Apply command ran despite declining the prompt: %+v", fr.Calls)
	}
	if len(*prompts) != 1 {
		t.Fatalf("got %d prompt(s), want exactly 1", len(*prompts))
	}
	if !bytes.Contains([]byte(stdout), []byte("skip")) {
		t.Errorf("stdout summary missing a skip line after declining: %q", stdout)
	}
}

// TestUpdateRun_NoYes_TTY_ConfirmPromptErrors_FailsClosedToSkip asserts that
// a prompt failure (e.g. huh erroring out despite isTerminal() reporting
// true) resolves to a skip, never a harness error — the same fail-closed
// behavior as declining.
func TestUpdateRun_NoYes_TTY_ConfirmPromptErrors_FailsClosedToSkip(t *testing.T) {
	stubConfirmSeams(t, true, func(string) (bool, error) { return false, errors.New("no tty available") })

	fr := &exec.FakeRunner{}
	client := updatepkg.New(fr, updatepkg.WithSteps([]updatepkg.Step{fakeUpdateStep("brew", true, nil)}))
	cfg := config.UpdateConfig{LogDir: t.TempDir()}

	_, stderr, err := runUpdate(t, client, cfg, "run")
	if err != nil {
		t.Fatalf("Execute() = %v (exit %d), want nil (a failed prompt fails closed, not as a harness error)", err, ExitCode(err))
	}
	if len(fr.Calls) != 0 {
		t.Errorf("destructive step's Apply command ran despite a failed prompt: %+v", fr.Calls)
	}
	if !bytes.Contains([]byte(stderr), []byte("warning")) {
		t.Errorf("stderr missing a warning about the failed prompt: %q", stderr)
	}
}

// TestUpdateRun_YesAppliesDestructiveSteps confirms --yes both applies the
// destructive step AND never invokes the confirmation seam at all (explicit
// consent short-circuits the prompt entirely, even on a real terminal).
func TestUpdateRun_YesAppliesDestructiveSteps(t *testing.T) {
	prompts := stubConfirmSeams(t, true, func(string) (bool, error) {
		t.Fatal("confirmation prompt must not fire when --yes is passed")
		return false, nil
	})

	fr := &exec.FakeRunner{}
	client := updatepkg.New(fr, updatepkg.WithSteps([]updatepkg.Step{fakeUpdateStep("brew", true, nil)}))
	cfg := config.UpdateConfig{LogDir: t.TempDir()}

	_, _, err := runUpdate(t, client, cfg, "run", "--yes")
	if err != nil {
		t.Fatalf("Execute() = %v (exit %d), want nil", err, ExitCode(err))
	}
	if len(fr.Calls) != 1 || fr.Calls[0].Args[len(fr.Calls[0].Args)-1] != "apply" {
		t.Errorf("expected the Apply command to run once, got %+v", fr.Calls)
	}
	if len(*prompts) != 0 {
		t.Errorf("confirmation prompt fired despite --yes: %v", *prompts)
	}
}

func TestUpdateRun_FailedStepExitsOne(t *testing.T) {
	fr := &exec.FakeRunner{}
	client := updatepkg.New(fr, updatepkg.WithSteps([]updatepkg.Step{
		fakeUpdateStep("ok", false, nil),
		fakeUpdateStep("broken", true, errors.New("boom")),
	}))
	cfg := config.UpdateConfig{LogDir: t.TempDir()}

	stdout, _, err := runUpdate(t, client, cfg, "run", "--yes")
	if ExitCode(err) != 1 {
		t.Errorf("ExitCode = %d, want 1", ExitCode(err))
	}
	if !bytes.Contains([]byte(stdout), []byte("ok")) {
		t.Errorf("sibling step's ok result missing from summary despite the other step failing: %q", stdout)
	}
	if !bytes.Contains([]byte(stdout), []byte("FAIL")) {
		t.Errorf("stdout summary missing the FAIL line: %q", stdout)
	}
}

func TestUpdateRun_OnlyRestrictsRoster(t *testing.T) {
	fr := &exec.FakeRunner{}
	client := updatepkg.New(fr, updatepkg.WithSteps([]updatepkg.Step{
		fakeUpdateStep("brew", false, nil),
		fakeUpdateStep("go", false, nil),
		fakeUpdateStep("npm", false, nil),
	}))
	cfg := config.UpdateConfig{LogDir: t.TempDir()}

	_, _, err := runUpdate(t, client, cfg, "run", "--yes", "--only", "brew,go")
	if err != nil {
		t.Fatalf("Execute() = %v (exit %d), want nil", err, ExitCode(err))
	}
	if len(fr.Calls) != 2 {
		t.Fatalf("got %d calls, want 2 (only brew+go)", len(fr.Calls))
	}
	names := map[string]bool{fr.Calls[0].Name: true, fr.Calls[1].Name: true}
	if !names["brew"] || !names["go"] || names["npm"] {
		t.Errorf("wrong step subset ran: %+v", fr.Calls)
	}
}

func TestUpdateRun_UnknownOnlyNameExitsTwo(t *testing.T) {
	fr := &exec.FakeRunner{}
	client := updatepkg.New(fr, updatepkg.WithSteps([]updatepkg.Step{fakeUpdateStep("brew", false, nil)}))
	cfg := config.UpdateConfig{LogDir: t.TempDir()}

	_, _, err := runUpdate(t, client, cfg, "run", "--only", "not-a-real-step")
	if ExitCode(err) != 2 {
		t.Errorf("ExitCode = %d, want 2", ExitCode(err))
	}
	if len(fr.Calls) != 0 {
		t.Errorf("no step should have run against an invalid --only: %+v", fr.Calls)
	}
}

func TestUpdateRun_JSONStdoutIsByteClean(t *testing.T) {
	fr := &exec.FakeRunner{}
	client := updatepkg.New(fr, updatepkg.WithSteps([]updatepkg.Step{
		fakeUpdateStep("brew", true, nil),
		fakeUpdateStep("npm", true, errors.New("boom")),
	}))
	cfg := config.UpdateConfig{LogDir: t.TempDir()}

	stdout, stderr, err := runUpdate(t, client, cfg, "run", "--yes", "--json")
	if ExitCode(err) != 1 {
		t.Errorf("ExitCode = %d, want 1", ExitCode(err))
	}

	var report struct {
		Steps []struct {
			Name        string `json:"name"`
			Destructive bool   `json:"destructive"`
			Skipped     bool   `json:"skipped"`
			Failed      bool   `json:"failed"`
			Error       string `json:"error"`
		} `json:"steps"`
		Ok      int `json:"ok"`
		Skipped int `json:"skipped"`
		Failed  int `json:"failed"`
	}
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("stdout is not clean JSON: %v\nstdout=%q", err, stdout)
	}
	if report.Ok != 1 || report.Failed != 1 || report.Skipped != 0 {
		t.Errorf("got ok=%d failed=%d skipped=%d, want ok=1 failed=1 skipped=0", report.Ok, report.Failed, report.Skipped)
	}
	if len(report.Steps) != 2 {
		t.Fatalf("got %d steps, want 2", len(report.Steps))
	}
	// The transcript (step lines, log-path notice) belongs on stderr, never
	// mixed into the JSON stdout stream.
	if len(stderr) == 0 {
		t.Error("expected a non-empty stderr transcript alongside --json stdout")
	}
}

func TestUpdateRun_WritesTimestampedLogFile(t *testing.T) {
	fr := &exec.FakeRunner{}
	client := updatepkg.New(fr, updatepkg.WithSteps([]updatepkg.Step{fakeUpdateStep("brew", false, nil)}))
	logDir := t.TempDir()
	cfg := config.UpdateConfig{LogDir: logDir}

	if _, _, err := runUpdate(t, client, cfg, "run"); err != nil {
		t.Fatalf("Execute() = %v (exit %d), want nil", err, ExitCode(err))
	}

	entries, err := os.ReadDir(logDir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d log file(s) in %s, want 1", len(entries), logDir)
	}
	contents, err := os.ReadFile(filepath.Join(logDir, entries[0].Name()))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Contains(contents, []byte("brew")) {
		t.Errorf("log file missing the step's transcript line: %q", string(contents))
	}
}

func TestUpdateCheck_DefaultConfig_NoLogDirStillSucceeds(t *testing.T) {
	// A zero-value UpdateConfig (no [update] section) must still run —
	// openTranscript falls back to config.UpdateLogDir(), which itself must
	// not error just because $HOME/config dir resolution differs in test
	// environments; redirect HOME to a writable temp dir so the fallback
	// path is exercised rather than skipped.
	t.Setenv("HOME", t.TempDir())
	fr := &exec.FakeRunner{}
	client := updatepkg.New(fr, updatepkg.WithSteps([]updatepkg.Step{fakeUpdateStep("brew", false, nil)}))

	if _, _, err := runUpdate(t, client, config.UpdateConfig{}, "check"); err != nil {
		t.Fatalf("Execute() = %v (exit %d), want nil", err, ExitCode(err))
	}
}
