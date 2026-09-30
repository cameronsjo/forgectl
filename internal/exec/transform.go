package exec

import "github.com/cameronsjo/forgectl/internal/tmux/tmuxesc"

// Transform is a re-spelling the seam applies to an opaque argument on a
// caller's behalf, for an adapter whose backend reads a sealed value in its
// own syntax: tmux format-expands a -c directory, so the tmux adapter must
// escape the sealed cwd (forgectl#839).
//
// The set is CLOSED, and that is the whole design. The function is an
// unexported field, and every Transform is minted by a constructor in this
// file from a fixed, pure escape. A caller outside this package can pick a
// Transform but cannot write one, so no caller code ever receives a payload:
// the promise that only buildCmd lets a payload leave its wrapper still
// holds. An open `func(string) string` parameter would break it, because any
// importer could pass a closure that captures the nonce or the socket path.
// TestNoCallerCodeReceivesAnOpaquePayload keeps it closed, and keeps MapOpaque
// to its sanctioned callers.
//
// Adding a Transform means adding a constructor here over a pure function
// that returns nothing but its result, and adding its caller to that test's
// allowlist.
type Transform struct {
	apply func(string) string
}

// TmuxDirOperand escapes a directory for a tmux -c operand: format expansion
// and the trailing-';' command split (tmuxesc.DirOperand). It is a function
// rather than a variable so no importer can reassign it.
func TmuxDirOperand() Transform { return Transform{apply: tmuxesc.DirOperand} }

// MapOpaque returns a new opaque argument holding t applied to a's payload.
//
// t runs ONCE, here, and the result is closed over as an immutable string, so
// the new argument's reveal stays pure and repeatable (see SecretArg on why a
// reveal must not call out).
//
// Anything but an opaque argument (a fixed constant, the separator, the zero
// value), or the zero Transform, maps to the zero Arg, which validate refuses
// as never constructed. Re-spelling a constant is a defect, and it fails
// loudly rather than passing the constant through unchanged.
func MapOpaque(a Arg, t Transform) Arg {
	if a.kind != argOpaque || a.reveal == nil || t.apply == nil {
		return Arg{}
	}
	return Opaque(t.apply(a.reveal()))
}
