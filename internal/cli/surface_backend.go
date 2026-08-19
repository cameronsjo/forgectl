package cli

import (
	"errors"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/surface/backend"
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
	// yet drive.
	//
	// It is a distinct error rather than a variant of "unknown", because the
	// two need different responses: an unknown name is a typo the operator can
	// fix, and this one is a build that does not have the adapter yet. The
	// plumbing — protocol, trampoline, service, rollback — is complete and
	// tested; what is missing is the code that talks to a specific manager,
	// which is forgectl#332.
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
// tmux is driven; cmux and herdr still refuse. The refusal is deliberate rather
// than a stub left behind: the surface command, its target resolution, its
// launch state machine, and its rollback all exist and are exercised, and
// shipping them behind a truthful refusal is better than shipping a command
// that silently does nothing or one that lies about which manager it drove.
//
// The tmux arm resolves its server from the environment at construction, so an
// adapter is built per invocation rather than cached: the socket a launch
// targets is a property of the environment that launch ran in.
func surfaceAdapterFor(name string) (backend.Adapter, error) {
	kind, err := parseBackendKind(name)
	if err != nil {
		return nil, err
	}
	switch kind {
	case backend.KindTmux:
		return newTmuxAdapter()
	case backend.KindCmux, backend.KindHerdr, backend.KindUnspecified:
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
	return tmuxadapter.New(exec.NewOSSensitiveRunner(), abs, os.Getenv, os.Getuid)
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
