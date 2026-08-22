package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	osexec "os/exec"
	"path/filepath"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/surface/backend"
	"github.com/cameronsjo/forgectl/internal/surface/cmuxadapter"
	"github.com/cameronsjo/forgectl/internal/surface/herdradapter"
	"github.com/cameronsjo/forgectl/internal/surface/tmuxadapter"
)

// Backend selection.
//
// The name is matched against a closed set and nothing else — no prefix, no
// detection, no fallback to "whatever is running". A surface launch creates a
// workspace in a manager and starts a session in it; picking the manager on
// the operator's behalf is picking where their work appears.

var (
	// errUnknownBackend reports a name outside the closed set.
	errUnknownBackend = errors.New("forgectl: unknown surface backend")

	// errBackendNotImplemented reports a backend this build names but cannot
	// drive.
	//
	// It is a distinct error rather than a variant of "unknown", because the two
	// need different responses: an unknown name is a typo the operator can fix,
	// and this one is a build without the adapter.
	//
	// As of #332 it is a CONSTRUCTION guard, not a reachable state. All three
	// named backends are driven, and parseBackendKind returns an error rather
	// than KindUnspecified, so nothing can reach the arm that produces it —
	// which is why its mutation survives and no test asserts it fires. It is
	// kept because the case it guards is real and recurring: adding a fourth
	// Kind and teaching parseBackendKind about it, while forgetting the switch
	// in surfaceAdapterFor, lands exactly here. The alternative is a fallthrough
	// returning a nil adapter and a nil error.
	errBackendNotImplemented = errors.New("forgectl: this build has no adapter for that surface backend yet")

	// errBackendUnavailable reports a backend this build CAN drive but whose
	// program is not installed or not resolvable. It is separate from
	// "not implemented" for the same reason that one is separate from
	// "unknown": the operator's next move differs — install tmux, rather than
	// wait for a release or fix a typo.
	errBackendUnavailable = errors.New("forgectl: that surface backend is not available on this machine")
)

// surfaceAdapterFor maps a backend name onto its adapter.
//
// All three backends are driven. errBackendNotImplemented survives for the
// unspecified arm alone — it is the honest answer for a Kind that named no
// backend, and keeping it distinct from errUnknownBackend preserves the
// three-way distinction an operator needs: a typo, a missing feature, and an
// uninstalled program send them to three different next moves.
//
// Every arm resolves its server from the environment at construction, so an
// adapter is built per invocation rather than cached: the endpoint a launch
// targets is a property of the environment that launch ran in.
func surfaceAdapterFor(name string) (backend.Adapter, error) {
	return surfaceAdapterForWithWarnings(name, io.Discard)
}

// surfaceAdapterForWithWarnings is the production wiring for advisory backend
// warnings. The plain helper keeps selection tests quiet; a real surface
// command supplies its stderr so warnings remain visible when slog is off.
func surfaceAdapterForWithWarnings(name string, warnings io.Writer) (backend.Adapter, error) {
	kind, err := parseBackendKind(name)
	if err != nil {
		return nil, err
	}
	switch kind {
	case backend.KindTmux:
		return newTmuxAdapter()
	case backend.KindCmux:
		return newCmuxAdapter(warnings)
	case backend.KindHerdr:
		return newHerdrAdapter(warnings)
	case backend.KindUnspecified:
	}
	return nil, fmt.Errorf("%w: %s (forgectl#332)", errBackendNotImplemented, kind)
}

// newTmuxAdapter resolves tmux on PATH and builds the adapter.
//
// The lookup happens here rather than inside the adapter because the sensitive
// runner requires an ABSOLUTE path — it refuses to let exec.LookPath choose the
// binary against the live process PATH, which is the one decision its captured
// environment would otherwise not cover.
func newTmuxAdapter() (backend.Adapter, error) {
	path, err := osexec.LookPath("tmux")
	if err != nil {
		return nil, fmt.Errorf("%w: tmux not found on PATH: %w", errBackendUnavailable, err)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("%w: resolve tmux path: %w", errBackendUnavailable, err)
	}
	// Assigned and returned explicitly rather than forwarded. A bare
	// `return tmuxadapter.New(...)` converts New's nil *Adapter into a NON-nil
	// backend.Adapter holding a nil pointer — two lines below two `return nil`
	// statements that behave differently. The caller checks err first today;
	// this makes the shape not depend on that.
	a, err := tmuxadapter.New(exec.NewOSSensitiveRunner(), abs, os.Getenv, os.Getuid)
	if err != nil {
		// Classified as unavailable, not not-implemented: tmux is driven, and
		// an operator whose socket cannot be resolved needs to fix their
		// environment rather than wait for a release.
		return nil, fmt.Errorf("%w: %w", errBackendUnavailable, err)
	}
	return a, nil
}

// newHerdrAdapter resolves herdr on PATH and builds the adapter.
//
// The lookup happens here for the same reason the other two do: the sensitive
// runner requires an ABSOLUTE path, refusing to let exec.LookPath choose the
// binary against the live process PATH.
func newHerdrAdapter(warnings io.Writer) (backend.Adapter, error) {
	path, err := osexec.LookPath("herdr")
	if err != nil {
		return nil, fmt.Errorf("%w: herdr not found on PATH: %w", errBackendUnavailable, err)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("%w: resolve herdr path: %w", errBackendUnavailable, err)
	}
	// Assigned and returned explicitly rather than forwarded, for the reason
	// spelled out in newTmuxAdapter: a bare return converts a nil *Adapter into
	// a NON-nil backend.Adapter holding a nil pointer.
	if warnings == nil {
		warnings = io.Discard
	}
	a, err := herdradapter.New(exec.NewOSSensitiveRunner(), abs, os.Getenv,
		herdradapter.WithWarnings(warnings))
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errBackendUnavailable, err)
	}
	return a, nil
}

// newCmuxAdapter resolves cmux on PATH and builds the adapter.
//
// The lookup happens here rather than inside the adapter for the same reason
// tmux's does: the sensitive runner requires an ABSOLUTE path, refusing to let
// exec.LookPath choose the binary against the live process PATH, which is the
// one decision its captured environment would otherwise not cover.
func newCmuxAdapter(warnings io.Writer) (backend.Adapter, error) {
	path, err := osexec.LookPath("cmux")
	if err != nil {
		return nil, fmt.Errorf("%w: cmux not found on PATH: %w", errBackendUnavailable, err)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("%w: resolve cmux path: %w", errBackendUnavailable, err)
	}
	// Assigned and returned explicitly rather than forwarded, for the reason
	// spelled out in newTmuxAdapter: a bare return would convert a nil *Adapter
	// into a NON-nil backend.Adapter holding a nil pointer.
	if warnings == nil {
		warnings = io.Discard
	}
	a, err := cmuxadapter.New(exec.NewOSSensitiveRunner(), abs, os.Getenv,
		cmuxadapter.WithWarnings(warnings))
	if err != nil {
		// Classified as unavailable, not not-implemented: cmux is driven, and an
		// operator whose endpoint cannot be resolved needs to fix their
		// environment rather than wait for a release.
		return nil, fmt.Errorf("%w: %w", errBackendUnavailable, err)
	}
	return a, nil
}

// parseBackendKind resolves the closed set of names.
func parseBackendKind(name string) (backend.Kind, error) {
	switch name {
	case "tmux":
		return backend.KindTmux, nil
	case "cmux":
		return backend.KindCmux, nil
	case "herdr":
		return backend.KindHerdr, nil
	default:
		// The supplied name is not echoed. It is not secret, but it is
		// command-line input to a message that gets printed to a terminal, and
		// every other refusal in this package is category-only for that reason.
		return backend.Kind(0), fmt.Errorf("%w: expected one of tmux, cmux, herdr", errUnknownBackend)
	}
}
