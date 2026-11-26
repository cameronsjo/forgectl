package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/rexbin"
	"github.com/cameronsjo/forgectl/internal/surface/backend"
	"github.com/cameronsjo/forgectl/internal/surface/rexadapter"
	"github.com/cameronsjo/forgectl/internal/termsafe"
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

	// errLegacyBackend reports a legacy surface backend whose adapter moved out
	// of core into a plugin (#1518).
	errLegacyBackend = errors.New("only hc and rex are native surfaces; legacy herdr, cmux, and tmux adapters moved to plugins")
)

// surfaceAdapterFor maps a backend name onto its adapter.
//
// Only hc and rex are native surfaces (#1518). Legacy names (tmux, cmux,
// herdr) return an actionable error wrapping errBackendNotImplemented and
// errLegacyBackend.
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
func surfaceAdapterForWithWarnings(name string, _ io.Writer) (backend.Adapter, error) {
	kind, err := parseBackendKind(name)
	if err != nil {
		return nil, err
	}
	switch kind {
	case backend.KindTmux, backend.KindCmux, backend.KindHerdr:
		return nil, fmt.Errorf("%w: %s: %w", errBackendNotImplemented, kind, errLegacyBackend)
	case backend.KindHc:
		return newHcAdapter()
	case backend.KindRex:
		return newRexAdapter(locateRex)
	case backend.KindUnspecified:
	}
	return nil, fmt.Errorf("%w: %s (forgectl#332)", errBackendNotImplemented, kind)
}

// rexLocation is where `rex` was found and how to re-check it before use.
type rexLocation struct {
	// Path is absolute.
	Path string
	// Verify re-runs the trust gates on Path; nil means there is nothing to
	// re-check (a test locator).
	Verify func(context.Context) error
}

// rexLocator finds the `rex` binary.
type rexLocator func() (rexLocation, error)

// rexResolveTimeout bounds locating rex: a `whoami` and a signature check per
// candidate, each with its own shorter limit inside rexbin.
const rexResolveTimeout = 60 * time.Second

// locateRex is how a surface launch finds rex: the `[surface.rex] binary_path`
// key, then PATH, then the installed Rex app bundles (internal/rexbin). A
// config file that did not decode is refused rather than read as "no pin",
// because the pin chooses an executable.
var locateRex rexLocator = resolveRex

func resolveRex() (rexLocation, error) { return resolveRexWith(config.Load) }

// rexPinRefusal refuses a [surface.rex] binary_path that came from a config
// file another user could have written: the pin picks an executable, and a
// pin outside any .app is gated by nothing else. The verdict was reached on
// the descriptor config.Load read, so there is no gap to race. The parent
// directory's owner and mode, and ACLs, are not judged.
func rexPinRefusal(cfg config.Config) error {
	if cfg.Surface.Rex.BinaryPath == "" {
		return nil
	}
	if err := cfg.ExecChoiceFileError(); err != nil {
		return fmt.Errorf("[surface.rex] binary_path is not trusted: %w", err)
	}
	return nil
}

// resolveRexWith is resolveRex with the config loader injected, so a test can
// feed it a config that did not decode.
func resolveRexWith(load func() config.Config) (rexLocation, error) {
	cfg := load()
	if err := cfg.DecodeError(); err != nil {
		return rexLocation{}, fmt.Errorf("config file: %w", termsafe.Error(err))
	}
	if cfg.DecodeDegraded() {
		return rexLocation{}, errors.New("the config file could not be located or read; not guessing where rex is")
	}
	if err := rexPinRefusal(cfg); err != nil {
		return rexLocation{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), rexResolveTimeout)
	defer cancel()
	r := rexbin.New(exec.OSRunner{})
	res, err := r.Resolve(ctx, cfg.Surface.Rex.BinaryPath)
	if err != nil {
		return rexLocation{}, err
	}
	return rexLocation{Path: res.Path, Verify: func(ctx context.Context) error { return r.Verify(ctx, res) }}, nil
}

// verifyingRunner re-checks the rex binary before every command the Rex
// adapter runs. The locator judged the file once, and the app bundle's parent
// directory can be rewritten by any admin account at any time, so each use
// repeats the gates; a failed check runs nothing.
type verifyingRunner struct {
	inner  exec.SensitiveRunner
	verify func(context.Context) error
}

func (v verifyingRunner) RunSensitive(ctx context.Context, cmd exec.SensitiveCommand) (exec.SensitiveResult, error) {
	if err := v.verify(ctx); err != nil {
		// A caller that cancelled, or ran out of time, during the check is a
		// cancel or a timeout to the adapter's classifier, not "unavailable".
		switch {
		case errors.Is(err, context.Canceled):
			err = fmt.Errorf("%w: %w", exec.ErrCanceled, err)
		case errors.Is(err, context.DeadlineExceeded):
			err = fmt.Errorf("%w: %w", exec.ErrTimeout, err)
		}
		return exec.SensitiveResult{}, fmt.Errorf("rex binary no longer passes its checks: %w", err)
	}
	return v.inner.RunSensitive(ctx, cmd)
}

// newRexAdapter builds the Rex adapter on the location locate finds. The path
// is resolved here, before any command runs, for the reason every backend's
// is: the sensitive runner requires an absolute path.
func newRexAdapter(locate rexLocator) (backend.Adapter, error) {
	loc, err := locate()
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errBackendUnavailable, err)
	}
	return buildRexAdapter(loc, exec.NewOSSensitiveRunner())
}

// buildRexAdapter builds the adapter on loc, over base. It wraps base so every
// command is re-checked when loc carries a Verify. base is a parameter so a
// test can watch what reaches the runner.
func buildRexAdapter(loc rexLocation, base exec.SensitiveRunner) (backend.Adapter, error) {
	run := base
	if loc.Verify != nil {
		run = verifyingRunner{inner: run, verify: loc.Verify}
	}
	// Assigned and returned explicitly rather than forwarded, for the reason
	// spelled out in newTmuxAdapter: a bare return converts a nil *Adapter
	// into a NON-nil backend.Adapter holding a nil pointer.
	a, err := rexadapter.New(run, loc.Path)
	if err != nil {
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
	case "hc":
		return backend.KindHc, nil
	case "rex":
		return backend.KindRex, nil
	default:
		// The supplied name is not echoed. It is not secret, but it is
		// command-line input to a message that gets printed to a terminal, and
		// every other refusal in this package is category-only for that reason.
		return backend.Kind(0), fmt.Errorf("%w: expected one of hc, rex", errUnknownBackend)
	}
}
