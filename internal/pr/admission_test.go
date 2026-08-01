package pr

// Test plan for admission.go
//
// LiveReviews (Classification: fail-safe tmux window count)
//   [x] Happy: 2 "pr-*" windows + 1 unrelated window, all in the client's
//       session → counts only the 2 prefixed windows
//   [x] Unhappy: list-windows errors (not "no server running") → (0, false) —
//       the caller MUST treat this as unreadable, never as a genuine zero
//   [x] Boundary: "pr-*" windows that exist in a DIFFERENT session are not
//       counted
//
// Admit (Classification: concurrency gate, fail-closed)
//   [x] Happy: has-session errors (no session yet) → genuine zero, full cap
//       free
//   [x] Boundary: live count already at/above max → 0 free, still ok
//   [x] Boundary: a non-positive max resolves to DefaultMaxConcurrentReviews

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
)

// winRow builds one list-windows -a fixture line: session, index "1", name,
// active "0", panes "1" — joined on tmux's field separator.
func winRow(session, name string) string {
	return strings.Join([]string{session, "1", name, "0", "1"}, "\x1f")
}

func TestAdmit_HasSessionErrors(t *testing.T) {
	fake := &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		if name == "tmux" && len(args) > 0 && args[0] == "has-session" {
			return "", errors.New("exit status 1")
		}
		return "", nil
	}}
	c := New(fake, WithTmuxSession("forgectl"))
	free, ok := c.Admit(context.Background(), 4)
	if !ok || free != 4 {
		t.Fatalf("Admit(ctx, 4) = (%d, %v), want (4, true)", free, ok)
	}
}

func TestLiveReviews_CountsPrefixedWindowsInSession(t *testing.T) {
	rows := strings.Join([]string{
		winRow("forgectl", "pr-a-b-1"),
		winRow("forgectl", "pr-c-d-2"),
		winRow("forgectl", "shell"),
	}, "\n")
	fake := &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		switch {
		case name == "tmux" && len(args) > 0 && args[0] == "has-session":
			return "", nil
		case name == "tmux" && len(args) > 0 && args[0] == "list-windows":
			return rows, nil
		}
		return "", nil
	}}
	c := New(fake, WithTmuxSession("forgectl"))
	n, ok := c.LiveReviews(context.Background())
	if !ok || n != 2 {
		t.Fatalf("LiveReviews() = (%d, %v), want (2, true)", n, ok)
	}
}

func TestLiveReviews_ListWindowsErrors(t *testing.T) {
	fake := &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		switch {
		case name == "tmux" && len(args) > 0 && args[0] == "has-session":
			return "", nil
		case name == "tmux" && len(args) > 0 && args[0] == "list-windows":
			// TRAP: this string must NOT contain "no server running" — isNoServer
			// (internal/tmux/sessions.go) would convert that to (nil, nil) and this
			// test would assert the wrong thing.
			return "", errors.New("boom: tmux exploded")
		}
		return "", nil
	}}
	c := New(fake, WithTmuxSession("forgectl"))
	n, ok := c.LiveReviews(context.Background())
	if ok || n != 0 {
		t.Fatalf("LiveReviews() = (%d, %v), want (0, false)", n, ok)
	}
}

func TestAdmit_LiveAtOrAboveMax(t *testing.T) {
	rows := strings.Join([]string{
		winRow("forgectl", "pr-a-b-1"),
		winRow("forgectl", "pr-c-d-2"),
	}, "\n")
	fake := &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		switch {
		case name == "tmux" && len(args) > 0 && args[0] == "has-session":
			return "", nil
		case name == "tmux" && len(args) > 0 && args[0] == "list-windows":
			return rows, nil
		}
		return "", nil
	}}
	c := New(fake, WithTmuxSession("forgectl"))
	free, ok := c.Admit(context.Background(), 2)
	if !ok || free != 0 {
		t.Fatalf("Admit(ctx, 2) = (%d, %v), want (0, true)", free, ok)
	}
}

func TestAdmit_MaxLessOrEqualZeroDefaults(t *testing.T) {
	// Zero-value FakeRunner: has-session succeeds (session exists), list-windows
	// returns no rows — live count 0, so free should equal the resolved default.
	fake := &exec.FakeRunner{}
	c := New(fake, WithTmuxSession("forgectl"))
	free, ok := c.Admit(context.Background(), 0)
	if !ok || free != DefaultMaxConcurrentReviews {
		t.Fatalf("Admit(ctx, 0) = (%d, %v), want (%d, true)", free, ok, DefaultMaxConcurrentReviews)
	}
}

func TestLiveReviews_IgnoresOtherSessions(t *testing.T) {
	rows := strings.Join([]string{
		winRow("other-session", "pr-a-b-1"),
		winRow("other-session", "pr-c-d-2"),
	}, "\n")
	fake := &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		switch {
		case name == "tmux" && len(args) > 0 && args[0] == "has-session":
			return "", nil
		case name == "tmux" && len(args) > 0 && args[0] == "list-windows":
			return rows, nil
		}
		return "", nil
	}}
	c := New(fake, WithTmuxSession("forgectl"))
	n, ok := c.LiveReviews(context.Background())
	if !ok || n != 0 {
		t.Fatalf("LiveReviews() = (%d, %v), want (0, true)", n, ok)
	}
}
