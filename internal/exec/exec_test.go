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

import (
	"context"
	"errors"
	"testing"
)

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
