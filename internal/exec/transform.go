package exec

import "github.com/cameronsjo/forgectl/internal/exec/internal/sealed"

// Transform is a re-spelling the seam applies to an opaque argument on a
// caller's behalf, for an adapter whose backend reads a sealed value in its
// own syntax: tmux format-expands a -c directory, so the tmux adapter must
// escape the sealed cwd (forgectl#839).
//
// The set is CLOSED, and since forgectl#854 the compiler closes it. The
// function lives in sealed.Transform's unexported field, so only a
// constructor in internal/exec/internal/sealed can mint one, and every
// constructor there names a fixed, pure escape from the leaf package tmuxesc.
// Neither this package nor a caller can write a Transform over its own
// function, so no caller code ever receives a payload. An open
// `func(string) string` parameter would break that, because any importer
// could pass a closure that captures the nonce or the socket path.
//
// The guard tests are the backstop. TestTransformIsMintedOnlyInTransformGo
// keeps every exec-side Transform declared in this file,
// TestNoCallerCodeReceivesAnOpaquePayload keeps MapOpaque to its sanctioned
// callers, and TestExportedAPI fails on any change to the pinned API surface
// of this package or of sealed (such as a new func-typed parameter or a new
// file) until it is reviewed.
//
// Adding a Transform means adding a constructor over a pure function to
// sealed, wrapping it here, and adding its caller to transformCallers in the
// guard test.
type Transform struct {
	t sealed.Transform
}

// TmuxDirOperand escapes a directory for a tmux -c operand: format expansion
// and the trailing-';' command split (tmuxesc.DirOperand). It is a function
// rather than a variable so no importer can reassign it.
func TmuxDirOperand() Transform { return Transform{t: sealed.TmuxDirOperand()} }

// MapOpaque returns a new opaque argument holding t applied to a's payload.
//
// t runs ONCE, inside sealed, and the result is sealed as an immutable
// string, so the new argument's reveal stays pure and repeatable (see
// SecretArg on why a reveal must not call out).
//
// Anything but an opaque argument (a fixed constant, the separator, the zero
// value), or the zero Transform, maps to the zero Arg, which validate refuses
// as never constructed. Re-spelling a constant is a defect, and it fails
// loudly rather than passing the constant through unchanged.
func MapOpaque(a Arg, t Transform) Arg {
	if a.kind != argOpaque {
		return Arg{}
	}
	v := a.v.Map(t.t)
	if !v.Set() {
		return Arg{}
	}
	return Arg{v: v, kind: argOpaque}
}
