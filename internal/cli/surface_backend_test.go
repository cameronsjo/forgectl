package cli

import (
	"errors"
	osexec "os/exec"
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

// TestSurfaceAdapterFor_DrivesEveryNamedBackend pins the honest state of this
// build: all three backends are driven, and an unrecognised name is still
// reported as one.
//
// The three-way distinction is the assertion. "Unknown name", "no adapter yet",
// and "the program is not installed" send an operator to three different next
// moves — fix a typo, wait for a release, install tmux — and collapsing any two
// of them loses that.
// Side effect worth declaring: the "tmux" case below calls the real
// surfaceAdapterFor, so on a machine with tmux installed it runs the real
// os.MkdirAll and creates /tmp/tmux-<uid> (0700) if it is not already there.
// Harmless — it is the directory tmux itself would use — but it is a write, and
// a test that writes outside its TempDir should say so.
func TestSurfaceAdapterFor_DrivesEveryNamedBackend(t *testing.T) {
	// Every DRIVEN backend gets the same criterion rather than one apiece: a
	// per-backend assertion only ever catches per-backend faults, and the arms
	// it does not compare are exactly where the next adapter's wiring drifts.
	// Each resolves to a real adapter wherever its program is installed, and to
	// a distinct "not available" refusal where it is not — never to "not
	// implemented", which would now be a lie, and never to "unknown".
	driven := map[string]backend.Kind{
		"tmux":  backend.KindTmux,
		"cmux":  backend.KindCmux,
		"herdr": backend.KindHerdr,
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
				// The only sanctioned failure here is a machine without the
				// program, or one whose endpoint cannot be resolved.
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
			// The launch refuses any adapter that cannot clean up after itself,
			// so an adapter that reaches the service without Close and Probe
			// would fail at rollback — while holding something that needs
			// closing.
			if _, err := backend.RequireCapabilities(adapter); err != nil {
				t.Errorf("the %s adapter does not satisfy Capabilities: %v", name, err)
			}
		})
	}

	// And an unrecognised name still reports itself as one.
	if _, err := surfaceAdapterFor("screen"); !errors.Is(err, errUnknownBackend) {
		t.Errorf("an unknown backend = %v, want errUnknownBackend", err)
	}
}

// TestNewTmuxAdapter_ReturnsATrulyNilAdapterOnEveryFailure pins the reason
// newTmuxAdapter assigns and returns explicitly instead of forwarding
// tmuxadapter.New's result: a bare `return tmuxadapter.New(...)` converts
// New's nil *Adapter into a NON-nil backend.Adapter holding a nil pointer, the
// moment New's error path is taken. `adapter != nil` would then be true and
// `err != nil` would also be true — a caller checking err first (as this
// package's tests do) would never notice, but a caller checking adapter first,
// or storing it, would hold a typed nil. This asserts the plain `== nil`
// comparison directly, on both of newTmuxAdapter's two failure exits.
func TestNewTmuxAdapter_ReturnsATrulyNilAdapterOnEveryFailure(t *testing.T) {
	t.Run("tmux not found on PATH", func(t *testing.T) {
		empty := t.TempDir()
		t.Setenv("PATH", empty)

		adapter, err := newTmuxAdapter()
		if !errors.Is(err, errBackendUnavailable) {
			t.Fatalf("newTmuxAdapter() err = %v, want errBackendUnavailable", err)
		}
		if adapter != nil {
			t.Error("newTmuxAdapter() returned a non-nil adapter alongside an error")
		}
	})

	t.Run("tmuxadapter.New refuses the resolved socket", func(t *testing.T) {
		// Without tmux on PATH the LookPath above fails FIRST and this subtest
		// silently becomes a duplicate of its sibling — asserting the same
		// thing and never reaching New at all. Skipping says so out loud
		// rather than reporting a pass for coverage that did not happen.
		if _, err := osexec.LookPath("tmux"); err != nil {
			t.Skip("tmux is not installed, so this cannot reach tmuxadapter.New")
		}
		// The failure is pushed into New itself by naming a socket path long
		// enough that tmuxadapter.checkSocketPath refuses it — the one
		// New-side refusal this package can force without touching the
		// filesystem. (checkSocketPath mirrors tmux.NewPinned's rules; it is
		// the one that actually runs here.)
		t.Setenv("TMUX", "/tmp/"+strings.Repeat("a", 5000)+",1,0")

		adapter, err := newTmuxAdapter()
		if !errors.Is(err, errBackendUnavailable) {
			t.Fatalf("newTmuxAdapter() err = %v, want errBackendUnavailable", err)
		}
		if adapter != nil {
			t.Error("newTmuxAdapter() returned a non-nil adapter alongside an error")
		}
	})
}

// TestTheSurfaceFlagHelpNamesExactlyTheDrivenBackends ties the operator-facing
// roster to the code that decides it.
//
// The flag's parenthesised list IS the roster as far as anyone reading `--help`
// is concerned, and nothing linked it to surfaceAdapterFor's switch: cmux
// shipped driven while the help still said "(tmux)", so the command refused to
// admit it could do the thing it had just learned to do. A stale roster is a
// small defect with a large surface — it is the first place an operator looks
// and the last place a diff touches.
//
// The check derives both sides rather than restating either. A backend is
// DRIVEN when surfaceAdapterFor does not refuse it as unimplemented — which
// covers the "installed" and "not installed" cases alike, since only the
// not-implemented refusal is a statement about this build.
func TestTheSurfaceFlagHelpNamesExactlyTheDrivenBackends(t *testing.T) {
	flag := newSurfaceLaunchCmd(module.Deps{}).Flags().Lookup("surface")
	if flag == nil {
		t.Fatal("the launch command has no --surface flag")
	}
	help := flag.Usage

	for _, name := range []string{"tmux", "cmux", "herdr"} {
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

// TestNewCmuxAdapter_ReturnsATrulyNilAdapterOnEveryFailure is the cmux
// counterpart, and its absence was the exact fault this file's other test
// argues against three functions above: "a per-backend assertion only ever
// catches per-backend faults, and the arms it does not compare are exactly
// where the next adapter's wiring drifts."
//
// The principle was applied to surfaceAdapterFor's driven-backend table and not
// to the typed-nil guard one function below it, so newCmuxAdapter's guard was
// claimed by a comment and pinned by nothing — rewriting its body to forward
// cmuxadapter.New's result directly left the suite green.
func TestNewCmuxAdapter_ReturnsATrulyNilAdapterOnEveryFailure(t *testing.T) {
	t.Run("cmux not found on PATH", func(t *testing.T) {
		empty := t.TempDir()
		t.Setenv("PATH", empty)

		adapter, err := newCmuxAdapter()
		if !errors.Is(err, errBackendUnavailable) {
			t.Fatalf("newCmuxAdapter() err = %v, want errBackendUnavailable", err)
		}
		if adapter != nil {
			t.Error("newCmuxAdapter() returned a non-nil adapter alongside an error")
		}
	})

	t.Run("cmuxadapter.New refuses the resolved socket", func(t *testing.T) {
		// Same guard as the tmux sibling, and for the same reason: without cmux
		// on PATH the LookPath fails FIRST and this subtest silently becomes a
		// duplicate that never reaches New. Skipping says so out loud rather
		// than reporting a pass for coverage that did not happen.
		if _, err := osexec.LookPath("cmux"); err != nil {
			t.Skip("cmux is not installed, so this cannot reach cmuxadapter.New")
		}
		// The direct analogue of the tmux subtest's oversized TMUX: a socket
		// path past sun_path, which cmuxadapter.checkSocketPath refuses. It is
		// the one New-side failure this package can force without touching the
		// filesystem.
		t.Setenv("CMUX_SOCKET_PATH", "/tmp/"+strings.Repeat("a", 5000)+"/cmux.sock")

		adapter, err := newCmuxAdapter()
		if !errors.Is(err, errBackendUnavailable) {
			t.Fatalf("newCmuxAdapter() err = %v, want errBackendUnavailable", err)
		}
		if adapter != nil {
			t.Error("newCmuxAdapter() returned a non-nil adapter alongside an error")
		}
	})
}
