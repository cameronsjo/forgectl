package exec

// Test plan for exec.go / fake.go
//
// OSRunner.RunWithInput (Classification: ops layer)
//   [x] Happy: stdin is actually piped into the child process (real `cat`
//       subprocess echoes it back on stdout)
//   [x] Unhappy: a failing command surfaces a *CommandError wrapping stderr
//
// FakeRunner.RunWithInput (Classification: test double)
//   [x] Happy: the call is recorded on Calls with Input set to the piped stdin
//   [x] Happy: RunFunc still produces the canned (name, args)-keyed output
//
// OSRunner.RunWithEnvFiltered (Classification: subprocess environment seam)
//   [x] Invariant: an inherited variable is absent, not merely empty
//   [x] Control: the old empty override remains observably present
//   [x] Boundary: an explicit override wins over removal of the same name
//
// FakeRunner.RunWithEnvFiltered (Classification: test double)
//   [x] Happy: both overrides and removals are observable on the recorded Call

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestOSRunner_RunStreaming_ConnectsStreamsWithoutBuffering(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := (OSRunner{}).RunStreaming(
		context.Background(),
		strings.NewReader("from stdin"),
		&stdout,
		&stderr,
		"sh", "-c", "IFS= read -r line; printf 'out:%s' \"$line\"; printf 'err:%s' \"$line\" >&2",
	)
	if err != nil {
		t.Fatalf("RunStreaming: %v", err)
	}
	if stdout.String() != "out:from stdin" || stderr.String() != "err:from stdin" {
		t.Errorf("streams not connected: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestOSRunner_RunStreaming_PreservesExitCodeWithoutRetainingArgv(t *testing.T) {
	err := (OSRunner{}).RunStreaming(context.Background(), nil, io.Discard, io.Discard, "sh", "-c", "exit 7", "secret-token")
	var cmdErr *CommandError
	if !errors.As(err, &cmdErr) {
		t.Fatalf("error = %T %v, want *CommandError", err, err)
	}
	if cmdErr.ExitCode != 7 {
		t.Errorf("ExitCode = %d, want 7", cmdErr.ExitCode)
	}
	if len(cmdErr.Args) != 0 || strings.Contains(err.Error(), "secret-token") {
		t.Errorf("streaming error retained argv: Args=%q error=%q", cmdErr.Args, err)
	}
}

func TestOSRunner_RunStreaming_PreservesCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := (OSRunner{}).RunStreaming(ctx, nil, io.Discard, io.Discard, "sh", "-c", "exit 0")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

func TestOSRunner_RunWithInput_PipesStdinToChild(t *testing.T) {
	out, err := (OSRunner{}).RunWithInput(context.Background(), "hello from stdin", "cat")
	if err != nil {
		t.Fatalf("RunWithInput: %v", err)
	}
	if out != "hello from stdin" {
		t.Errorf("RunWithInput output = %q, want %q", out, "hello from stdin")
	}
}

func TestOSRunner_RunWithInput_FailingCommand_WrapsStderr(t *testing.T) {
	_, err := (OSRunner{}).RunWithInput(context.Background(), "irrelevant", "sh", "-c", "cat >/dev/null; echo boom >&2; exit 1")
	if err == nil {
		t.Fatal("expected an error from a nonzero exit")
	}
	var cmdErr *CommandError
	if !errors.As(err, &cmdErr) {
		t.Fatalf("error = %T, want *CommandError", err)
	}
	if cmdErr.Stderr != "boom" {
		t.Errorf("CommandError.Stderr = %q, want %q", cmdErr.Stderr, "boom")
	}
}

// TestOSRunner_RunWithEnv_SetsEnvironmentVariable proves the mechanism the
// HOMEBREW_NO_AUTO_UPDATE fix depends on: RunWithEnv's env map must actually
// reach the child process, not just get recorded by a test double. Real
// subprocess, no FakeRunner.
func TestOSRunner_RunWithEnv_SetsEnvironmentVariable(t *testing.T) {
	out, err := (OSRunner{}).RunWithEnv(context.Background(), map[string]string{"FORGECTL_TEST_VAR": "present"}, "sh", "-c", "echo $FORGECTL_TEST_VAR")
	if err != nil {
		t.Fatalf("RunWithEnv: %v", err)
	}
	if out != "present" {
		t.Errorf("RunWithEnv output = %q, want %q", out, "present")
	}
}

// TestOSRunner_RunWithEnv_InheritsAmbientEnvironment confirms RunWithEnv
// merges onto the inherited environment (os.Environ()) rather than
// replacing it — the child must still see PATH etc.
func TestOSRunner_RunWithEnv_InheritsAmbientEnvironment(t *testing.T) {
	t.Setenv("FORGECTL_TEST_AMBIENT", "inherited")
	out, err := (OSRunner{}).RunWithEnv(context.Background(), map[string]string{"FORGECTL_TEST_VAR": "present"}, "sh", "-c", "echo $FORGECTL_TEST_AMBIENT-$FORGECTL_TEST_VAR")
	if err != nil {
		t.Fatalf("RunWithEnv: %v", err)
	}
	if out != "inherited-present" {
		t.Errorf("RunWithEnv output = %q, want %q", out, "inherited-present")
	}
}

// TestOSRunner_RunWithEnv_OverridesAmbientValue pins the case the merge test
// above cannot reach: an appended var whose key ALREADY exists in the ambient
// environment must win. That is exactly why brewStep pins
// HOMEBREW_NO_AUTO_UPDATE=1 — a user who exported it as 0 would otherwise keep
// the auto-update mutation the pin exists to stop.
func TestOSRunner_RunWithEnv_OverridesAmbientValue(t *testing.T) {
	t.Setenv("FORGECTL_TEST_OVERRIDE", "ambient")

	out, err := (OSRunner{}).RunWithEnv(context.Background(), map[string]string{"FORGECTL_TEST_OVERRIDE": "appended"}, "sh", "-c", "echo $FORGECTL_TEST_OVERRIDE")
	if err != nil {
		t.Fatalf("RunWithEnv: %v", err)
	}
	if out != "appended" {
		t.Errorf("appended value did not win over the ambient one: got %q, want %q", out, "appended")
	}

	// Control: without the append, the child sees the ambient value — so the
	// assertion above is testing the override, not a coincidence.
	ctrl, err := (OSRunner{}).Run(context.Background(), "sh", "-c", "echo $FORGECTL_TEST_OVERRIDE")
	if err != nil {
		t.Fatalf("Run (control): %v", err)
	}
	if ctrl != "ambient" {
		t.Errorf("control output = %q, want %q", ctrl, "ambient")
	}
}

// TestOSRunner_RunWithEnvFiltered_RemovesRatherThanEmpties is the production
// process-boundary proof for #413. The shell's ${name+x} expansion distinguishes
// an absent variable from one whose value is merely empty, so this assertion
// cannot pass through the old empty-string scrub.
func TestOSRunner_RunWithEnvFiltered_RemovesRatherThanEmpties(t *testing.T) {
	const (
		remove = "FORGECTL_TEST_FILTERED_REMOVE"
		keep   = "FORGECTL_TEST_FILTERED_KEEP"
	)
	t.Setenv(remove, "ambient-secret")
	t.Setenv(keep, "ambient-value")

	out, err := (OSRunner{}).RunWithEnvFiltered(t.Context(),
		map[string]string{keep: "overridden"}, []string{remove},
		"sh", "-c", `if [ "${FORGECTL_TEST_FILTERED_REMOVE+x}" = x ]; then printf present; else printf absent; fi; printf ':%s' "$FORGECTL_TEST_FILTERED_KEEP"`)
	if err != nil {
		t.Fatalf("RunWithEnvFiltered: %v", err)
	}
	if out != "absent:overridden" {
		t.Fatalf("filtered child environment = %q, want %q", out, "absent:overridden")
	}

	control, err := (OSRunner{}).RunWithEnv(t.Context(), map[string]string{remove: ""},
		"sh", "-c", `if [ "${FORGECTL_TEST_FILTERED_REMOVE+x}" = x ]; then printf present; else printf absent; fi`)
	if err != nil {
		t.Fatalf("RunWithEnv control: %v", err)
	}
	if control != "present" {
		t.Fatalf("empty-string control = %q, want present — verifier did not distinguish empty from absent", control)
	}
}

func TestOSRunner_RunWithEnvFiltered_OverrideWinsSameNameRemoval(t *testing.T) {
	const name = "FORGECTL_TEST_FILTERED_REPLACE"
	t.Setenv(name, "ambient")

	out, err := (OSRunner{}).RunWithEnvFiltered(t.Context(),
		map[string]string{name: "replacement"}, []string{name},
		"sh", "-c", `printf '%s' "$FORGECTL_TEST_FILTERED_REPLACE"`)
	if err != nil {
		t.Fatalf("RunWithEnvFiltered: %v", err)
	}
	if out != "replacement" {
		t.Fatalf("filtered override = %q, want replacement", out)
	}
}

func TestFakeRunner_RunWithEnvFiltered_RecordsOverridesAndRemovals(t *testing.T) {
	fake := &FakeRunner{}

	if _, err := fake.RunWithEnvFiltered(t.Context(),
		map[string]string{"KEEP": "value"}, []string{"REMOVE"}, "gh", "api", "user"); err != nil {
		t.Fatalf("RunWithEnvFiltered: %v", err)
	}

	call := fake.Last()
	if got := call.Env["KEEP"]; got != "value" {
		t.Errorf("call.Env[KEEP] = %q, want value", got)
	}
	if len(call.UnsetEnv) != 1 || call.UnsetEnv[0] != "REMOVE" {
		t.Errorf("call.UnsetEnv = %v, want [REMOVE]", call.UnsetEnv)
	}
}

func TestFakeRunner_RunWithInput_RecordsInputOnCall(t *testing.T) {
	fake := &FakeRunner{}

	if _, err := fake.RunWithInput(context.Background(), "clipboard payload", "pbcopy"); err != nil {
		t.Fatalf("RunWithInput: %v", err)
	}

	call := fake.Last()
	if call.Name != "pbcopy" {
		t.Errorf("call.Name = %q, want %q", call.Name, "pbcopy")
	}
	if call.Input != "clipboard payload" {
		t.Errorf("call.Input = %q, want %q", call.Input, "clipboard payload")
	}
	if call.Interactive {
		t.Errorf("call.Interactive = true, want false (RunWithInput is not the interactive path)")
	}
}

func TestFakeRunner_RunWithInput_UsesRunFunc(t *testing.T) {
	fake := &FakeRunner{
		RunFunc: func(name string, args []string) (string, error) {
			if name == "pbcopy" {
				return "canned output", nil
			}
			return "", nil
		},
	}

	out, err := fake.RunWithInput(context.Background(), "anything", "pbcopy")
	if err != nil {
		t.Fatalf("RunWithInput: %v", err)
	}
	if out != "canned output" {
		t.Errorf("RunWithInput output = %q, want %q", out, "canned output")
	}
}
