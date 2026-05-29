package tmux

import (
	"context"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
)

// sep is the field separator, spelled out here so test fixtures read clearly.
const sep = "\x1f"

func TestParseSessions(t *testing.T) {
	// One line per session, fields joined by the unit separator. Covers the
	// gnarly cases the bash precedent stumbled on: names AND paths with spaces,
	// a zero-window session, and the attached/detached split.
	out := "main" + sep + "3" + sep + "1" + sep + "1700000000" + sep + "/Users/cam/Projects/forgectl" + "\n" +
		"my session" + sep + "1" + sep + "0" + sep + "1700000100" + sep + "/Users/cam/Notes With Spaces" + "\n" +
		"empty" + sep + "0" + sep + "0" + sep + "1700000200" + sep + "/tmp"

	got := parseSessions(out)
	if len(got) != 3 {
		t.Fatalf("expected 3 sessions, got %d: %+v", len(got), got)
	}

	if got[0].Name != "main" || got[0].Windows != 3 || !got[0].Attached {
		t.Errorf("session 0 wrong: %+v", got[0])
	}
	if got[0].Path != "/Users/cam/Projects/forgectl" {
		t.Errorf("session 0 path wrong: %q", got[0].Path)
	}

	// Name and path both contain spaces — must survive intact.
	if got[1].Name != "my session" {
		t.Errorf("expected name with space %q, got %q", "my session", got[1].Name)
	}
	if got[1].Path != "/Users/cam/Notes With Spaces" {
		t.Errorf("expected path with spaces, got %q", got[1].Path)
	}
	if got[1].Attached {
		t.Errorf("session 1 should be detached")
	}

	// Zero windows must parse to 0, not drop the row.
	if got[2].Windows != 0 {
		t.Errorf("expected 0 windows, got %d", got[2].Windows)
	}
	if got[2].Created.IsZero() {
		t.Errorf("expected created time parsed for session 2")
	}
}

func TestParseSessions_Empty(t *testing.T) {
	if got := parseSessions(""); len(got) != 0 {
		t.Errorf("empty output should yield no sessions, got %+v", got)
	}
}

func TestParseSessions_SkipsShortRows(t *testing.T) {
	// A malformed row (too few fields) must be skipped, not panic.
	out := "good" + sep + "2" + sep + "1" + sep + "1700000000" + sep + "/tmp" + "\n" +
		"truncated" + sep + "2"
	got := parseSessions(out)
	if len(got) != 1 || got[0].Name != "good" {
		t.Errorf("expected only the well-formed row, got %+v", got)
	}
}

func TestListSessions_Wiring(t *testing.T) {
	// Verify the exact argv tmux receives: list-sessions -F <sessionFormat>.
	fake := &exec.FakeRunner{
		RunFunc: func(name string, args []string) (string, error) {
			return "main" + sep + "1" + sep + "1" + sep + "1700000000" + sep + "/tmp", nil
		},
	}
	c := New(fake, WithBins("tmux", "sesh"))

	if _, err := c.ListSessions(context.Background()); err != nil {
		t.Fatalf("ListSessions: %v", err)
	}

	call := fake.Last()
	if call.Name != "tmux" {
		t.Errorf("expected tmux binary, got %q", call.Name)
	}
	want := []string{"list-sessions", "-F", sessionFormat}
	if len(call.Args) != len(want) {
		t.Fatalf("args mismatch: got %v want %v", call.Args, want)
	}
	for i := range want {
		if call.Args[i] != want[i] {
			t.Errorf("arg %d: got %q want %q", i, call.Args[i], want[i])
		}
	}
}

func TestListSessions_NoServer(t *testing.T) {
	// tmux exits non-zero with "no server running" when nothing is up; that's
	// "no sessions", not an error.
	fake := &exec.FakeRunner{
		RunFunc: func(name string, args []string) (string, error) {
			return "", &exec.CommandError{
				Name:   "tmux",
				Args:   args,
				Stderr: "no server running on /tmp/tmux-501/default",
			}
		},
	}
	c := New(fake)

	sessions, err := c.ListSessions(context.Background())
	if err != nil {
		t.Fatalf("expected nil error for no-server, got %v", err)
	}
	if len(sessions) != 0 {
		t.Errorf("expected no sessions, got %+v", sessions)
	}
}
