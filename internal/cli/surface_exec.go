package cli

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/forgive"
	"github.com/cameronsjo/forgectl/internal/surface"
)

// This file is forgectl's pre-start bootstrap classifier: the first thing the
// binary does, ahead of environment capture, legacy-boundary preparation,
// logger setup, and module construction.
//
// `forgectl surface` (forgectl#331) starts a harness inside a terminal manager
// by asking that manager to type a command. The command it types re-enters this
// same binary as `surface _exec …`, carrying the path to a private socket and a
// one-use rendezvous nonce; the actual invocation — harness path, argv,
// environment, prompt — is delivered over that socket afterwards, never through
// argv a manager can see.
//
// That re-entry must not go through normal startup. Startup reads config,
// prepares a migration boundary, builds a logger, constructs every module, and
// logs what it dispatched — so a bootstrap that fell through would do a large
// amount of work it does not need and write a socket path and a nonce into the
// log on the way. Claiming the invocation here means none of that is reachable.
//
// Phase 3 ships the classifier alone. The runtime behind it is a stub that
// refuses with a typed error, so a hand-crafted `surface _exec` is *claimed and
// refused* rather than either running or falling through.

const (
	surfaceVerb     = "surface"
	surfaceExecVerb = "_exec"

	// nonceHexLen is the encoded length of a 256-bit rendezvous nonce.
	//
	// Taken from the package that produces these nonces, for the same reason
	// maxSocketPathLen is taken from the package that produces socket paths:
	// two independent literals let the producer emit a bootstrap this
	// classifier refuses.
	nonceHexLen = surface.NonceHexLen

	// maxSocketPathLen bounds the socket path well under the platform's
	// sun_path limit (104 on Darwin, 108 on Linux), so an over-long path is
	// refused here with a clear category rather than at bind time.
	//
	// Taken from the package that *produces* these paths rather than declared
	// again here. Two independent literals would let the producer emit a
	// bootstrap this classifier refuses — a defect that would surface only on
	// whoever has the longest temp path.
	maxSocketPathLen = surface.MaxSocketPathLen
)

// Bootstrap refusals are deliberately category-only and carry no offending
// value. The classifier handles a socket path and a rendezvous nonce; an error
// that quoted the token it rejected would print the nonce to stderr, which is
// the leak this whole early seam exists to prevent.
// bootstrapProtocol is the only wire version this build speaks, rendered from
// the protocol's own constant rather than written again here. The outer process
// and the trampoline are always the same binary in the intended flow, so a
// mismatch means someone hand-built the invocation, or an upgrade replaced the
// binary between a manager being asked to type the command and it running —
// both cases must refuse rather than negotiate.
//
// It is a var only because strconv.Itoa is not a constant expression; deriving
// it is what keeps the classifier and the protocol from drifting apart on a
// version bump.
var bootstrapProtocol = strconv.Itoa(surface.ProtocolVersion)

var (
	errBootstrapMalformed = errors.New("forgectl: malformed surface bootstrap invocation")
	errBootstrapProtocol  = errors.New("forgectl: unsupported surface bootstrap protocol version")

	// errTrampolineNotImplemented is Phase 3's terminal state. It is a typed
	// error rather than a nil return so a valid bootstrap fails inspectably
	// instead of looking like a trampoline that ran and did nothing.
	errTrampolineNotImplemented = errors.New("forgectl: surface bootstrap runtime is not implemented in this build")
)

// bootstrapRequest is a validated bootstrap invocation. Both fields are opaque
// by construction: exec.SecretArg renders redacted through every formatting
// verb, slog, and JSON, and offers no accessor that reveals its payload —
// comparison goes through Equal. Nothing downstream can print them by accident.
type bootstrapRequest struct {
	socket exec.SecretArg
	nonce  exec.SecretArg
}

// String and LogValue redact at the STRUCT level, not only the field level.
// The fields already render redacted on their own, so this is belt and braces
// — but it is the cheap kind: it means a future field added to this struct is
// covered before anyone remembers to make it opaque, and it makes %s and %q
// legal verbs on the request rather than vet errors that invite a caller to
// reach for the fields instead.
func (bootstrapRequest) String() string { return "surface bootstrap request " + exec.Redacted }

func (r bootstrapRequest) LogValue() slog.Value { return slog.StringValue(r.String()) }

// trampolineRuntime is the seam Phase 4 (forgectl#331) fills: connect to the
// socket, complete the nonce handshake, receive the invocation, and exec the
// harness. It is an interface so the classifier can be tested without any of
// that existing.
type trampolineRuntime interface {
	Run(ctx context.Context, req bootstrapRequest) error
}

// notImplementedTrampoline is Phase 3's stub.
type notImplementedTrampoline struct{}

func (notImplementedTrampoline) Run(context.Context, bootstrapRequest) error {
	return errTrampolineNotImplemented
}

// productionTrampolineRuntime returns the runtime Execute hands the classifier.
func productionTrampolineRuntime() trampolineRuntime { return notImplementedTrampoline{} }

// trySurfaceExec claims and handles a surface bootstrap invocation. It reports
// handled=true for every bootstrap candidate — including malformed ones, which
// it refuses — and handled=false only for argv that is not a candidate at all,
// which then continues into normal startup untouched.
//
// The two-value shape matters: a malformed candidate returning handled=false
// would fall through to startup and get its socket and nonce logged, which is
// exactly the defect the ordering exists to prevent. Claimed-and-refused is the
// fail-closed answer.
func trySurfaceExec(ctx context.Context, argv []string, rt trampolineRuntime) (handled bool, err error) {
	if !isBootstrapCandidate(argv) {
		return false, nil
	}
	req, err := parseBootstrap(argv)
	if err != nil {
		return true, err
	}
	return true, rt.Run(ctx, req)
}

// isBootstrapCandidate reports whether argv is an attempt at a bootstrap,
// well-formed or not.
//
// The test is deliberately wider than the accepted form — any `_exec` token
// after a leading `surface` — so that a global-flag or misspelled variant
// (`--no-icons surface _exec …`, `Surface. _exec …`) is claimed and refused
// here rather than continuing into the pipeline with its nonce still in argv.
// That matters because every LATER stage accepts those forms: the extension
// rung skips leading inert flags and would hand an unregistered `surface` verb,
// nonce and all, to a PATH-resolved `forgectl-surface` binary; and argv
// normalization rewrites a forgiving spelling to the canonical module name
// once a `surface` module exists.
//
// So the leading token is located with firstNonFlag and compared after
// forgive.Normalize — the same two transformations the rest of the pipeline
// applies — rather than by indexing argv[0] raw. forgive is a pure string
// normalizer (it imports only strings), so this costs the classifier none of
// its independence from the module registry it runs ahead of.
//
// It is equally deliberately narrower than "argv contains _exec anywhere". The
// leading `surface` is what keeps the launch passthrough intact: `forgectl
// launch -p _exec` forwards an operator's prompt to claude byte-clean, and a
// classifier that claimed it would break an ordinary launch to defend a token
// that is only reserved under `surface`.
func isBootstrapCandidate(argv []string) bool {
	first, idx := firstNonFlag(argv)
	if first == "" || forgive.Normalize(first) != surfaceVerb {
		return false
	}
	for _, a := range argv[idx+1:] {
		if a == surfaceExecVerb {
			return true
		}
	}
	return false
}

// parseBootstrap accepts exactly:
//
//	surface _exec --protocol <version> --socket <absolute-path> --nonce <64-hex>
//
// in that order, with no aliases, no `--flag=value` forms, no abbreviations, no
// duplicates, and no extra tokens. The grammar is closed because the only
// legitimate producer of this argv is forgectl itself, building it from
// constants — so every degree of freedom a permissive parser would offer is
// one an attacker gets and the real caller never uses.
func parseBootstrap(argv []string) (bootstrapRequest, error) {
	const want = 8
	if len(argv) != want {
		return bootstrapRequest{}, errBootstrapMalformed
	}
	if argv[0] != surfaceVerb || argv[1] != surfaceExecVerb {
		return bootstrapRequest{}, errBootstrapMalformed
	}
	if argv[2] != "--protocol" || argv[4] != "--socket" || argv[6] != "--nonce" {
		return bootstrapRequest{}, errBootstrapMalformed
	}

	// Protocol is checked before the value fields so a version mismatch reports
	// itself as one, rather than as whatever the newer version's socket or
	// nonce format happens to fail.
	if argv[3] != bootstrapProtocol {
		return bootstrapRequest{}, errBootstrapProtocol
	}
	if !validSocketPath(argv[5]) || !validNonceEncoding(argv[7]) {
		return bootstrapRequest{}, errBootstrapMalformed
	}

	return bootstrapRequest{
		socket: exec.Secret(argv[5]),
		nonce:  exec.Secret(argv[7]),
	}, nil
}

// validSocketPath requires an absolute, bounded, single-line path. The
// trampoline dials this path; a relative one would resolve against whatever cwd
// the terminal manager happened to have, which is not a directory forgectl
// chose.
func validSocketPath(p string) bool {
	if p == "" || len(p) > maxSocketPathLen || !filepath.IsAbs(p) {
		return false
	}
	return !strings.ContainsAny(p, "\x00\n\r")
}

// validNonceEncoding requires exactly 64 lowercase hex characters. Lowercase
// specifically: the encoder is forgectl, it emits lowercase, and accepting both
// cases would mean two spellings of one nonce.
func validNonceEncoding(n string) bool {
	if len(n) != nonceHexLen {
		return false
	}
	for _, c := range n {
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f':
		default:
			return false
		}
	}
	return true
}
