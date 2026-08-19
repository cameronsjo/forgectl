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

// TestSurfaceAdapterFor_DrivesTmuxAndRefusesTheRest pins the honest state of
// this build: tmux is driven, cmux and herdr are still forgectl#332.
//
// The three-way distinction is the assertion. "Unknown name", "no adapter yet",
// and "the program is not installed" send an operator to three different next
// moves — fix a typo, wait for a release, install tmux — and collapsing any two
// of them loses that.
func TestSurfaceAdapterFor_DrivesTmuxAndRefusesTheRest(t *testing.T) {
	for _, name := range []string{"cmux", "herdr"} {
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

	// tmux resolves to a real adapter wherever tmux is installed, and to a
	// distinct "not available" refusal where it is not — never to "not
	// implemented", which would now be a lie, and never to "unknown".
	t.Run("tmux", func(t *testing.T) {
		adapter, err := surfaceAdapterFor("tmux")
		if errors.Is(err, errBackendNotImplemented) {
			t.Fatal("tmux reported as unimplemented; this build drives it")
		}
		if errors.Is(err, errUnknownBackend) {
			t.Fatal("tmux reported as an unknown backend")
		}
		if err != nil {
			// The only sanctioned failure here is a machine without tmux.
			if !errors.Is(err, errBackendUnavailable) {
				t.Fatalf("surfaceAdapterFor(tmux) = %v, want an adapter or errBackendUnavailable", err)
			}
			return
		}
		if adapter == nil {
			t.Fatal("surfaceAdapterFor(tmux) returned no adapter and no error")
		}
		if adapter.Kind() != backend.KindTmux {
			t.Errorf("adapter.Kind() = %v, want tmux", adapter.Kind())
		}
		// The launch refuses any adapter that cannot clean up after itself, so
		// an adapter that reaches the service without Close and Probe would
		// fail at rollback — while holding something that needs closing.
		if _, err := backend.RequireCapabilities(adapter); err != nil {
			t.Errorf("the tmux adapter does not satisfy Capabilities: %v", err)
		}
	})

	// And an unrecognised name still reports itself as one.
	if _, err := surfaceAdapterFor("screen"); !errors.Is(err, errUnknownBackend) {
		t.Errorf("an unknown backend = %v, want errUnknownBackend", err)
	}
}
