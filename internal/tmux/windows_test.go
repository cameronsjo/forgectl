package tmux

import (
	"context"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
)

// TestWindowFormatCarriesIdentityPrefix binds the two halves of dispatch
// verification together WITHOUT tmux: internal/pr captures a dispatch identity
// with `new-window -P -F tmux.IdentityFormat`, then matches it against the
// first three fields of a list-windows row. If windowFormat ever stopped
// leading with IdentityFormat — a reordered or inserted field — VerifyDispatched
// would match nothing and report every healthy review gone. The existing
// real-tmux matrix would catch it, but it skips wherever tmux is absent, so this
// is the assertion that always runs.
func TestWindowFormatCarriesIdentityPrefix(t *testing.T) {
	if !strings.HasPrefix(windowFormat, IdentityFormat+FieldSep) {
		t.Fatalf("windowFormat = %q, want prefix %q", windowFormat, IdentityFormat+FieldSep)
	}
	identityFields := strings.Split(IdentityFormat, FieldSep)
	if len(identityFields) != 3 {
		t.Fatalf("IdentityFormat = %q, want 3 fields", IdentityFormat)
	}
	// parseWindows reads f[0..2] as ServerPID/ServerStart/ID, and admission
	// rejoins them with FieldSep to rebuild the captured identity. Prove that
	// round trip on a real row rather than trusting the constants alone.
	row := strings.Join([]string{"123", "456", "@7", "reviews", "1", "pr-o-r-1", "0", "1"}, FieldSep)
	windows := parseWindows(row)
	if len(windows) != 1 {
		t.Fatalf("parseWindows(%q) = %d rows, want 1", row, len(windows))
	}
	got := strings.Join([]string{windows[0].ServerPID, windows[0].ServerStart, windows[0].ID}, FieldSep)
	if want := "123" + FieldSep + "456" + FieldSep + "@7"; got != want {
		t.Errorf("rebuilt identity = %q, want %q", got, want)
	}
}

func TestParseWindows(t *testing.T) {
	out := "123" + sep + "456" + sep + "@0" + sep + "main" + sep + "0" + sep + "editor" + sep + "1" + sep + "2" + "\n" +
		"123" + sep + "456" + sep + "@1" + sep + "main" + sep + "1" + sep + "my window" + sep + "0" + sep + "1" + "\n" +
		"789" + sep + "999" + sep + "@0" + sep + "work" + sep + "0" + sep + "shell" + sep + "1" + sep + "1"
	got := parseWindows(out)
	if len(got) != 3 {
		t.Fatalf("expected 3 windows, got %d", len(got))
	}
	// Target must be pre-built as session:index.
	if got[0].Target != "main:0" {
		t.Errorf("window 0 target: got %q want main:0", got[0].Target)
	}
	if got[0].ServerPID != "123" || got[0].ServerStart != "456" || got[0].ID != "@0" {
		t.Errorf("window 0 identity wrong: %+v", got[0])
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

func TestLastSession_OutsideAllZeroTimestamps(t *testing.T) {
	// Every session never-attached (ts=0). The -1 sentinel means the first row
	// still wins (0 > -1), so we attach deterministically rather than to "".
	fake := &exec.FakeRunner{RunFunc: func(string, []string) (string, error) {
		return "0" + sep + "first" + "\n" + "0" + sep + "second", nil
	}}
	c := New(fake, WithInsideTmux(func() bool { return false }))
	if err := c.LastSession(context.Background()); err != nil {
		t.Fatalf("LastSession: %v", err)
	}
	argsEqual(t, fake.Last().Args, []string{"attach-session", "-t", "first"})
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
