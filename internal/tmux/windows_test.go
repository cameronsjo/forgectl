package tmux

import (
	"context"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
)

func TestParseWindows(t *testing.T) {
	out := "main" + sep + "0" + sep + "editor" + sep + "1" + sep + "2" + "\n" +
		"main" + sep + "1" + sep + "my window" + sep + "0" + sep + "1" + "\n" +
		"work" + sep + "0" + sep + "shell" + sep + "1" + sep + "1"
	got := parseWindows(out)
	if len(got) != 3 {
		t.Fatalf("expected 3 windows, got %d", len(got))
	}
	// Target must be pre-built as session:index.
	if got[0].Target != "main:0" {
		t.Errorf("window 0 target: got %q want main:0", got[0].Target)
	}
	if !got[0].Active || got[0].Panes != 2 {
		t.Errorf("window 0 wrong: %+v", got[0])
	}
	// Name with a space survives.
	if got[1].Name != "my window" || got[1].Target != "main:1" {
		t.Errorf("window 1 wrong: %+v", got[1])
	}
	if got[1].Active {
		t.Errorf("window 1 should be inactive")
	}
}

func TestParsePanes(t *testing.T) {
	out := "main" + sep + "0" + sep + "0" + sep + "title one" + sep + "nvim" + sep + "1" + "\n" +
		"main" + sep + "0" + sep + "1" + sep + "title two" + sep + "zsh" + sep + "0"
	got := parsePanes(out)
	if len(got) != 2 {
		t.Fatalf("expected 2 panes, got %d", len(got))
	}
	if got[0].Target != "main:0.0" || got[0].Command != "nvim" || !got[0].Active {
		t.Errorf("pane 0 wrong: %+v", got[0])
	}
	if got[1].Target != "main:0.1" || got[1].Active {
		t.Errorf("pane 1 wrong: %+v", got[1])
	}
}

func TestListWindows_Construction(t *testing.T) {
	fake := &exec.FakeRunner{RunFunc: func(string, []string) (string, error) { return "", nil }}
	c := New(fake)
	if _, err := c.ListWindows(context.Background()); err != nil {
		t.Fatalf("ListWindows: %v", err)
	}
	argsEqual(t, fake.Last().Args, []string{"list-windows", "-a", "-F", windowFormat})
}

func TestBuildTree(t *testing.T) {
	// Deliberately out-of-order input to prove sorting (work before main;
	// window 1 before 0; pane 1 before 0).
	sessions := []Session{
		{Name: "work", Attached: false},
		{Name: "main", Attached: true},
	}
	windows := []Window{
		{Session: "main", Index: 1, Name: "server", Active: false, Panes: 1},
		{Session: "main", Index: 0, Name: "editor", Active: true, Panes: 2},
		{Session: "work", Index: 0, Name: "shell", Active: true, Panes: 1},
	}
	panes := []Pane{
		{Session: "main", Window: 0, Index: 1, Command: "zsh", Active: false},
		{Session: "main", Window: 0, Index: 0, Command: "nvim", Active: true},
	}

	got := buildTree(sessions, windows, panes, iconTreeMarkers)
	want := strings.Join([]string{
		"● main",
		"  0: editor * (2 panes)",
		"    0: nvim *",
		"    1: zsh",
		"  1: server (1 pane)",
		"○ work",
		"  0: shell * (1 pane)",
	}, "\n")
	if got != want {
		t.Errorf("buildTree mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestBuildTree_ASCIIMarkers(t *testing.T) {
	sessions := []Session{{Name: "main", Attached: true}}
	got := buildTree(sessions, nil, nil, asciiTreeMarkers)
	if !strings.HasPrefix(got, "* main") {
		t.Errorf("ASCII markers: expected '* main' prefix, got %q", got)
	}
}

func TestJumpToWindow_InsideUsesSwitchClient(t *testing.T) {
	fake := &exec.FakeRunner{}
	c := New(fake, WithInsideTmux(func() bool { return true }))
	if err := c.JumpToWindow(context.Background(), "work:2"); err != nil {
		t.Fatalf("JumpToWindow: %v", err)
	}
	call := fake.Last()
	if call.Interactive {
		t.Errorf("inside tmux a window jump must switch-client, not attach")
	}
	argsEqual(t, call.Args, []string{"switch-client", "-t", "work:2"})
}

func TestKillOthers_Construction(t *testing.T) {
	fake := &exec.FakeRunner{}
	c := New(fake)
	if err := c.KillOthers(context.Background(), "keepme"); err != nil {
		t.Fatalf("KillOthers: %v", err)
	}
	argsEqual(t, fake.Last().Args, []string{"kill-session", "-a", "-t", "keepme"})
}

func TestLastSession_Inside(t *testing.T) {
	fake := &exec.FakeRunner{}
	c := New(fake, WithInsideTmux(func() bool { return true }))
	if err := c.LastSession(context.Background()); err != nil {
		t.Fatalf("LastSession: %v", err)
	}
	call := fake.Last()
	if call.Interactive {
		t.Errorf("inside tmux, last must switch-client (non-interactive)")
	}
	argsEqual(t, call.Args, []string{"switch-client", "-l"})
}

func TestLastSession_OutsideAttachesMostRecent(t *testing.T) {
	// Outside tmux: pick the greatest session_last_attached, then attach.
	fake := &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		return "100" + sep + "older" + "\n" + "200" + sep + "newest" + "\n" + "150" + sep + "middle", nil
	}}
	c := New(fake, WithInsideTmux(func() bool { return false }))
	if err := c.LastSession(context.Background()); err != nil {
		t.Fatalf("LastSession: %v", err)
	}
	call := fake.Last()
	if !call.Interactive {
		t.Errorf("outside tmux, last must attach (interactive)")
	}
	argsEqual(t, call.Args, []string{"attach-session", "-t", "newest"})
}
