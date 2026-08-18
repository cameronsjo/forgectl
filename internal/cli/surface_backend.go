package cli

import (
	"errors"
	"fmt"

	"github.com/cameronsjo/forgectl/internal/surface/backend"
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
	// yet drive.
	//
	// It is a distinct error rather than a variant of "unknown", because the
	// two need different responses: an unknown name is a typo the operator can
	// fix, and this one is a build that does not have the adapter yet. The
	// plumbing — protocol, trampoline, service, rollback — is complete and
	// tested; what is missing is the code that talks to a specific manager,
	// which is forgectl#332.
	errBackendNotImplemented = errors.New("forgectl: this build has no adapter for that surface backend yet")
)

// surfaceAdapterFor maps a backend name onto its adapter.
//
// Every arm currently refuses. That is deliberate rather than a stub left
// behind: the surface command, its target resolution, its launch state
// machine, and its rollback all exist and are exercised, and shipping them
// behind a truthful refusal is better than shipping a command that silently
// does nothing or one that lies about which manager it drove.
func surfaceAdapterFor(name string) (backend.Adapter, error) {
	kind, err := parseBackendKind(name)
	if err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("%w: %s (forgectl#332)", errBackendNotImplemented, kind)
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
