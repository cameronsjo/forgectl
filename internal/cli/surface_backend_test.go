package cli

import (
	"errors"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/module"
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
		"hc":    backend.KindHc,
		"rex":   backend.KindRex,
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

// TestSurfaceAdapterFor_DrivesNativeBackendsAndRefusesLegacy pins the honest
// state of this build (#1518): only hc and rex are native surfaces, legacy
// herdr/cmux/tmux return an actionable error pointing to plugins, and an
// unrecognised name reports errUnknownBackend.
func TestSurfaceAdapterFor_DrivesNativeBackendsAndRefusesLegacy(t *testing.T) {
	driven := map[string]backend.Kind{
		"hc":  backend.KindHc,
		"rex": backend.KindRex,
	}
	for name, want := range driven {
		t.Run(name, func(t *testing.T) {
			adapter, err := surfaceAdapterFor(name)
			if errors.Is(err, errBackendNotImplemented) {
				t.Fatalf("%s reported as unimplemented; this build drives it", name)
			}
			if errors.Is(err, errUnknownBackend) {
				t.Fatalf("%s reported as an unknown backend", name)
			}
			if err != nil {
				if !errors.Is(err, errBackendUnavailable) {
					t.Fatalf("surfaceAdapterFor(%s) = %v, want an adapter or errBackendUnavailable", name, err)
				}
				return
			}
			if adapter == nil {
				t.Fatalf("surfaceAdapterFor(%s) returned no adapter and no error", name)
			}
			if adapter.Kind() != want {
				t.Errorf("adapter.Kind() = %v, want %v", adapter.Kind(), want)
			}
			if _, err := backend.RequireCapabilities(adapter); err != nil {
				t.Errorf("the %s adapter does not satisfy Capabilities: %v", name, err)
			}
		})
	}

	for _, legacy := range []string{"tmux", "cmux", "herdr"} {
		t.Run("legacy "+legacy, func(t *testing.T) {
			adapter, err := surfaceAdapterFor(legacy)
			if adapter != nil {
				t.Errorf("surfaceAdapterFor(%s) returned a non-nil adapter", legacy)
			}
			if !errors.Is(err, errBackendNotImplemented) || !errors.Is(err, errLegacyBackend) {
				t.Fatalf("surfaceAdapterFor(%s) = %v, want errBackendNotImplemented + errLegacyBackend", legacy, err)
			}
			if !strings.Contains(err.Error(), "plugins") {
				t.Errorf("legacy refusal should mention plugins: %q", err.Error())
			}
		})
	}

	// And an unrecognised name still reports itself as one.
	if _, err := surfaceAdapterFor("screen"); !errors.Is(err, errUnknownBackend) {
		t.Errorf("an unknown backend = %v, want errUnknownBackend", err)
	}
}

// TestTheSurfaceFlagHelpNamesExactlyTheDrivenBackends ties the operator-facing
// roster to the code that decides it.
func TestTheSurfaceFlagHelpNamesExactlyTheDrivenBackends(t *testing.T) {
	flag := newSurfaceLaunchCmd(module.Deps{}).Flags().Lookup("surface")
	if flag == nil {
		t.Fatal("the launch command has no --surface flag")
	}
	help := flag.Usage

	for _, name := range []string{"tmux", "cmux", "herdr", "hc", "rex"} {
		_, err := surfaceAdapterFor(name)
		driven := !errors.Is(err, errBackendNotImplemented)
		named := strings.Contains(help, name)

		switch {
		case driven && !named:
			t.Errorf("%s is driven but --surface's help does not name it: %q", name, help)
		case !driven && named:
			t.Errorf("%s is not implemented but --surface's help advertises it: %q", name, help)
		}
	}
}
