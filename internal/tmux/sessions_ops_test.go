package tmux

import (
	"context"
	"errors"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
)

// argsEqual is a small helper for asserting exact argv.
func argsEqual(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("argv length mismatch: got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("argv[%d]: got %q want %q", i, got[i], want[i])
		}
	}
}

func TestAttachOrSwitch_Inside(t *testing.T) {
	// Inside tmux: must switch-client (non-interactive), never attach.
	fake := &exec.FakeRunner{}
	c := New(fake, WithInsideTmux(func() bool { return true }))

	if err := c.AttachOrSwitch(context.Background(), "alpha"); err != nil {
		t.Fatalf("AttachOrSwitch: %v", err)
	}
	call := fake.Last()
	if call.Interactive {
		t.Errorf("inside tmux must use the non-interactive switch path")
	}
	if call.Name != "tmux" {
		t.Errorf("expected tmux, got %q", call.Name)
	}
	argsEqual(t, call.Args, []string{"switch-client", "-t", "alpha"})
}

func TestAttachOrSwitch_Outside(t *testing.T) {
	// Outside tmux: must attach-session, and it must take the tty (interactive).
	fake := &exec.FakeRunner{}
	c := New(fake, WithInsideTmux(func() bool { return false }))

	if err := c.AttachOrSwitch(context.Background(), "bravo"); err != nil {
		t.Fatalf("AttachOrSwitch: %v", err)
	}
	call := fake.Last()
	if !call.Interactive {
		t.Errorf("outside tmux must use the interactive attach path")
	}
	argsEqual(t, call.Args, []string{"attach-session", "-t", "bravo"})
}

func TestKillSession_Construction(t *testing.T) {
	fake := &exec.FakeRunner{}
	c := New(fake)
	if err := c.KillSession(context.Background(), "doomed"); err != nil {
		t.Fatalf("KillSession: %v", err)
	}
	argsEqual(t, fake.Last().Args, []string{"kill-session", "-t", "doomed"})
}

func TestRenameSession_ArgvOrder(t *testing.T) {
	// The trap: -t targets the OLD name, the trailing bare arg is the NEW name.
	fake := &exec.FakeRunner{}
	c := New(fake)
	if err := c.RenameSession(context.Background(), "old name", "fresh"); err != nil {
		t.Fatalf("RenameSession: %v", err)
	}
	argsEqual(t, fake.Last().Args, []string{"rename-session", "-t", "old name", "fresh"})
}

func TestHasSession_ExitCode(t *testing.T) {
	// Exists: has-session exits 0 → true.
	present := &exec.FakeRunner{RunFunc: func(string, []string) (string, error) { return "", nil }}
	if !New(present).HasSession(context.Background(), "alpha") {
		t.Errorf("expected HasSession true on exit 0")
	}
	// Missing: has-session exits non-zero → false (never string-matched).
	absent := &exec.FakeRunner{RunFunc: func(string, []string) (string, error) {
		return "", errors.New("can't find session: nope")
	}}
	if New(absent).HasSession(context.Background(), "nope") {
		t.Errorf("expected HasSession false on non-zero exit")
	}
	argsEqual(t, present.Last().Args, []string{"has-session", "-t", "alpha"})
}

func TestPick_DelegatesToSesh(t *testing.T) {
	// Pick must shell out to `sesh connect <name>` interactively (it takes the tty).
	fake := &exec.FakeRunner{}
	c := New(fake, WithBins("tmux", "sesh"))
	if err := c.Pick(context.Background(), "projectx"); err != nil {
		t.Fatalf("Pick: %v", err)
	}
	call := fake.Last()
	if call.Name != "sesh" {
		t.Errorf("expected sesh binary, got %q", call.Name)
	}
	if !call.Interactive {
		t.Errorf("sesh connect must take the tty (interactive)")
	}
	argsEqual(t, call.Args, []string{"connect", "projectx"})
}

func TestSeshList_Parse(t *testing.T) {
	fake := &exec.FakeRunner{RunFunc: func(string, []string) (string, error) {
		return "main\nprojectx\n~/Projects/foo", nil
	}}
	c := New(fake, WithBins("tmux", "sesh"))
	names, err := c.SeshList(context.Background())
	if err != nil {
		t.Fatalf("SeshList: %v", err)
	}
	argsEqual(t, names, []string{"main", "projectx", "~/Projects/foo"})
	argsEqual(t, fake.Last().Args, []string{"list"})
}
