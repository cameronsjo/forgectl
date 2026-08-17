// Package privdir pins a private directory by descriptor, with every check its
// later use depends on already performed.
//
// The problem it solves is time-of-check/time-of-use. Code that verifies a
// directory by path and then opens files under it by rebuilding path strings
// has verified nothing: between the check and each later open, another process
// can swap the directory for a symlink and redirect every one of them. The fix
// is to stop consulting the name — pin a descriptor once, prove it is a
// directory this user owns at the intended mode, and do all later work with
// openat against that descriptor.
//
// This is lifted from internal/launch, where it guarded the usage-statistics
// namespace (forgectl#285). It moved here when a second consumer appeared —
// the surface coordinator's private socket directory (forgectl#331) — because
// hand-rolling a third copy of a filesystem race guard is how the third copy
// ends up subtly weaker than the first two.
package privdir

import (
	"errors"
	"io/fs"
)

var (
	// ErrAbsent reports that the directory does not exist. It is distinct from
	// a refusal on purpose: a caller that treats "never created" as an empty
	// store must not treat "exists and was refused" the same way.
	ErrAbsent = errors.New("privdir: directory is absent")

	// ErrUnsafe reports a directory that exists and that forgectl declined to
	// use. Its message is terminal-safe, because callers print it.
	ErrUnsafe = errors.New("privdir: directory is unsafe")

	// ErrUnsupported reports a platform without the descriptor-relative
	// primitives this package is built on.
	ErrUnsupported = errors.New("privdir: not supported on this platform")
)

// Spec describes the directory to pin.
//
// The shape is deliberately base-plus-single-leaf rather than an arbitrary
// path. Everything at and below the leaf is verified descriptor-relative, and
// a single component is what makes that checkable — a multi-segment leaf would
// reintroduce the intermediate names this package exists to stop consulting.
type Spec struct {
	// Base is the parent directory. It is opened and pinned, but its own
	// ancestors are not walked no-follow: on macOS the standard temp and state
	// roots sit under symlinked ancestors (/var → /private/var), and a strict
	// walk refuses paths operators legitimately use. Everything below Base —
	// where names are predictable and therefore worth attacking — is fully
	// verified.
	Base string

	// Leaf is a single path component under Base. This is the directory whose
	// descriptor is returned.
	Leaf string

	// Mode is the permission bits the leaf must end up with, conventionally
	// 0o700. A leaf found broader than this is narrowed, but only after its
	// identity and ownership are proven, so a chmod can never land on an
	// object that was never ours.
	Mode fs.FileMode

	// AncestorMode is the permission for directories created above Base when
	// Create is set. It is deliberately separate from Mode: a base several
	// levels below a shared root should not make that root private forever —
	// enabling a feature should not be what decides ~/.local is 0700.
	AncestorMode fs.FileMode

	// Create makes Pin materialise Base and Leaf when absent. Without it, an
	// absent directory reports ErrAbsent and nothing is written.
	Create bool
}
