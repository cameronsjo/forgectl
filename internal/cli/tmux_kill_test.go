package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/tmux"
)

// existsRunner returns ("", nil) for all calls — HasSession returns true,
// KillSession / KillOthers succeed.
func existsRunner() *exec.FakeRunner {
	return &exec.FakeRunner{}
}

// absentRunner returns an error for all calls — HasSession returns false.
func absentRunner() *exec.FakeRunner {
	return &exec.FakeRunner{
		RunFunc: func(name string, args []string) (string, error) {
			return "", &exec.CommandError{Name: name, Args: args, Stderr: "no server running"}
		},
	}
}

func TestKillCmd_YesFlagSkipsConfirm(t *testing.T) {
	fake := existsRunner()
	client := tmux.New(fake)
	cmd := newTmuxKillCmd(client)

	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--yes", "mysession"})

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// kill-session must have been called
	found := false
	for _, c := range fake.Calls {
		if len(c.Args) > 0 && c.Args[0] == "kill-session" {
			found = true
		}
	}
	if !found {
		t.Errorf("kill-session never called; calls: %v", fake.Calls)
	}
	if !strings.Contains(out.String(), "killed mysession") {
		t.Errorf("output missing confirmation: %q", out.String())
	}
}

func TestKillCmd_OthersFlagRoutesToKillOthers(t *testing.T) {
	fake := existsRunner()
	client := tmux.New(fake)
	cmd := newTmuxKillCmd(client)

	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--yes", "--others", "mysession"})

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out.String(), "except mysession") {
		t.Errorf("output missing --others message: %q", out.String())
	}
}

func TestKillCmd_MissingSessionErrors(t *testing.T) {
	client := tmux.New(absentRunner())
	cmd := newTmuxKillCmd(client)
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetArgs([]string{"--yes", "nosuch"})

	err := cmd.ExecuteContext(context.Background())
	if err == nil {
		t.Fatal("expected error for missing session, got nil")
	}
	if !strings.Contains(err.Error(), "no such session") {
		t.Errorf("error message = %q, want it to mention no such session", err.Error())
	}
}
