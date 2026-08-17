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

var (
	// ErrInvalidStartSpec reports a start request that cannot be handed to an
	// adapter.
	ErrInvalidStartSpec = errors.New("surface: invalid start request")
	// ErrBootstrapNotOpaque reports an attempt to build a bootstrap command
	// from a plain argument.
	ErrBootstrapNotOpaque = errors.New("surface: bootstrap command must be an opaque argument")
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
func NewBootstrapCommand(arg exec.Arg) (BootstrapCommand, error) {
	if !arg.Secret() {
		return BootstrapCommand{}, ErrBootstrapNotOpaque
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
type StartSpec struct {
	// CWD is the canonical absolute directory the workspace opens in.
	CWD string
	// Name is a human label a manager may display. It never enters a
	// reference, a log, or an identity check.
	Name string
	// Tag is the ownership tag. The service draws it so that the tag in the
	// native object name and the tag in the resulting reference are the same
	// value by construction rather than by two independent generations.
	Tag RecoveryTag
	// Bootstrap is the opaque command the manager will be asked to type.
	Bootstrap BootstrapCommand
}

// Validate checks the spec before any adapter sees it.
func (s StartSpec) Validate() error {
	if s.CWD == "" || !filepath.IsAbs(s.CWD) {
		return fmt.Errorf("%w: target directory must be absolute", ErrInvalidStartSpec)
	}
	if s.CWD != filepath.Clean(s.CWD) {
		return fmt.Errorf("%w: target directory is not canonical", ErrInvalidStartSpec)
	}
	if !s.Tag.Valid() {
		return fmt.Errorf("%w: %w", ErrInvalidStartSpec, ErrInvalidRecoveryTag)
	}
	if !s.Bootstrap.Set() {
		return fmt.Errorf("%w: no bootstrap command", ErrInvalidStartSpec)
	}
	if s.Name == "" {
		return fmt.Errorf("%w: display name is empty", ErrInvalidStartSpec)
	}
	if len(s.Name) > maxDisplayNameLen {
		return fmt.Errorf("%w: display name exceeds %d bytes", ErrInvalidStartSpec, maxDisplayNameLen)
	}
	if !utf8.ValidString(s.Name) {
		return fmt.Errorf("%w: display name is not valid UTF-8", ErrInvalidStartSpec)
	}
	for _, r := range s.Name {
		if termsafe.IsUnsafeTerminalRune(r) {
			return fmt.Errorf("%w: display name contains a control character", ErrInvalidStartSpec)
		}
	}
	return nil
}

// OwnershipName is the native object name the adapter must create. It is
// derived here rather than composed by each adapter so that reconciliation and
// reference validation are matching against one spelling.
func (s StartSpec) OwnershipName() string { return s.Tag.OwnershipName() }

func (s StartSpec) LogValue() slog.Value {
	// The directory and the label are the two things a manager sees anyway,
	// and they are also the two things a log must not carry: a log outlives
	// the workspace and travels further than the manager's own UI.
	return slog.GroupValue(slog.String("tag", s.Tag.String()))
}

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
type Closer interface {
	Close(ctx context.Context, ref Ref) CloseResult
}

// Prober reports whether exactly one object named by a reference still exists,
// under the same re-resolution and incarnation rules as Close.
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
