package backend

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strconv"
	"unicode/utf8"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// maxDisplayNameLen bounds the human label a manager may show. It is not a
// security boundary — the label never enters a reference or an identity check
// — but it does reach a manager's UI and its command line, so it is bounded
// and single-line like everything else that leaves this process.
const maxDisplayNameLen = 128

// maxTargetPathLen bounds the target directory. It is generous relative to any
// real project path and exists so that a pathological value cannot reach a
// manager's command line unbounded.
const maxTargetPathLen = 4096

var (
	// ErrInvalidStartSpec reports a start request that cannot be handed to an
	// adapter.
	ErrInvalidStartSpec = errors.New("surface: invalid start request")
	// ErrBootstrapNotOpaque reports an attempt to build a bootstrap command
	// from a plain argument.
	ErrBootstrapNotOpaque = errors.New("surface: bootstrap command must be an opaque argument")
	// ErrBootstrapEmpty reports a bootstrap command with no payload.
	ErrBootstrapEmpty = errors.New("surface: bootstrap command is empty")
	// ErrAdapterCapabilities reports an adapter that cannot both close and
	// probe what it creates.
	ErrAdapterCapabilities = errors.New("surface: adapter cannot clean up what it creates")
)

// BootstrapCommand is the one command a manager is asked to type, held so that
// no adapter can read it.
//
// What it contains — the absolute forgectl self-path, the hidden re-entry
// verb, the private socket path, the protocol version, and the rendezvous
// nonce — is exactly the set of things that must reach the terminal manager
// and must not reach anything else. Wrapping it in an opaque argument rather
// than a string is what makes "the adapter does not log the nonce" a property
// of the type instead of a property of every adapter author's care.
//
// The only thing an adapter can do with it is put it into a sensitive
// command's arguments. There is no accessor that returns a string, and every
// formatting verb, slog, and JSON path renders it redacted.
type BootstrapCommand struct{ arg exec.Arg }

// NewBootstrapCommand wraps a quoted bootstrap command line.
//
// It refuses a non-opaque argument. A plain argument renders its own value
// under every verb, so accepting one here would produce a BootstrapCommand
// that looks opaque, satisfies this type's contract at the call site, and
// prints the nonce the first time an adapter logs its command.
//
// It also refuses an empty payload. exec.Arg reports Secret from its kind
// alone, so exec.Opaque("") is opaque and would pass every gate here — leaving
// a workspace the manager is asked to type nothing into, with Set reporting
// that a bootstrap was supplied. The emptiness test goes through Equal so the
// check itself does not reveal the value.
func NewBootstrapCommand(arg exec.Arg) (BootstrapCommand, error) {
	if !arg.Secret() {
		return BootstrapCommand{}, ErrBootstrapNotOpaque
	}
	if arg.Equal(exec.Opaque("")) {
		return BootstrapCommand{}, ErrBootstrapEmpty
	}
	return BootstrapCommand{arg: arg}, nil
}

// SensitiveArg forwards the still-opaque value into a sensitive command. This
// is the type's entire forwarding surface: the value goes on, and nothing
// comes back out.
func (b BootstrapCommand) SensitiveArg() exec.Arg { return b.arg }

// Set reports whether a bootstrap was supplied.
func (b BootstrapCommand) Set() bool { return b.arg.Secret() }

func (BootstrapCommand) String() string   { return exec.Redacted }
func (BootstrapCommand) GoString() string { return exec.Redacted }

func (BootstrapCommand) Format(f fmt.State, verb rune) {
	if verb == 'q' {
		_, _ = io.WriteString(f, strconv.Quote(exec.Redacted))
		return
	}
	_, _ = io.WriteString(f, exec.Redacted)
}

func (BootstrapCommand) LogValue() slog.Value { return slog.StringValue(exec.Redacted) }

func (BootstrapCommand) MarshalJSON() ([]byte, error) {
	return []byte(strconv.Quote(exec.Redacted)), nil
}

func (BootstrapCommand) MarshalText() ([]byte, error) { return []byte(exec.Redacted), nil }

// StartSpec is everything an adapter is given, and it is deliberately a short
// list.
//
// The target directory and display name are here because a manager creating a
// workspace necessarily learns them. The ownership tag is here because the
// adapter needs it before mutating, to name the object and to reconcile
// against later. The invocation is not here, and cannot be: no field of this
// struct can hold one, which is what makes the privacy boundary a compile-time
// fact rather than a review convention.
//
// Every field is unexported, and the two string-valued ones are held as opaque
// arguments rather than strings. That is not ceremony. A manager necessarily
// sees the directory and the label; a *log* must not, because it outlives the
// workspace and travels further than the manager's UI — and a plain string
// field defeats every rendering method this type could define, because fmt
// reaches a value in an unexported field by reflection and prints the field
// rather than consulting Format or String. Holding them the way exec.Arg does,
// behind a closure, is what makes the redaction hold at any nesting depth.
//
// The consequence is that an adapter forwards them into a sensitive command
// without reading them, which is the same contract the bootstrap already has.
type StartSpec struct {
	cwd       exec.Arg
	name      exec.Arg
	tag       RecoveryTag
	bootstrap BootstrapCommand
}

// NewStartSpec validates a start request and seals its two path-and-label
// values.
//
// Validation happens here, on the plain strings, because it is the last point
// at which anything may look at them. The directory gets the same text checks
// as the display name: both reach a manager's command line and its UI, and the
// directory is the more exposed of the two, since a checked-out directory name
// is chosen by whoever wrote the repository rather than by the operator.
func NewStartSpec(cwd, name string, tag RecoveryTag, bootstrap BootstrapCommand) (StartSpec, error) {
	if cwd == "" || !filepath.IsAbs(cwd) {
		return StartSpec{}, fmt.Errorf("%w: target directory must be absolute", ErrInvalidStartSpec)
	}
	if cwd != filepath.Clean(cwd) {
		return StartSpec{}, fmt.Errorf("%w: target directory is not canonical", ErrInvalidStartSpec)
	}
	if err := safeText(cwd, maxTargetPathLen, "target directory"); err != nil {
		return StartSpec{}, err
	}
	if name == "" {
		return StartSpec{}, fmt.Errorf("%w: display name is empty", ErrInvalidStartSpec)
	}
	if err := safeText(name, maxDisplayNameLen, "display name"); err != nil {
		return StartSpec{}, err
	}
	if !tag.Valid() {
		return StartSpec{}, fmt.Errorf("%w: %w", ErrInvalidStartSpec, ErrInvalidRecoveryTag)
	}
	if !bootstrap.Set() {
		return StartSpec{}, fmt.Errorf("%w: no bootstrap command", ErrInvalidStartSpec)
	}
	return StartSpec{
		cwd:       exec.Opaque(cwd),
		name:      exec.Opaque(name),
		tag:       tag,
		bootstrap: bootstrap,
	}, nil
}

// CWD is the canonical absolute directory the workspace opens in, opaque so it
// can be forwarded into a sensitive command without being rendered.
func (s StartSpec) CWD() exec.Arg { return s.cwd }

// Name is the human label a manager may display. It never enters a reference,
// a log, or an identity check.
func (s StartSpec) Name() exec.Arg { return s.name }

// Tag is the ownership tag. The service draws it so that the tag in the native
// object name and the tag in the resulting reference are the same value by
// construction rather than by two independent generations.
func (s StartSpec) Tag() RecoveryTag { return s.tag }

// Bootstrap is the opaque command the manager will be asked to type.
func (s StartSpec) Bootstrap() BootstrapCommand { return s.bootstrap }

// Validate reports whether this spec came through NewStartSpec intact. The
// zero value does not, so an adapter handed one built by struct literal — which
// the unexported fields prevent outside this package, and which a future
// in-package caller could still write — refuses before mutating.
func (s StartSpec) Validate() error {
	if !s.cwd.Secret() || !s.name.Secret() {
		return fmt.Errorf("%w: spec was not built by NewStartSpec", ErrInvalidStartSpec)
	}
	if !s.tag.Valid() {
		return fmt.Errorf("%w: %w", ErrInvalidStartSpec, ErrInvalidRecoveryTag)
	}
	if !s.bootstrap.Set() {
		return fmt.Errorf("%w: no bootstrap command", ErrInvalidStartSpec)
	}
	return nil
}

// safeText bounds a string that will reach a manager's command line and UI:
// length-capped, valid UTF-8, and free of the control sequences that would let
// it rewrite a terminal it was only supposed to appear in.
func safeText(s string, limit int, what string) error {
	if len(s) > limit {
		return fmt.Errorf("%w: %s exceeds %d bytes", ErrInvalidStartSpec, what, limit)
	}
	if !utf8.ValidString(s) {
		return fmt.Errorf("%w: %s is not valid UTF-8", ErrInvalidStartSpec, what)
	}
	for _, r := range s {
		if termsafe.IsUnsafeTerminalRune(r) {
			return fmt.Errorf("%w: %s contains a control character", ErrInvalidStartSpec, what)
		}
	}
	return nil
}

// OwnershipName is the native object name the adapter must create. It is
// derived here rather than composed by each adapter so that reconciliation and
// reference validation are matching against one spelling.
func (s StartSpec) OwnershipName() string { return s.tag.OwnershipName() }

// The directory and the label are the two things a manager sees anyway, and
// they are also the two a log must not carry: a log outlives the workspace and
// travels further than the manager's own UI. So this type renders as its
// ownership tag under every route, not only under slog — a LogValue alone
// would leave `fmt.Errorf("start %v: %w", spec, err)` and any %+v in a debug
// line printing both fields in the clear, which is the shape an adapter author
// reaches for without thinking about it.
func (s StartSpec) String() string { return "surface start spec " + s.tag.String() }

func (s StartSpec) GoString() string { return s.String() }

// Format is not what stops the leak — String and GoString already cover %v,
// %s, %q, and %#v, and the fields are opaque under reflection regardless. It
// is here so the exotic verbs fmt does *not* route through Stringer (%d, %x on
// a struct) render the same safe line instead of a mangled fallback.
func (s StartSpec) Format(f fmt.State, verb rune) {
	if verb == 'q' {
		_, _ = io.WriteString(f, strconv.Quote(s.String()))
		return
	}
	_, _ = io.WriteString(f, s.String())
}

func (s StartSpec) LogValue() slog.Value {
	return slog.GroupValue(slog.String("tag", s.tag.String()))
}

func (s StartSpec) MarshalJSON() ([]byte, error) {
	return []byte(strconv.Quote(s.String())), nil
}

func (s StartSpec) MarshalText() ([]byte, error) { return []byte(s.String()), nil }

// Adapter starts one workspace in one terminal manager.
//
// Start returns a StartResult rather than (Ref, error) because the interesting
// case is neither success nor failure: it is a daemon that may have created
// something and did not say so. An error return cannot carry that, and a
// caller reading one has to guess — which is how the guess becomes "close
// whatever matches the name".
type Adapter interface {
	Kind() Kind
	Start(ctx context.Context, spec StartSpec) StartResult
}

// Closer removes exactly one object named by a reference, after re-resolving
// the reference's server source and confirming the incarnation still matches.
//
// An implementation MUST also confirm ownership by reading the object back and
// refusing the close unless its native name or title equals
// ref.OwnershipName(), returning CloseIdentityMismatch when it does not.
//
// That obligation is stated here rather than enforced by Ref because only tmux
// can enforce it in the type: a tmux session name is chosen by forgectl and
// bound to the reference's tag at construction, but a cmux workspace UUID and
// a herdr workspace ID are server-assigned and cannot carry the tag. So for
// two of the three backends, a create response that named the wrong object —
// a raced reply, a truncated parse landing on a neighbouring record, a stale
// ID echoed after a restart — yields a fully valid Ref pointing at somebody
// else's workspace, and this read-back is what catches it.
type Closer interface {
	Close(ctx context.Context, ref Ref) CloseResult
}

// Prober reports whether exactly one object named by a reference still exists,
// under the same re-resolution, incarnation, and ownership-name rules as
// Close. A probe that finds an object whose name is not ref.OwnershipName()
// reports ProbeIdentityMismatch, not ProbePresent.
type Prober interface {
	Probe(ctx context.Context, ref Ref) ProbeResult
}

// Capabilities is an adapter that can clean up after itself. Every v1 backend
// implements it.
type Capabilities interface {
	Adapter
	Closer
	Prober
}

// RequireCapabilities checks an adapter before any listener or manager
// mutation.
//
// The check is up front rather than at the point of use because the point of
// use is rollback, and discovering there that an adapter cannot close what it
// created means discovering it while holding something that needs closing.
// Optional future capabilities may be absent; these two may not, for a launch
// whose failure path depends on them.
//
// The nil check catches a nil interface, not a typed nil pointer — a
// (*someAdapter)(nil) has a type and reaches its own Kind method. Nothing
// short of reflection distinguishes those, and a wiring bug that constructs a
// typed nil is not what this gate is for.
func RequireCapabilities(a Adapter) (Capabilities, error) {
	if a == nil {
		return nil, fmt.Errorf("%w: no adapter", ErrAdapterCapabilities)
	}
	if !a.Kind().Valid() {
		return nil, fmt.Errorf("%w: adapter reports no backend kind", ErrAdapterCapabilities)
	}
	full, ok := a.(Capabilities)
	if !ok {
		return nil, fmt.Errorf("%w: %s adapter does not implement both close and probe", ErrAdapterCapabilities, a.Kind())
	}
	return full, nil
}
