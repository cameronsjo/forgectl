package tmux

import (
	"context"
	"errors"
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
	row := strings.Join([]string{"123", "456", "@7", "$3", "reviews", "1", "pr-o-r-1", "0", "1"}, FieldSep)
	windows, err := parseWindows(row)
	if err != nil {
		t.Fatalf("parseWindows(%q): %v", row, err)
	}
	if len(windows) != 1 {
		t.Fatalf("parseWindows(%q) = %d rows, want 1", row, len(windows))
	}
	got := strings.Join([]string{windows[0].ServerPID, windows[0].ServerStart, windows[0].ID}, FieldSep)
	if want := "123" + FieldSep + "456" + FieldSep + "@7"; got != want {
		t.Errorf("rebuilt identity = %q, want %q", got, want)
	}
}

func TestParseWindows(t *testing.T) {
	out := "123" + sep + "456" + sep + "@0" + sep + "$1" + sep + "main" + sep + "0" + sep + "editor" + sep + "1" + sep + "2" + "\n" +
		"123" + sep + "456" + sep + "@1" + sep + "$1" + sep + "main" + sep + "1" + sep + "my window" + sep + "0" + sep + "1" + "\n" +
		"789" + sep + "999" + sep + "@0" + sep + "$1" + sep + "work" + sep + "0" + sep + "shell" + sep + "1" + sep + "1"
	got, err := parseWindows(out)
	if err != nil {
		t.Fatalf("parseWindows: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 windows, got %d", len(got))
	}
	if got[0].ServerPID != "123" || got[0].ServerStart != "456" || got[0].ID != "@0" || got[0].SessionID != "$1" {
		t.Errorf("window 0 identity wrong: %+v", got[0])
	}
	if got[0].Session != "main" || got[0].Index != 0 {
		t.Errorf("window 0 display fields wrong: %+v", got[0])
	}
	if !got[0].Active || got[0].Panes != 2 {
		t.Errorf("window 0 wrong: %+v", got[0])
	}
	// Name with a space survives.
	if got[1].Name != "my window" || got[1].Index != 1 {
		t.Errorf("window 1 wrong: %+v", got[1])
	}
	if got[1].Active {
		t.Errorf("window 1 should be inactive")
	}
}

func TestParsePanes(t *testing.T) {
	out := "123" + sep + "456" + sep + "%0" + sep + "@0" + sep + "0" + sep + "title one" + sep + "nvim" + sep + "1" + "\n" +
		"123" + sep + "456" + sep + "%1" + sep + "@0" + sep + "1" + sep + "title two" + sep + "zsh" + sep + "0"
	got, err := parsePanes(out)
	if err != nil {
		t.Fatalf("parsePanes: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 panes, got %d", len(got))
	}
	if got[0].ID != "%0" || got[0].WindowID != "@0" || got[0].Command != "nvim" || !got[0].Active {
		t.Errorf("pane 0 wrong: %+v", got[0])
	}
	if got[1].ID != "%1" || got[1].WindowID != "@0" || got[1].Active {
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

func TestNewWindow_RevalidatesSessionAndReturnsCommandIdentity(t *testing.T) {
	sessionRow := strings.Join([]string{"123", "456", "$1", "forge", "1", "0", "0", "/repo"}, FieldSep)
	fake := &exec.FakeRunner{RunFunc: func(_ string, args []string) (string, error) {
		switch args[0] {
		case "list-sessions":
			return sessionRow, nil
		case "new-window":
			// tmux 3.5a and older render FieldSep back as this printable
			// octal escape. NewWindow owns normalization and validation.
			return `123\037456\037@7`, nil
		}
		return "", nil
	}}
	c := New(fake)
	identityEnv(c, "", "/tmp")
	session := SessionIdentity{
		Generation: ServerGeneration{Selector: ServerSelector{TmpDir: "/tmp"}, PID: "123", StartTime: "456"},
		ID:         "$1",
		Name:       "forge",
	}

	got, err := c.NewWindow(context.Background(), session, "review", "/repo", "codex", "exec", "prompt")
	if err != nil {
		t.Fatalf("NewWindow: %v", err)
	}
	if got.Generation != session.Generation || got.ID != "@7" || got.SessionID != "$1" || got.Name != "review" {
		t.Fatalf("NewWindow identity = %+v", got)
	}
	if len(fake.Calls) != 2 {
		t.Fatalf("calls = %d, want revalidation + creation: %+v", len(fake.Calls), fake.Calls)
	}
	argsEqual(t, fake.Calls[1].Args, []string{
		"new-window", "-P", "-F", IdentityFormat,
		"-t", "$1:", "-n", "review", "-c", "/repo",
		"--", "codex", "exec", "prompt",
	})
}

func TestNewWindow_RefusesGenerationDrift(t *testing.T) {
	sessionRow := strings.Join([]string{"123", "456", "$1", "forge", "1", "0", "0", "/repo"}, FieldSep)
	fake := &exec.FakeRunner{RunFunc: func(_ string, args []string) (string, error) {
		if args[0] == "list-sessions" {
			return sessionRow, nil
		}
		return "999" + FieldSep + "888" + FieldSep + "@7", nil
	}}
	c := New(fake)
	identityEnv(c, "", "/tmp")
	session := SessionIdentity{
		Generation: ServerGeneration{Selector: ServerSelector{TmpDir: "/tmp"}, PID: "123", StartTime: "456"},
		ID:         "$1",
	}
	if _, err := c.NewWindow(context.Background(), session, "review", ""); !errors.Is(err, ErrGenerationChanged) {
		t.Fatalf("NewWindow error = %v, want ErrGenerationChanged", err)
	}
}

func TestBuildTree(t *testing.T) {
	// Deliberately out-of-order input to prove sorting (work before main;
	// window 1 before 0; pane 1 before 0).
	sessions := []Session{
		{ID: "$2", Name: "work", Attached: false},
		{ID: "$1", Name: "main", Attached: true},
	}
	windows := []Window{
		{ID: "@1", SessionID: "$1", Session: "main", Index: 1, Name: "server", Active: false, Panes: 1},
		{ID: "@0", SessionID: "$1", Session: "main", Index: 0, Name: "editor", Active: true, Panes: 2},
		{ID: "@2", SessionID: "$2", Session: "work", Index: 0, Name: "shell", Active: true, Panes: 1},
	}
	panes := []Pane{
		{ID: "%1", WindowID: "@0", Index: 1, Command: "zsh", Active: false},
		{ID: "%0", WindowID: "@0", Index: 0, Command: "nvim", Active: true},
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

// TestBuildTreeGroupsByNativeID proves the display layer no longer re-derives
// parentage from names and indexes. Two sessions share a display name and two
// windows share an index; only the native ids tell them apart, and the old
// name+index composite would have filed every child under whichever key
// collided first.
func TestBuildTreeGroupsByNativeID(t *testing.T) {
	sessions := []Session{
		{ID: "$1", Name: "dup", Attached: true},
		{ID: "$2", Name: "dup", Attached: false},
	}
	windows := []Window{
		{ID: "@1", SessionID: "$1", Session: "dup", Index: 0, Name: "first", Panes: 1},
		{ID: "@2", SessionID: "$2", Session: "dup", Index: 0, Name: "second", Panes: 1},
	}
	panes := []Pane{
		{ID: "%1", WindowID: "@1", Index: 0, Command: "nvim"},
		{ID: "%2", WindowID: "@2", Index: 0, Command: "zsh"},
	}
	got := buildTree(sessions, windows, panes, iconTreeMarkers)
	want := strings.Join([]string{
		"● dup",
		"  0: first (1 pane)",
		"    0: nvim",
		"○ dup",
		"  0: second (1 pane)",
		"    0: zsh",
	}, "\n")
	if got != want {
		t.Errorf("buildTree mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
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

// lastSessionRunner answers the last-attached probe from lastAttached, and the
// revalidation listing that follows from the same set of sessions — so the
// attach argv a test asserts is the one that survived a real revalidation, not
// one the fake handed over unchecked.
func lastSessionRunner(lastAttached []string, sessions []string) *exec.FakeRunner {
	return &exec.FakeRunner{RunFunc: func(_ string, args []string) (string, error) {
		if len(args) >= 3 && args[0] == "list-sessions" {
			if args[2] == lastAttachedFormat {
				return strings.Join(lastAttached, "\n"), nil
			}
			return strings.Join(sessions, "\n"), nil
		}
		return "", nil
	}}
}

func lastAttachedRow(ts, pid, start, id, name string) string {
	return strings.Join([]string{ts, pid, start, id, name}, sep)
}

func TestLastSession_OutsideAllZeroTimestamps(t *testing.T) {
	// Every session never-attached (ts=0). The -1 sentinel means the first row
	// still wins (0 > -1), so we attach deterministically rather than to nothing.
	fake := lastSessionRunner(
		[]string{
			lastAttachedRow("0", "123", "456", "$1", "first"),
			lastAttachedRow("0", "123", "456", "$2", "second"),
		},
		[]string{
			strings.Join([]string{"123", "456", "$1", "first", "1", "0", "1700000000", "/w"}, sep),
			strings.Join([]string{"123", "456", "$2", "second", "1", "0", "1700000000", "/w"}, sep),
		},
	)
	c := New(fake, WithInsideTmux(func() bool { return false }))
	identityEnv(c, "", "/tmp")
	if err := c.LastSession(context.Background()); err != nil {
		t.Fatalf("LastSession: %v", err)
	}
	argsEqual(t, fake.Last().Args, []string{"attach-session", "-t", "$1"})
}

func TestLastSession_OutsideAttachesMostRecent(t *testing.T) {
	// Outside tmux: pick the greatest session_last_attached, then attach by its
	// native id — never by the name it carried in the listing.
	fake := lastSessionRunner(
		[]string{
			lastAttachedRow("100", "123", "456", "$1", "older"),
			lastAttachedRow("200", "123", "456", "$2", "newest"),
			lastAttachedRow("150", "123", "456", "$3", "middle"),
		},
		[]string{
			strings.Join([]string{"123", "456", "$2", "newest", "1", "0", "1700000000", "/w"}, sep),
		},
	)
	c := New(fake, WithInsideTmux(func() bool { return false }))
	identityEnv(c, "", "/tmp")
	if err := c.LastSession(context.Background()); err != nil {
		t.Fatalf("LastSession: %v", err)
	}
	call := fake.Last()
	if !call.Interactive {
		t.Errorf("outside tmux, last must attach (interactive)")
	}
	argsEqual(t, call.Args, []string{"attach-session", "-t", "$2"})
}

// TestLastSession_OutsideRefusesWhenWinnerVanished closes the window between
// picking the most-recently-attached session and attaching to it: if it is gone
// by then, LastSession must refuse rather than attach to whatever is left.
func TestLastSession_OutsideRefusesWhenWinnerVanished(t *testing.T) {
	fake := lastSessionRunner(
		[]string{lastAttachedRow("200", "123", "456", "$2", "newest")},
		[]string{strings.Join([]string{"123", "456", "$9", "survivor", "1", "0", "1700000000", "/w"}, sep)},
	)
	c := New(fake, WithInsideTmux(func() bool { return false }))
	identityEnv(c, "", "/tmp")
	if err := c.LastSession(context.Background()); !errors.Is(err, ErrObjectGone) {
		t.Fatalf("LastSession error = %v, want ErrObjectGone", err)
	}
	for _, call := range fake.Calls {
		if len(call.Args) > 0 && call.Args[0] != "list-sessions" {
			t.Fatalf("ran %v after the winner vanished, want only listings", call.Args)
		}
	}
}
