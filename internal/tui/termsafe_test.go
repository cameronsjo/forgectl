package tui

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/termsafe/termsafetest"
	"github.com/cameronsjo/forgectl/internal/tmux"
)

// hostileRunner serves tmux and sesh listings whose every operator-visible
// string carries terminal controls.
func hostileRunner() *exec.FakeRunner {
	const sep = "\x1f"
	h := termsafetest.Hostile
	session := strings.Join([]string{
		"123", "456", "$0", h("work"), "1", "1", "1700000000", h("/tmp/w"),
	}, sep)
	window := strings.Join([]string{
		"123", "456", "@0", "$0", h("work"), "0", h("edit"), "1", "2",
	}, sep)
	pane := strings.Join([]string{
		"123", "456", "%0", "@0", "0", h("t"), h("vim"), "1",
	}, sep)

	return &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		if strings.HasSuffix(name, "sesh") {
			return h("candidate"), nil
		}
		if len(args) == 0 {
			return "", nil
		}
		switch args[0] {
		case "list-sessions":
			return session, nil
		case "list-windows":
			return window, nil
		case "list-panes":
			return pane, nil
		}
		return "", nil
	}}
}

func hostileModel(t *testing.T) model {
	t.Helper()
	client := tmux.New(hostileRunner(),
		tmux.WithLookPath(func(string) (string, error) { return "/usr/bin/sesh", nil }))
	return sized(newModel(context.Background(), client, true), 80, 24)
}

// TestViewOfBenignFixtureIsAlreadyInert is the control every other assertion in
// this file rests on: with ordinary names, the drawn screen must already carry
// no escape sequence at all.
//
// Lip Gloss emits none under `go test` because stdout is not a terminal and its
// renderer degrades to plain text. That is a property of the harness, not of
// this change — so it is asserted rather than assumed. Without it, a screen
// full of styling escapes would fail the hostile-fixture tests below for
// reasons having nothing to do with a session name, and would keep failing
// after a correct fix.
func TestViewOfBenignFixtureIsAlreadyInert(t *testing.T) {
	fake := &exec.FakeRunner{RunFunc: func(_ string, _ []string) (string, error) {
		return oneSessionRow, nil
	}}
	m := sized(newModel(context.Background(), tmux.New(fake), true), 80, 24)
	out, _ := m.Update(key("2"))
	m = out.(model)
	termsafetest.AssertInert(t, "benign sessions screen", m.View())
	if !strings.Contains(m.View(), "alpha") {
		t.Fatalf("benign session name missing from the view; the control proves nothing:\n%s", m.View())
	}
}

// TestScreensDrawNothingUnsafe walks the four screens that render tmux- or
// sesh-derived text. A session, window, or pane name is chosen by whoever
// created the object — any same-uid process — and the TUI redraws the whole
// screen on every keystroke, so an escape in a name repaints its chrome.
func TestScreensDrawNothingUnsafe(t *testing.T) {
	for _, tc := range []struct {
		screen string
		key    string
	}{
		{"pick", "1"},
		{"sessions", "2"},
		{"windows", "3"},
		{"tree", "4"},
	} {
		t.Run(tc.screen, func(t *testing.T) {
			m := hostileModel(t)
			out, _ := m.Update(key(tc.key))
			m = out.(model)
			view := m.View()
			if view == "" {
				t.Fatalf("%s screen drew nothing; the check would pass vacuously", tc.screen)
			}
			termsafetest.AssertInert(t, tc.screen+" screen", view)
		})
	}
}

// TestErrorStatusDrawsNothingUnsafe pins errStatus, which was a live sanitizer
// with no test at all: nothing in this package drove a failing runner, so
// deleting its termsafe.SafeLine left the whole suite green.
//
// That gap is structural, not an oversight in probing. Every other sink here is
// reachable by rendering a screen, so a mutation probe finds it; this one is
// reachable only by making a tmux call FAIL, which no fixture did. A comment
// asserting "the error path already goes through errStatus" was true of the
// code and unverified by anything — the shape this repo has been burned by
// before.
func TestErrorStatusDrawsNothingUnsafe(t *testing.T) {
	m := hostileModel(t)
	// An error is the one input whose text forgectl never composes at all:
	// *exec.CommandError concatenates raw tmux stderr, which echoes the session
	// name tmux was given.
	m.setStatus(errors.New("tmux: "+termsafetest.Hostile("boom")), "")
	if m.status == "" {
		t.Fatal("status is empty; the check would pass vacuously")
	}
	termsafetest.AssertInert(t, "error status", m.status)
	if !strings.Contains(m.status, "boom") {
		t.Errorf("error status lost its legible text: %q", m.status)
	}
}

// TestSuccessStatusDrawsNothingUnsafe covers the footer's OTHER half. The error
// path goes through errStatus (pinned directly above); the success path
// composes its message from the same session name ("killed <name>") and had no
// such boundary.
func TestSuccessStatusDrawsNothingUnsafe(t *testing.T) {
	m := hostileModel(t)
	m.setStatus(nil, "killed "+termsafetest.Hostile("work"))
	if m.status == "" {
		t.Fatal("status is empty; the check would pass vacuously")
	}
	termsafetest.AssertInert(t, "success status", m.status)
	if !strings.Contains(m.status, "killed work") {
		t.Errorf("success status lost its legible text: %q", m.status)
	}
}

// TestConfirmPromptDrawsNothingUnsafe covers the kill confirmation, which
// renders the selected session's name inside a huh form.
func TestConfirmPromptDrawsNothingUnsafe(t *testing.T) {
	m := hostileModel(t)
	out, _ := m.Update(key("2"))
	m = out.(model)
	out, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	m = out.(model)
	if m.mode != formMode {
		t.Fatalf("expected the confirmation form, got mode %v", m.mode)
	}
	termsafetest.AssertInert(t, "confirmation form", m.View())
}

// TestRenamePromptDrawsNothingUnsafe pins startRename's %q boundary directly.
// The kill prompt above reaches its sibling through Update; rename used to have
// no test at all, so changing its title to %s left the suite green.
func TestRenamePromptDrawsNothingUnsafe(t *testing.T) {
	m := hostileModel(t)
	out, _ := m.startRename(tmux.SessionIdentity{Name: termsafetest.Hostile("work")})
	m = out.(model)
	if m.mode != formMode {
		t.Fatalf("expected the rename form, got mode %v", m.mode)
	}
	view := m.View()
	if view == "" {
		t.Fatal("rename form drew nothing; the check would pass vacuously")
	}
	termsafetest.AssertInert(t, "rename form", view)
	if !strings.Contains(view, "Rename") || !strings.Contains(view, "work") {
		t.Fatalf("rename prompt lost its legible title: %q", view)
	}
}
