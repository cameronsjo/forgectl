package cli

import (
	"errors"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/surface/backend"
)

// TestParseBackendKind_IsAClosedSet keeps the manager from being guessed at.
// A surface launch creates a workspace and starts a session in it; choosing
// the manager on the operator's behalf is choosing where their work appears.
func TestParseBackendKind_IsAClosedSet(t *testing.T) {
	accepted := map[string]backend.Kind{
		"tmux":  backend.KindTmux,
		"cmux":  backend.KindCmux,
		"herdr": backend.KindHerdr,
	}
	for name, want := range accepted {
		t.Run(name, func(t *testing.T) {
			got, err := parseBackendKind(name)
			if err != nil {
				t.Fatalf("parseBackendKind(%q) = %v, want %v", name, err, want)
			}
			if got != want {
				t.Errorf("parseBackendKind(%q) = %v, want %v", name, got, want)
			}
		})
	}

	// No prefix, no case folding, no whitespace tolerance, no detection.
	for _, name := range []string{"", "tm", "TMUX", "tmux ", "screen", "auto", "tmux,cmux"} {
		t.Run("refuses "+name, func(t *testing.T) {
			if _, err := parseBackendKind(name); !errors.Is(err, errUnknownBackend) {
				t.Errorf("parseBackendKind(%q) = %v, want errUnknownBackend", name, err)
			}
		})
	}
}

// TestParseBackendKind_RefusalNeverEchoesTheName keeps command-line input out
// of a terminal-rendered message.
func TestParseBackendKind_RefusalNeverEchoesTheName(t *testing.T) {
	hostile := "\x1b[2Kscreen"
	_, err := parseBackendKind(hostile)
	if err == nil {
		t.Fatal("a bogus backend name was accepted")
	}
	if strings.Contains(err.Error(), hostile) || strings.Contains(err.Error(), "\x1b") {
		t.Errorf("the refusal echoed the supplied name: %q", err.Error())
	}
}

// TestSurfaceAdapterFor_RefusesEveryBackendForNow pins the honest state of this
// build: the plumbing is complete, the adapters are forgectl#332.
//
// It is a real assertion rather than a placeholder — the distinction between
// "unknown name" and "no adapter yet" is what tells an operator whether they
// made a typo or hit a missing feature, and collapsing the two would lose it.
func TestSurfaceAdapterFor_RefusesEveryBackendForNow(t *testing.T) {
	for _, name := range []string{"tmux", "cmux", "herdr"} {
		t.Run(name, func(t *testing.T) {
			adapter, err := surfaceAdapterFor(name)
			if adapter != nil {
				t.Fatal("an adapter was returned; update this test alongside forgectl#332")
			}
			if !errors.Is(err, errBackendNotImplemented) {
				t.Errorf("surfaceAdapterFor(%q) = %v, want errBackendNotImplemented", name, err)
			}
			if errors.Is(err, errUnknownBackend) {
				t.Error("a recognised backend was reported as unknown; an operator would " +
					"go looking for a typo that is not there")
			}
		})
	}

	// And an unrecognised name still reports itself as one.
	if _, err := surfaceAdapterFor("screen"); !errors.Is(err, errUnknownBackend) {
		t.Errorf("an unknown backend = %v, want errUnknownBackend", err)
	}
}
